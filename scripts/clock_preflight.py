#!/usr/bin/env python3
"""Fail closed when local provider, database, and API clocks cannot agree."""

import argparse
import datetime as dt
import email.utils
import subprocess
import sys
import time
import urllib.error
import urllib.request


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL).strip()


def sample(name, *args):
    before = time.time()
    stamp = command(*args)
    after = time.time()
    measured = float(stamp)
    # Round-trip midpoint removes most command startup latency.
    skew = measured - (before + after) / 2
    print(f"{name}: {dt.datetime.fromtimestamp(measured, dt.timezone.utc).isoformat()} (skew {skew:+.1f}s)")
    return abs(skew)


def https_sample(name, url):
    before = time.time()
    request = urllib.request.Request(url, method="HEAD")
    try:
        response = urllib.request.urlopen(request, timeout=10)
    except urllib.error.HTTPError as exc:
        # An unauthenticated 401/404 still carries the provider's TLS-protected Date.
        response = exc
    after = time.time()
    stamp = response.headers.get("Date")
    if not stamp:
        raise ValueError(f"{name} did not return a Date header")
    parsed = email.utils.parsedate_to_datetime(stamp)
    if parsed.tzinfo is None:
        raise ValueError(f"{name} returned a Date without timezone")
    measured = parsed.timestamp()
    skew = measured - (before + after) / 2
    print(f"{name} HTTPS Date: {dt.datetime.fromtimestamp(measured, dt.timezone.utc).isoformat()} (skew {skew:+.1f}s)")
    return abs(skew)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--max-skew-seconds", type=float, default=30)
    parser.add_argument("--external", action="store_true", help="compare host time with provider HTTPS Date headers")
    args = parser.parse_args()
    print("Host UTC:", dt.datetime.now(dt.timezone.utc).isoformat())
    try:
        postgres = command("docker", "compose", "ps", "-q", "postgres")
        dex = command("docker", "compose", "ps", "-q", "dex")
        if not postgres or not dex:
            raise ValueError("PostgreSQL or Dex container is not running in this Compose project")
        samples = [
            ("Docker VM", "docker", "exec", postgres, "date", "+%s.%N"),
            ("PostgreSQL", "docker", "exec", postgres, "sh", "-c", "psql -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\" -Atc 'select extract(epoch from now())'"),
            ("Dex", "docker", "exec", dex, "date", "+%s.%N"),
        ]
        worst = max(sample(*entry) for entry in samples)
        if args.external:
            for name, url in (
                ("Stripe", "https://api.stripe.com/v1/account"),
                ("Slack", "https://slack.com/api/api.test"),
                ("HubSpot", "https://api.hubapi.com/"),
            ):
                worst = max(worst, https_sample(name, url))
    except (subprocess.CalledProcessError, ValueError, urllib.error.URLError) as exc:
        print(f"Clock preflight unavailable: {exc}", file=sys.stderr)
        return 2
    if worst > args.max_skew_seconds:
        print("Clock skew exceeds safe threshold. Synchronize the host clock, then restart/resynchronize Docker.", file=sys.stderr)
        return 1
    print("Clock preflight PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
