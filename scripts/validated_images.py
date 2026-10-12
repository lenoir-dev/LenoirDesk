#!/usr/bin/env python3
"""Record/check exact local Compose image IDs for an OIDC smoke without rebuild."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
DEFAULT = ROOT / "tmp/validated-images.json"


def output(*args: str) -> str:
    return subprocess.check_output(args, cwd=ROOT, stderr=subprocess.DEVNULL).decode().strip()


def source_hash() -> str:
    tracked = output("git", "ls-files", "--cached", "--", "Dockerfile", "go.mod", "go.sum",
                     "cmd", "internal", "migrations")
    digest = hashlib.sha256()
    for name in sorted(tracked.splitlines()):
        path = ROOT / name
        if not path.is_file() or path.is_symlink():
            raise RuntimeError("image source path unavailable")
        digest.update(name.encode() + b"\0")
        digest.update(path.read_bytes())
    return digest.hexdigest()


def images() -> dict[str, str]:
    project = json.loads(output("docker", "compose", "config", "--format", "json"))["name"]
    return {service: output("docker", "image", "inspect", "--format", "{{.Id}}",
                            project + "-" + service + ":latest") for service in ("api", "worker", "migrate")}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("record", "check"))
    parser.add_argument("--manifest", type=Path, default=DEFAULT)
    args = parser.parse_args()
    manifest = args.manifest.resolve()
    if not (manifest == ROOT / "tmp" or ROOT / "tmp" in manifest.parents):
        parser.error("manifest must be in ignored tmp directory")
    current = {"source_sha256": source_hash(), "images": images()}
    if args.mode == "record":
        # Compose may assign service-specific image metadata even though each
        # service uses the same Dockerfile. Pin each exact image ID instead of
        # assuming the IDs are identical.
        manifest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        manifest.write_text(json.dumps(current, indent=2) + "\n")
        manifest.chmod(0o600)
        print("validated local image manifest recorded")
    else:
        if not manifest.is_file() or json.loads(manifest.read_text()) != current:
            raise RuntimeError("local image or source differs from validated manifest; rebuild required")
        print("validated image IDs and source matched; no rebuild required")


if __name__ == "__main__":
    main()
