#!/usr/bin/env python3
"""Build and verify a private, unapproved candidate from a clean Git checkout."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import tempfile

from safe_publish import ROOT, REQUIRED_NOTICES, categories, check_image, check_image_archive

VERSION = "0.1.0-rc.2-review"
OUTPUT = ROOT / "tmp" / "private-candidate" / ("relaydesk-" + VERSION + ".tar")


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def run(args: list[str], **kwargs) -> str:
    result = subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs)
    return result.stdout.decode().strip()


def list_files(folder: Path) -> list[Path]:
    files = sorted(p for p in folder.rglob("*") if p.is_file())
    if any(p.is_symlink() for p in folder.rglob("*")):
        raise RuntimeError("candidate contains a symlink")
    return files


def scan_file(path: Path) -> list[str]:
    found = set()
    tail = b""
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            data = tail + chunk
            found.update(categories(data))
            tail = data[-8192:]
    return sorted(found)


def verify(archive: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="relaydesk-candidate-verify-") as temp:
        root = Path(temp)
        with tarfile.open(archive) as tar:
            for member in tar:
                path = Path(member.name)
                if path.is_absolute() or ".." in path.parts or not member.isfile() or member.size > 1024 * 1024 * 1024:
                    raise RuntimeError("unsafe candidate archive member")
                target = root / path
                target.parent.mkdir(parents=True, exist_ok=True)
                source = tar.extractfile(member)
                if source is None:
                    raise RuntimeError("candidate member unavailable")
                with target.open("xb") as output:
                    shutil.copyfileobj(source, output)
        checksums = root / "CHECKSUMS.sha256"
        expected = {}
        for line in checksums.read_text().splitlines():
            checksum, name = line.split("  ", 1)
            if name in expected or not (root / name).is_file() or not name.startswith(("source/", "bin/", "images/", "PROVENANCE", "RELEASE")):
                raise RuntimeError("invalid candidate checksum manifest")
            expected[name] = checksum
        actual = {str(p.relative_to(root)): digest(p) for p in list_files(root) if p != checksums}
        if expected != actual:
            raise RuntimeError("candidate checksum mismatch")
        image = root / "images" / "relaydesk-api.tar"
        findings = check_image_archive(image)
        for path in list_files(root):
            if path == image:
                continue  # exact saved image archive was inspected layer by layer
            findings.extend((str(path.relative_to(root)), item) for item in scan_file(path))
        if findings:
            raise RuntimeError("candidate security scan blocked; categories only: " + ",".join(sorted({str(f[-1]) for f in findings})))
        metadata = json.loads((root / "PROVENANCE.json").read_text())
        if metadata["version"] != VERSION or metadata["status"] != "review_only":
            raise RuntimeError("candidate provenance mismatch")
        for name in REQUIRED_NOTICES:
            source_notice = root / "source" / name
            binary_notice = root / "bin/licenses" / Path(name).name
            if not source_notice.is_file() or not binary_notice.is_file() or source_notice.read_bytes() != binary_notice.read_bytes():
                raise RuntimeError("candidate notice missing or changed: " + Path(name).name)
        return {"sha256": digest(archive), "members": len(actual) + 1,
                "image_id": metadata["image_id"], "commit": metadata["commit"]}


def create(archive: Path) -> dict:
    if run(["git", "status", "--porcelain"], cwd=ROOT):
        raise RuntimeError("candidate requires a clean committed checkout")
    if archive.exists():
        raise RuntimeError("candidate path already exists")
    archive.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(archive.parent, 0o700)
    commit = run(["git", "rev-parse", "HEAD"], cwd=ROOT)
    commit_time = int(run(["git", "show", "-s", "--format=%ct", "HEAD"], cwd=ROOT))
    goarch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(platform.machine().lower())
    if goarch is None:
        raise RuntimeError("unsupported local architecture")
    with tempfile.TemporaryDirectory(prefix="relaydesk-candidate-build-") as temp:
        staging = Path(temp) / "staging"
        staging.mkdir(mode=0o700)
        source = staging / "source"
        run(["python3", str(ROOT / "scripts/safe_publish.py"), "--export", str(source)], cwd=ROOT)
        (staging / "bin").mkdir()
        build_env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=goarch)
        for name in ("api", "worker", "witness"):
            run(["go", "build", "-trimpath", "-buildvcs=false", "-o", str(staging / "bin" / name), "./cmd/" + name],
                cwd=source, env=build_env)
        shutil.copytree(source / "docs/licenses", staging / "bin/licenses")
        tag = "relaydesk-api:" + VERSION
        run(["docker", "build", "--pull=false", "--build-arg", "TARGETARCH=" + goarch,
             "--label", "org.opencontainers.image.revision=" + commit,
             "--label", "org.opencontainers.image.version=" + VERSION,
             "-t", tag, str(source)], cwd=ROOT)
        image_id = run(["docker", "image", "inspect", "--format", "{{.Id}}", tag], cwd=ROOT)
        image_findings = check_image(tag)
        if image_findings:
            raise RuntimeError("candidate image security/notice scan blocked; categories only: " +
                               ",".join(sorted({item for _, item in image_findings})))
        (staging / "images").mkdir()
        image_archive = staging / "images" / "relaydesk-api.tar"
        run(["docker", "save", "-o", str(image_archive), tag], cwd=ROOT)
        os.chmod(image_archive, 0o600)
        metadata = {"version": VERSION, "status": "review_only", "commit": commit,
                    "commit_time_unix": commit_time, "go_arch": goarch,
                    "go_version": run(["go", "version"], cwd=ROOT), "image_id": image_id,
                    "image_tag": tag, "build_source": "safe_publish_allowlisted_export"}
        (staging / "PROVENANCE.json").write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n")
        (staging / "RELEASE-STATUS.txt").write_text(
            "PRIVATE LOCAL REVIEW CANDIDATE. See the Mission 08.1 evidence and verdict before use.\n"
            "Do not publish, deploy to production, or treat this archive as a release.\n"
            "Use source/docs/mission-081-evidence.md for test evidence and source/docs/recovery-witness.md for restore limits.\n")
        files = list_files(staging)
        checksums = staging / "CHECKSUMS.sha256"
        checksums.write_text("".join(digest(p) + "  " + str(p.relative_to(staging)) + "\n" for p in files))
        files.append(checksums)
        with tarfile.open(archive, "w") as tar:
            for path in sorted(files):
                info = tar.gettarinfo(str(path), arcname=str(path.relative_to(staging)))
                info.uid = info.gid = 0
                info.uname = info.gname = ""
                info.mtime = commit_time
                with path.open("rb") as stream:
                    tar.addfile(info, stream)
        os.chmod(archive, 0o600)
    return verify(archive)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("create", "verify"))
    parser.add_argument("--archive", type=Path, default=OUTPUT)
    args = parser.parse_args()
    result = create(args.archive) if args.command == "create" else verify(args.archive)
    print(json.dumps({"archive": str(args.archive), **result}, sort_keys=True))


if __name__ == "__main__":
    main()
