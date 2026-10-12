#!/usr/bin/env python3
"""Evaluate immutable Mission 08.1 load artifacts without hiding unfinished work."""

import argparse
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
ARTIFACTS = ROOT / "tmp" / "mission081-artifacts"


def artifact(stem: str, profile: str) -> dict:
    data = json.loads((ARTIFACTS / f"{stem}-{profile}.json").read_text())
    if data["exit_code"] != 0 or data["profile"] != profile or data["result"] is None:
        raise RuntimeError(f"{stem}-{profile}: missing or failed benchmark")
    return data


def require(ok: bool, reason: str) -> None:
    if not ok:
        raise RuntimeError(reason)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", default="final-m081")
    parser.add_argument("--drain-label", default="drain-m081")
    parser.add_argument("--c-drain-label")
    args = parser.parse_args()
    runs = {p: artifact(args.label, p) for p in "ABC"}
    baseline = {p: artifact("baseline-v4", p) for p in "ABC"}
    require(len({d["harness_sha256"] for d in runs.values()}) == 1, "standard harnesses differ")
    require(len({d["production_diff_sha256"] for d in runs.values()}) == 1, "production patches differ")
    require(len({d["docker_cpu_memory_version"] for d in [*runs.values(), *baseline.values()]}) == 1,
            "Docker resource allocation differs")
    for profile, data in runs.items():
        r = data["result"]
        require(r["seed"] == 808 and r["tenants"] == 20 and r["requested_deliveries"] == 6000,
                f"{profile}: workload identity drift")
        require(r["unique_event_ids"] == r["durable_inbox"] == r["runs"] == 5941,
                f"{profile}: accepted event lost")
        require(r["http_status_counts"] == {"200": 59, "202": 5941},
                f"{profile}: unexpected ingress outcome")
        require(r["crm_missing_event_actions"] == 0 and r["db_deadlocks"] == 0,
                f"{profile}: missing CRM link or database deadlock")
        require(r["actual_slack_sends"] <= r["distinct_paid_effects"],
                f"{profile}: duplicate invoice notification")
        require(r["actual_contact_writes"] == r["contact_count"],
                f"{profile}: repeated Contact write to an unchanged synthetic Customer")
    a, b, c = (runs[p]["result"] for p in "ABC")
    require(a["crm_converged_60s_percent"] >= 99 and a["crm_unconverged_at_observation"] == 0,
            "A: healthy CRM latency target failed")
    require(a["succeeded_runs"] == 5941 and a["actual_contact_writes"] == a["distinct_customer_keys"] and
            a["actual_slack_sends"] == a["distinct_paid_effects"], "A: business effect mismatch")
    progress = b["per_tenant_crm_progress"]
    normal = [v for k, v in progress.items() if k not in ("0", "1")]
    require(sum(v["Accepted"] for v in normal) == sum(v["Within60"] for v in normal),
            "B: healthy tenants did not progress fairly")
    require(b["simulated_429"] > 0 and b["simulated_503"] > 0,
            "B: outage or quota fault injection missing")
    require(c["crm_converged_60s_percent"] >= 99,
            "C: coalesced healthy-window latency target failed")
    reads = sum(n for key, n in c["provider_http_requests"].items() if "/GET/" in key)
    require(c["business_actions"] <= 1000 and reads <= 5000 and
            c["actual_contact_writes"] == c["distinct_customer_keys"] == 20 and
            c["actual_slack_sends"] == c["distinct_paid_effects"] == 20,
            "C: aggregate work or external effect amplification")
    drain = artifact(args.drain_label, "B")
    require(drain["production_diff_sha256"] == runs["B"]["production_diff_sha256"],
            "extended B used different production code")
    d = drain["result"]
    require(drain["post_ingress_observation_limit_seconds"] >= 600 and
            d["unique_event_ids"] == 5941 and d["crm_unconverged_at_observation"] == 0 and
            d["unfinished_obligations"] == 0 and d["succeeded_runs"] == 5941,
            "extended B did not fully drain")
    require(d["per_tenant_crm_progress"]["1"].get("Unconverged", 0) == 0,
            "extended B outage tenant did not recover")
    require(d["actual_contact_writes"] == d["contact_count"] and
            d["actual_slack_sends"] == d["distinct_paid_effects"],
            "extended B repeated or missed a provider effect")
    if args.c_drain_label:
        c_drain = artifact(args.c_drain_label, "C")
        require(c_drain["production_diff_sha256"] == runs["C"]["production_diff_sha256"],
                "extended C used different production code")
        cd = c_drain["result"]
        require(c_drain["post_ingress_observation_limit_seconds"] >= 240 and
                cd["seed"] == 808 and cd["unique_event_ids"] == 5941 and
                cd["crm_missing_event_actions"] == 0 and cd["crm_unconverged_at_observation"] == 0 and
                cd["unfinished_obligations"] == 0 and cd["succeeded_runs"] == 5941 and
                cd["actual_contact_writes"] == cd["contact_count"] == 20 and
                cd["actual_slack_sends"] == cd["distinct_paid_effects"] == 20,
                "extended C did not fully drain without duplicate effects")
    else:
        require(c["crm_unconverged_at_observation"] == 0 and c["succeeded_runs"] == 5941,
                "C: coalesced runs did not converge at standard observation")
    print(json.dumps({"status": "PASS", "healthy_crm_within_60_percent": a["crm_converged_60s_percent"],
                      "degraded_full_drain_seconds": d["observation_seconds"],
                      "repeated_entity_actions": c["business_actions"],
                      "repeated_entity_provider_reads": reads}, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (OSError, KeyError, ValueError, RuntimeError) as exc:
        print("FAIL: " + str(exc), file=sys.stderr)
        raise SystemExit(1) from None
