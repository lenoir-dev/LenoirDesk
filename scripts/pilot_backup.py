#!/usr/bin/env python3
"""Create a private PostgreSQL custom-format pilot backup; never print its data."""

import argparse
import os
from pathlib import Path
import re
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-container", required=True)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{2,100}", args.source_container):
        parser.error("invalid container name")
    destination = args.output_dir.resolve()
    if destination == ROOT or ROOT in destination.parents:
        ignored = subprocess.run(["git", "check-ignore", "-q", str(destination)], cwd=ROOT,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if ignored.returncode != 0:
            parser.error("repository backup directory must be ignored by Git")
    destination.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(destination, 0o700)
    name = "relaydesk-" + time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + ".dump"
    path = destination / name
    fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    try:
        with os.fdopen(fd, "wb") as out:
            result = subprocess.run(["docker", "exec", args.source_container, "pg_dump", "-U", "postgres",
                                     "-Fc", "--no-owner", "-d", "relaydesk"],
                                    stdout=out, stderr=subprocess.DEVNULL, check=False)
        if result.returncode:
            raise RuntimeError("pg_dump failed; diagnostics withheld")
        with path.open("rb") as source:
            checked = subprocess.run(["docker", "exec", "-i", args.source_container, "pg_restore", "-l"],
                                     stdin=source, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                     check=False)
        if checked.returncode:
            raise RuntimeError("backup format verification failed")
        print("private backup created: " + str(path))
    except BaseException:
        path.unlink(missing_ok=True)
        raise


if __name__ == "__main__":
    main()
