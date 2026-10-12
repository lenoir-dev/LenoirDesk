#!/usr/bin/env python3
"""Generate/check notices for the exact compiled Go graph and vendored UI assets."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
NOTICE = ROOT / "docs/licenses/THIRD_PARTY_NOTICES.txt"
MANIFEST = ROOT / "docs/licenses/compiled-go-modules.json"
GOOSE_NOTICE = ROOT / "docs/licenses/GOOSE_THIRD_PARTY_NOTICES.txt"
GOOSE_MANIFEST = ROOT / "docs/licenses/goose-modules.json"
ASSETS = {
    "internal/api/assets/htmx.min.js": "docs/licenses/htmx-0BSD.txt",
    "internal/api/assets/vanilla-calendar.js": "docs/licenses/vanilla-calendar-pro-MIT.txt",
    "internal/api/assets/vanilla-calendar.css": "docs/licenses/vanilla-calendar-pro-MIT.txt",
}


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def clean_notice(data: bytes) -> str:
    # Keep all license words and attribution; normalize upstream CRLF and
    # trailing indentation so generated notices pass the repository diff gate.
    return "\n".join(line.rstrip() for line in data.decode("utf-8", "replace").splitlines()).rstrip() + "\n"


def classify(data: bytes) -> str:
    text = data.decode("utf-8", "replace")
    if "Mozilla Public License Version 2.0" in text:
        return "MPL-2.0"
    if "MIT No Attribution" in text:
        return "MIT-0"
    if "Permission to use, copy, modify, and distribute this software for any" in text and "with or without fee" in text:
        return "ISC"
    if "Apache License" in text and "Version 2.0, January 2004" in text:
        return "Apache-2.0"
    if "Permission is hereby granted, free of charge" in text and "THE SOFTWARE IS PROVIDED" in text:
        return "MIT"
    if "Redistribution and use in source and binary forms" in text and "THIS SOFTWARE IS PROVIDED" in text:
        if "Neither the name" in text or "Neither the names" in text:
            return "BSD-3-Clause"
        return "BSD-2-Clause"
    raise RuntimeError("unclassified dependency license; review required")


def license_record(name: str, version: str, directory: Path) -> tuple[dict, str]:
    license_files = sorted(p for p in directory.iterdir() if p.is_file() and p.name.lower().startswith(("license", "licence", "copying")))
    notice_files = sorted(p for p in directory.iterdir() if p.is_file() and p.name.lower().startswith("notice"))
    if not license_files:
        raise RuntimeError(f"missing license for {name}@{version}")
    license_data = license_files[0].read_bytes()
    record = {"module": name, "version": version, "spdx": classify(license_data),
              "license_file": license_files[0].name, "license_sha256": sha(license_data),
              "notice_files": [{"file": p.name, "sha256": sha(p.read_bytes())} for p in notice_files]}
    chunk = f"\n===== {name}@{version} ({record['spdx']}) =====\n"
    chunk += clean_notice(license_data)
    for notice in notice_files:
        chunk += f"\n----- {notice.name} -----\n" + clean_notice(notice.read_bytes())
    return record, chunk


TARGETS = (("darwin", "amd64"), ("darwin", "arm64"),
           ("linux", "amd64"), ("linux", "arm64"))


def graph() -> list[dict]:
    # Use a fixed cross-platform union so the committed notice is verified
    # identically on a developer Mac and a Linux CI runner. The source ships
    # for all four targets; CGO is disabled in the Docker build.
    modules: dict[tuple[str, str], Path] = {}
    decoder = json.JSONDecoder()
    for goos, goarch in TARGETS:
        env = os.environ.copy()
        env.update({"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"})
        raw = subprocess.check_output(
            ["go", "list", "-deps", "-json", "./cmd/api", "./cmd/worker", "./cmd/witness"],
            cwd=ROOT, env=env, text=True)
        while raw.strip():
            raw = raw.lstrip()
            obj, consumed = decoder.raw_decode(raw)
            raw = raw[consumed:]
            mod = obj.get("Module")
            if mod and mod.get("Path") != "github.com/lenoir-dev/LenoirDesk":
                modules[(mod["Path"], mod.get("Version", ""))] = Path(mod["Dir"])
    results = []
    for (name, version), directory in sorted(modules.items()):
        record, chunk = license_record(name, version, directory)
        results.append((record, chunk))
    return results


def expected() -> tuple[bytes, bytes]:
    modules = graph()
    entries = []
    chunks = ["LenoirDesk third-party notices\n",
              "Go dependency union for cmd/api, cmd/worker and cmd/witness on Linux/macOS amd64/arm64 with CGO disabled. Build-only tools are not included in these binaries.\n"]
    for item, chunk in modules:
        entries.append(item)
        chunks.append(chunk)
    go_root = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    go_license = (go_root / "LICENSE").read_bytes()
    chunks.append("\n===== Go standard library/toolchain (BSD-3-Clause) =====\n")
    chunks.append(clean_notice(go_license))
    assets = []
    for asset, license_path in sorted(ASSETS.items()):
        asset_data = (ROOT / asset).read_bytes()
        license_data = (ROOT / license_path).read_bytes()
        if not asset_data or not license_data:
            raise RuntimeError(f"missing asset or notice: {asset}")
        assets.append({"asset": asset, "sha256": sha(asset_data), "license": license_path,
                       "license_sha256": sha(license_data)})
    manifest = {"schema": 2, "targets": [f"{goos}/{goarch}/cgo0" for goos, goarch in TARGETS], "compiled_modules": entries, "go_license_sha256": sha(go_license),
                "frontend_assets": assets}
    return ("".join(chunks).encode(), (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode())


def goose_expected(write: bool) -> bytes:
    inventory = json.loads(GOOSE_MANIFEST.read_text())
    if inventory["goose_version"] != "v3.28.0" or inventory["goose_module_count"] != len(inventory["modules"]):
        raise RuntimeError("Goose inventory/version mismatch")
    dockerfile = (ROOT / "Dockerfile").read_text()
    for arch, digest in inventory["binary_sha256"].items():
        if f"{arch}) checksum={digest}" not in dockerfile:
            raise RuntimeError("Goose binary checksum changed without inventory review")
    chunks = ["LenoirDesk Goose migration binary third-party notices\n",
              "Conservative inventory from go version -m on the pinned v3.28.0 binary. Some listed modules may be build-graph-only.\n"]
    seen = set()
    for item in inventory["modules"]:
        name, version = item["module"], item["version"]
        if (name, version) in seen:
            raise RuntimeError("duplicate Goose module")
        seen.add((name, version))
        download = json.loads(subprocess.check_output(["go", "mod", "download", "-json", name + "@" + version], text=True))
        record, chunk = license_record(name, version, Path(download["Dir"]))
        for key in ("spdx", "license_file", "license_sha256", "notice_files"):
            if key in item and item[key] != record[key]:
                raise RuntimeError(f"Goose dependency notice changed: {name}")
            item[key] = record[key]
        chunks.append(chunk)
    actual = (json.dumps(inventory, indent=2, sort_keys=True) + "\n").encode()
    if write:
        GOOSE_MANIFEST.write_bytes(actual)
    elif actual != GOOSE_MANIFEST.read_bytes():
        raise RuntimeError("Goose license inventory needs regeneration")
    return "".join(chunks).encode()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="store_true")
    args = parser.parse_args()
    notice, manifest = expected()
    if args.write:
        NOTICE.write_bytes(notice)
        MANIFEST.write_bytes(manifest)
    elif not NOTICE.is_file() or not MANIFEST.is_file() or NOTICE.read_bytes() != notice or MANIFEST.read_bytes() != manifest:
        raise SystemExit("BLOCKED: third-party notice inventory is absent or differs from compiled graph/assets")
    goose_notice = goose_expected(args.write)
    if args.write:
        GOOSE_NOTICE.write_bytes(goose_notice)
    elif not GOOSE_NOTICE.is_file() or GOOSE_NOTICE.read_bytes() != goose_notice:
        raise SystemExit("BLOCKED: Goose third-party notice inventory is absent or differs from pinned binary graph")
    print("PASS: compiled dependency and frontend notice inventory")


if __name__ == "__main__":
    main()
