#!/usr/bin/env python3
"""Gate govulncheck results against security/accepted-risks.yml (D65).

Reads `govulncheck -format json ./...` output. A finding is reachable when
govulncheck traced it to a function imap-mcp calls. The gate fails (exit 1)
on any reachable finding whose OSV id is not an accepted-risk entry
(osv/<id>). Findings that are only imported or only required are listed
for information. Accepted entries govulncheck no longer reports are listed
as stale (the daily re-validation removes them, D68).

Usage: vuln_gate.py govulncheck.json [security/accepted-risks.yml]
"""
import json
import sys
from pathlib import Path

import yaml


def read_stream(path):
    """govulncheck JSON is a stream of concatenated objects."""
    text = Path(path).read_text(encoding="utf-8")
    dec, i, out = json.JSONDecoder(), 0, []
    while i < len(text):
        while i < len(text) and text[i].isspace():
            i += 1
        if i >= len(text):
            break
        obj, i = dec.raw_decode(text, i)
        out.append(obj)
    return out


def findings(path):
    """Map OSV id -> {'level': called|imported|required, 'module', 'version', 'fixed'}."""
    rank = {"required": 0, "imported": 1, "called": 2}
    out = {}
    for obj in read_stream(path):
        f = obj.get("finding")
        if not f:
            continue
        trace = f.get("trace") or [{}]
        top = trace[0]
        level = "called" if top.get("function") else ("imported" if top.get("package") else "required")
        cur = out.get(f["osv"])
        if cur is None or rank[level] > rank[cur["level"]]:
            out[f["osv"]] = {"level": level, "module": top.get("module", ""), "version": top.get("version", ""),
                             "fixed": f.get("fixed_version", "")}
    return out


def accepted(path):
    data = yaml.safe_load(Path(path).read_text(encoding="utf-8")) or {}
    return {str(e["id"]).removeprefix("osv/"): e for e in data.get("entries") or [] if str(e.get("id", "")).startswith("osv/")}


def main():
    vuln_json = sys.argv[1]
    reg = sys.argv[2] if len(sys.argv) > 2 else "security/accepted-risks.yml"
    found, acc = findings(vuln_json), accepted(reg)
    failed = 0
    for osv, f in sorted(found.items()):
        where = f"{osv} in {f['module']}@{f['version']} (fixed in {f['fixed'] or 'none'})"
        if f["level"] == "called" and osv not in acc:
            print(f"::error::reachable vulnerability {where}: upgrade, or trace it and add osv/{osv} to {reg}")
            failed += 1
        elif osv in acc:
            print(f"accepted: {where} [{f['level']}]")
        else:
            print(f"info: {where} [{f['level']}, not called]")
    for osv in sorted(set(acc) - set(found)):
        print(f"stale: osv/{osv} is accepted but govulncheck no longer reports it")
    print(f"vuln-gate: {len(found)} findings, {failed} unaccepted reachable")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
