#!/usr/bin/env python3
"""Audit the small EN/RU UI dictionary and static/dynamic lookup sites."""

from collections import Counter
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
API = ROOT / "internal" / "api"
SOURCE = (API / "ui_locale.go").read_text()
ROWS = re.findall(r'^\s*"([^"]+)":\s*\{"((?:[^"\\]|\\.)*)",\s*"((?:[^"\\]|\\.)*)"\},?\s*$', SOURCE, re.M)
KEYS = {row[0]: (row[1], row[2]) for row in ROWS}
PRODUCTION = "\n".join(path.read_text() for path in API.glob("*.go")
                       if path.name != "ui_locale.go" and not path.name.endswith("_test.go"))
LITERALS = set(re.findall(r'\btr\s*\([^,]+,\s*"([^"]+)"\)', PRODUCTION))
LITERALS.update(re.findall(r'\btr\s+[^\s}]+\s+"([^"]+)"', PRODUCTION))
DYNAMIC_PREFIXES = set(re.findall(r'\b(?:printf|Sprintf)\s*\(?(?:\s*)"([^"%]*?)%s"', PRODUCTION))
DECLARED_DYNAMIC = {key for key in KEYS if any(key.startswith(prefix) for prefix in DYNAMIC_PREFIXES)}
OTHER_REFERENCES = {key for key in KEYS if re.search(r'"' + re.escape(key) + r'"', PRODUCTION)}


def verbs(value):
    return re.findall(r'(?<!%)%(?:\[[0-9]+\])?[-+#0 ]*(?:[0-9]+|\*)?(?:\.[0-9*]+)?[a-zA-Z]',
                      value.replace("%%", ""))


errors = []
duplicates = [key for key, count in Counter(row[0] for row in ROWS).items() if count > 1]
if duplicates:
    errors.append("duplicate keys: " + ", ".join(sorted(duplicates)))
for key, (english, russian) in KEYS.items():
    if not english or not russian:
        errors.append("empty EN/RU translation: " + key)
    if verbs(english) != verbs(russian):
        errors.append("placeholder mismatch: " + key)
missing = sorted(LITERALS - KEYS.keys())
if missing:
    errors.append("missing literal keys: " + ", ".join(missing))
for prefix in DYNAMIC_PREFIXES:
    if not any(key.startswith(prefix) for key in KEYS):
        errors.append("dynamic prefix has no dictionary entries: " + prefix)
unused_candidates = sorted(KEYS.keys() - LITERALS - DECLARED_DYNAMIC - OTHER_REFERENCES)
report = {"entries": len(KEYS), "literal_lookups": len(LITERALS),
          "dynamic_prefixes": sorted(DYNAMIC_PREFIXES), "unused_candidates": unused_candidates,
          "errors": errors}
print(json.dumps(report, indent=2, ensure_ascii=False))
sys.exit(1 if errors else 0)
