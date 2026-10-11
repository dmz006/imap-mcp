#!/usr/bin/env python3
"""Validate security/accepted-risks.yml (AGENT.md D65).

Fails (exit 1) on:
  - a missing field: id, kind, package, version, severity, impact.traced
    (bool), impact.reachable (yes|no|unknown), impact.analysis (non-empty),
    impact.method (when traced), first_added, added, expires, validated_by,
    reason;
  - severity not one of low|medium|moderate|high|critical;
  - kind not one of go-module|go-stdlib;
  - an id that is duplicated or lacks a cve/, osv/, dependabot/ or
    code-scanning/ prefix;
  - dates that are not YYYY-MM-DD, first_added after added, or expires later
    than added + 90 days (traced) / + 30 days (untraced);
  - with --base REF: an entry whose first_added differs from its value at
    REF (first_added never changes when an entry is renewed).

Prints ESCALATE lines (not failures) for a high/critical entry marked
reachable "yes", and for a renewed entry (added > first_added) that is past
its first acceptance window. Being past `expires` is not a failure here;
the daily re-validation reports it (D68).

Usage: check_accepted_risks.py [--base REF] [path]
"""
import argparse
import datetime as dt
import subprocess
import sys
from pathlib import Path

import yaml

PREFIXES = ("cve/", "osv/", "dependabot/", "code-scanning/")
SEVERITIES = {"low", "medium", "moderate", "high", "critical"}
KINDS = {"go-module", "go-stdlib"}
REACHABLE = {"yes", "no", "unknown"}


def day(v):
    if isinstance(v, dt.date):
        return v
    return dt.date.fromisoformat(str(v))


def load(text):
    data = yaml.safe_load(text) or {}
    return data.get("entries") or []


def check(entries, base_entries=None, today=None):
    today = today or dt.date.today()
    errors, escalations = [], []
    seen = set()
    base = {e.get("id"): e for e in (base_entries or [])}
    for i, e in enumerate(entries):
        where = e.get("id") or f"entry #{i + 1}"
        for f in ("id", "kind", "package", "version", "severity", "first_added", "added", "expires", "validated_by", "reason"):
            if e.get(f) in (None, ""):
                errors.append(f"{where}: missing {f}")
        imp = e.get("impact") or {}
        if not isinstance(imp.get("traced"), bool):
            errors.append(f"{where}: impact.traced must be true or false")
        if imp.get("reachable") not in REACHABLE:
            errors.append(f"{where}: impact.reachable must be yes, no or unknown")
        if not str(imp.get("analysis") or "").strip():
            errors.append(f"{where}: impact.analysis is empty")
        if imp.get("traced") is True and not str(imp.get("method") or "").strip():
            errors.append(f"{where}: impact.method is required when traced")
        if e.get("severity") not in SEVERITIES:
            errors.append(f"{where}: severity must be one of {sorted(SEVERITIES)}")
        if e.get("kind") not in KINDS:
            errors.append(f"{where}: kind must be one of {sorted(KINDS)}")
        rid = str(e.get("id") or "")
        if not rid.startswith(PREFIXES):
            errors.append(f"{where}: id must start with one of {PREFIXES}")
        if rid in seen:
            errors.append(f"{where}: duplicate id")
        seen.add(rid)
        try:
            first, added, expires = day(e.get("first_added")), day(e.get("added")), day(e.get("expires"))
        except (TypeError, ValueError):
            errors.append(f"{where}: dates must be YYYY-MM-DD")
            continue
        if first > added:
            errors.append(f"{where}: first_added is after added")
        window = 90 if imp.get("traced") is True else 30
        if expires > added + dt.timedelta(days=window):
            errors.append(f"{where}: expires is more than {window} days after added")
        if rid in base and str(base[rid].get("first_added")) != str(e.get("first_added")):
            errors.append(f"{where}: first_added changed (was {base[rid].get('first_added')})")
        if imp.get("reachable") == "yes" and e.get("severity") in ("high", "critical"):
            escalations.append(f"ESCALATE {where}: {e.get('severity')} and reachable")
        if added > first and today > first + dt.timedelta(days=window):
            escalations.append(f"ESCALATE {where}: renewed past its first acceptance window")
    return errors, escalations


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", help="git ref to compare first_added against")
    ap.add_argument("path", nargs="?", default="security/accepted-risks.yml")
    a = ap.parse_args()
    entries = load(Path(a.path).read_text(encoding="utf-8"))
    base_entries = None
    if a.base:
        r = subprocess.run(["git", "show", f"{a.base}:{a.path}"], capture_output=True, text=True)
        base_entries = load(r.stdout) if r.returncode == 0 else []
    errors, escalations = check(entries, base_entries)
    for line in escalations:
        print(line)
    for line in errors:
        print(f"::error::{line}")
    print(f"accepted-risks: {len(entries)} entries, {len(errors)} errors, {len(escalations)} escalations")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
