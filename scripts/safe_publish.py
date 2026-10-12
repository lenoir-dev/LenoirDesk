#!/usr/bin/env python3
"""Local, fail-closed source publication preflight. Never prints matched content."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
TOKEN_PATTERNS = {
    "provider_key": re.compile(rb"(?:sk|rk)_(?:test|live)_[A-Za-z0-9_-]{24,}"),
    "webhook_secret": re.compile(rb"whsec_[A-Za-z0-9_-]{24,}"),
    "slack_token": re.compile(rb"xox[baprs]-[A-Za-z0-9-]{24,}"),
    "github_token": re.compile(rb"(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}"),
    "private_key": re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH |)PRIVATE KEY-----"),
    "app_secret_assignment": re.compile(rb"(?m)^RELAYDESK_(?:ENCRYPTION_KEY|OIDC_CLIENT_SECRET|HUBSPOT_CLIENT_SECRET|SLACK_CLIENT_SECRET|STAGE0_\w*(?:KEY|TOKEN|SECRET))=[^\s#<]{16,}$"),
}
EXAMPLE_FILES = {".env.example", ".env.stage0.settings.example", ".env.stage0.secrets.example"}
ROOT_FILES = {"README.md", "Makefile", "go.mod", "go.sum", "Dockerfile", "compose.yaml", "sqlc.yaml", ".dockerignore", ".gitignore", *EXAMPLE_FILES}
SOURCE_DIRS = {"cmd", "internal", "migrations", "scripts", "tests", "docs", "deploy", "db"}
FORBIDDEN_PARTS = {".git", "tmp", "bin", "__pycache__", "node_modules", "coverage", "reports", "stage0-private"}
FORBIDDEN_DOC_PREFIXES = ("docs/reviews/", "docs/stage-0-", "docs/mission-05")
FORBIDDEN_SUFFIXES = (".log", ".db", ".sqlite", ".dump", ".pem", ".key", ".p12", ".zip", ".tar", ".gz", ".pdf", ".png", ".jpg")
PUBLIC_SCREENSHOTS = {"docs/screenshots/overview.jpg", "docs/screenshots/automation.jpg", "docs/screenshots/runs.jpg", "docs/screenshots/reconciliation.jpg"}
MAX_SOURCE_BYTES = 8 * 1024 * 1024
REQUIRED_NOTICES = ("docs/licenses/vanilla-calendar-pro-MIT.txt", "docs/licenses/htmx-0BSD.txt", "docs/licenses/goose-MIT.txt",
                    "docs/licenses/THIRD_PARTY_NOTICES.txt", "docs/licenses/compiled-go-modules.json",
                    "docs/licenses/GOOSE_THIRD_PARTY_NOTICES.txt", "docs/licenses/goose-modules.json")


def git(*args: str) -> bytes:
    return subprocess.check_output(["git", *args], cwd=ROOT)


def eligible(path: str) -> bool:
    p = Path(path)
    if p.is_absolute() or ".." in p.parts or not p.parts or any(part in FORBIDDEN_PARTS for part in p.parts):
        return False
    if path.startswith(FORBIDDEN_DOC_PREFIXES) or (path.endswith(FORBIDDEN_SUFFIXES) and path not in PUBLIC_SCREENSHOTS):
        return False
    if p.name.startswith(".env") and p.name not in EXAMPLE_FILES:
        return False
    if path in PUBLIC_SCREENSHOTS:
        return True
    if path == "db/queries/health.sql":
        return True
    if path in {"deploy/dex/lenoirdesk.css", "deploy/dex/password.html", "tests/historical_picker.test.cjs"}:
        return True
    if path.startswith("internal/api/assets/") and p.suffix in {".css", ".svg"}:
        return True
    if path == ".github/workflows/ci.yml":
        return True
    if len(p.parts) == 1:
        return path in ROOT_FILES
    if p.parts[0] not in SOURCE_DIRS:
        return False
    if p.parts[0] == "docs":
        return p.suffix == ".md" or path == "docs/openapi.yaml" or (p.parts[:2] == ("docs", "licenses") and p.suffix in {".txt", ".json"})
    return p.suffix in {".go", ".sql", ".sh", ".py", ".yaml", ".yml", ".json", ".md", ".js", ".txt", ".mod", ".sum"}


def categories(data: bytes) -> list[str]:
    found = []
    for name, pattern in TOKEN_PATTERNS.items():
        matches = pattern.finditer(data)
        if name == "app_secret_assignment":
            matches = (m for m in matches if not any(marker in m.group().lower() for marker in
                       (b"placeholder", b"example", b"replace", b"change-me", b"demo-only", b"test-only")))
        if any(True for _ in matches):
            found.append(name)
    return found


def scan_file(path: Path) -> list[str]:
    if path.is_symlink() or not path.is_file() or path.stat().st_size > MAX_SOURCE_BYTES:
        return ["unsafe_file_type_or_size"]
    return categories(path.read_bytes())


def source_files() -> list[str]:
    raw = git("ls-files", "--cached", "--others", "--exclude-standard", "-z")
    return sorted(set(filter(None, raw.decode().split("\0"))))


def check_history() -> list[tuple[str, str]]:
    findings = []
    seen = set()
    for line in git("rev-list", "--objects", "--all").decode().splitlines():
        object_id = line.split(" ", 1)[0]
        if object_id in seen:
            continue
        seen.add(object_id)
        if subprocess.check_output(["git", "cat-file", "-t", object_id], cwd=ROOT).strip() != b"blob":
            continue
        size = int(subprocess.check_output(["git", "cat-file", "-s", object_id], cwd=ROOT))
        if size > MAX_SOURCE_BYTES:
            continue
        data = subprocess.check_output(["git", "cat-file", "blob", object_id], cwd=ROOT)
        for category in categories(data):
            findings.append((object_id[:12], category))
    return findings


def check_dockerignore() -> list[str]:
    text = (ROOT / ".dockerignore").read_text().splitlines()
    required = {".git", ".env", ".env.*", "tmp/"}
    return sorted(required - set(text))


def missing_notices(included: list[str]) -> list[str]:
    # The Go binary embeds all three vendor assets. Source and image deliveries
    # must therefore carry their exact notices and the compiled-module inventory.
    return [name for name in REQUIRED_NOTICES if name not in included]


def check_image_archive(archive: Path) -> list[str]:
    findings = []
    found_notices = set()

    def scan_stream(stream) -> list[str]:
        found = set()
        tail = b""
        while chunk := stream.read(1024 * 1024):
            data = tail + chunk
            found.update(categories(data))
            tail = data[-8192:]
        return sorted(found)

    with tarfile.open(archive) as outer:
        for member in outer:
            if not member.isfile():
                continue
            stream = outer.extractfile(member)
            if stream is None:
                continue
            # Docker may save OCI blobs instead of named layer.tar files.
            # Inspect every blob, including layers and binaries larger than
            # the source-file cap, without extracting paths to disk.
            if member.name.endswith("/layer.tar") or member.name.startswith("blobs/sha256/"):
                try:
                    with tarfile.open(fileobj=stream, mode="r|*") as layer:
                        for item in layer:
                            if not item.isfile():
                                continue
                            content = layer.extractfile(item)
                            if content is None:
                                continue
                            normalized = item.name.lstrip("./")
                            if normalized.startswith("app/licenses/"):
                                source = ROOT / "docs/licenses" / Path(normalized).name
                                if source.is_file():
                                    data = content.read()
                                    if data == source.read_bytes():
                                        found_notices.add(source.name)
                                    else:
                                        findings.append(("image layer", "notice_content_mismatch"))
                                    continue
                            for category in scan_stream(content):
                                findings.append(("image layer", category))
                except tarfile.ReadError:
                    stream.seek(0)
                    for category in scan_stream(stream):
                        findings.append(("image metadata", category))
            else:
                for category in scan_stream(stream):
                    findings.append(("image metadata", category))
    for name in REQUIRED_NOTICES:
        if Path(name).name not in found_notices:
            findings.append(("image layer", "missing_notice:" + Path(name).name))
    return findings


def check_image(image: str) -> list[str]:
    with tempfile.TemporaryDirectory(prefix="relaydesk-image-scan-") as temp:
        archive = Path(temp) / "image.tar"
        subprocess.run(["docker", "save", "-o", str(archive), image], check=True, stdout=subprocess.DEVNULL)
        findings = check_image_archive(archive)
        # docker create/cp inspects the final overlay without executing the API.
        container = subprocess.check_output(["docker", "create", "--entrypoint", "/bin/true", image], text=True).strip()
        try:
            target = Path(temp) / "final-licenses"
            target.mkdir()
            subprocess.run(["docker", "cp", container + ":/app/licenses/.", str(target)], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for name in REQUIRED_NOTICES:
                local = target / Path(name).name
                if not local.is_file() or local.read_bytes() != (ROOT / name).read_bytes():
                    findings.append(("final image", "missing_or_changed_notice:" + Path(name).name))
            goose = Path(temp) / "goose"
            subprocess.run(["docker", "cp", container + ":/app/goose", str(goose)], check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            inventory = json.loads((ROOT / "docs/licenses/goose-modules.json").read_text())
            build_info = subprocess.check_output(["go", "version", "-m", str(goose)], text=True)
            fields = [line.split() for line in build_info.splitlines()]
            arch = next((field[1].split("=", 1)[1] for field in fields if len(field) > 1 and field[0] == "build" and field[1].startswith("GOARCH=")), "")
            actual_modules = [{"module": field[1], "version": field[2]} for field in fields if len(field) > 2 and field[0] in {"mod", "dep"}]
            expected_modules = [{"module": item["module"], "version": item["version"]} for item in inventory["modules"]]
            if hashlib.sha256(goose.read_bytes()).hexdigest() != inventory["binary_sha256"].get(arch):
                findings.append(("final image", "goose_binary_checksum_mismatch"))
            if actual_modules != expected_modules:
                findings.append(("final image", "goose_dependency_inventory_mismatch"))
        finally:
            subprocess.run(["docker", "rm", "-f", container], check=True, stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL)
        return findings


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--export", type=Path, help="create a clean allowlisted directory (never uploads)")
    parser.add_argument("--image", help="also inspect all layers of a local Docker image")
    args = parser.parse_args()
    findings: list[tuple[str, str]] = []
    files = source_files()
    included = []
    excluded = []
    for name in files:
        if eligible(name):
            included.append(name)
            for category in scan_file(ROOT / name):
                findings.append((name, category))
        else:
            excluded.append(name)
            if Path(name).name.startswith(".env") and Path(name).name not in EXAMPLE_FILES:
                findings.append((name, "secret_file_visible_to_git"))
    for name in missing_notices(included):
        findings.append((name, "required_notice_not_exported"))
    for oid, category in check_history():
        findings.append((f"history:{oid}", category))
    for rule in check_dockerignore():
        findings.append((".dockerignore", "missing_rule:" + rule))
    for sample in (".env", ".env.stage0.secrets"):
        ignored = subprocess.run(["git", "check-ignore", "-q", sample], cwd=ROOT).returncode == 0
        if not ignored:
            findings.append((".gitignore", "missing_ignore_rule:" + sample))
    if args.image:
        findings += check_image(args.image)
    print(f"source candidates: {len(files)}; allowlisted: {len(included)}; excluded from export: {len(excluded)}")
    if findings:
        print(f"BLOCKED: {len(findings)} findings. Values are suppressed.")
        for place, category in findings:
            print(f"  {place}: {category}")
        return 1
    if args.export:
        dest = args.export.resolve()
        if dest == ROOT or ROOT in dest.parents:
            raise SystemExit("Export destination must be outside the repository")
        if dest.exists():
            raise SystemExit("Export destination must not exist")
        dest.mkdir(mode=0o700, parents=True)
        for name in included:
            target = dest / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, target)
        (dest / "SOURCE_MANIFEST.txt").write_text("\n".join(included) + "\n")
        print(f"clean export created: {dest} ({len(included)} files)")
    else:
        print("PASS: source/history/context preflight" + (" and local image layers" if args.image else "; image layers require --image"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
