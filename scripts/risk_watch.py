#!/usr/bin/env python3
"""Daily re-validation of security/accepted-risks.yml (D68).

Inputs: the registry, a fresh `govulncheck -format json ./...` result,
`go list -m -json all` (the module versions in use) and, for code-scanning
entries, the repository's code-scanning alerts (`--code-alerts`, the JSON
array from the API). Sorts every entry into:

  fixable   - govulncheck reports a fixed version for it, or no longer
              reports it at all: upgrade the module and remove the entry;
  retrace   - the module (or Go toolchain) version in use differs from the
              accepted `version`, or a dismissed code-scanning alert is
              open again: the analysis must be redone;
  (code-scanning entries whose alert is fixed are fixable: removed only)
  expiring  - past `expires`, or due within 14 days.

Writes:
  --report FILE   markdown for the tracking issue (retrace + expiring)
  --upgrades FILE one `module@version` per line to `go get` (fixable)
  --remove FILE   one registry id per line to delete (fixable)

With --apply-removals, deletes the listed entries' blocks from the registry
as text, leaving every other line untouched.

Usage:
  risk_watch.py --registry R --vulns V --modules M --report F --upgrades F --remove F
  risk_watch.py --registry R --apply-removals IDS_FILE
"""
import argparse
import datetime as dt
import json
import re
import sys
from pathlib import Path

import yaml

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from vuln_gate import findings, read_stream  # noqa: E402


def module_versions(path):
    out = {}
    for obj in read_stream(path):
        out[obj.get("Path")] = obj.get("Version", "")
    return out


def classify(entries, found, mods, goversion, today, alerts=None):
    fixable, retrace, expiring = [], [], []
    alerts = alerts or {}
    for e in entries:
        rid = str(e["id"])
        if rid.startswith("code-scanning/"):
            state = alerts.get(rid.removeprefix("code-scanning/"))
            if state == "fixed" or (alerts and state is None):
                fixable.append((e, None))
            elif state == "open":
                retrace.append((e, "alert open again"))
            exp = dt.date.fromisoformat(str(e["expires"]))
            if exp <= today + dt.timedelta(days=14):
                expiring.append((e, exp))
            continue
        osv = rid.removeprefix("osv/")
        f = found.get(osv)
        pkg = e.get("package")
        in_use = goversion if e.get("kind") == "go-stdlib" else mods.get(pkg, "")
        if rid.startswith("osv/") and (f is None or (f["fixed"] and e.get("kind") == "go-module")):
            fixable.append((e, f))
        elif in_use and str(e.get("version")) != in_use:
            retrace.append((e, in_use))
        exp = dt.date.fromisoformat(str(e["expires"]))
        if exp <= today + dt.timedelta(days=14):
            expiring.append((e, exp))
    return fixable, retrace, expiring


def remove_blocks(text, ids):
    """Drop each `  - id: X` block (until the next entry or end)."""
    lines, out, skip = text.splitlines(keepends=True), [], False
    for line in lines:
        m = re.match(r"^  - id:\s*(\S+)", line)
        if m:
            skip = m.group(1).strip("\"'") in ids
        if not skip:
            out.append(line)
    return "".join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--registry", default="security/accepted-risks.yml")
    ap.add_argument("--vulns")
    ap.add_argument("--modules")
    ap.add_argument("--goversion", default="")
    ap.add_argument("--code-alerts", help="code-scanning alerts JSON (array with number, state)")
    ap.add_argument("--report")
    ap.add_argument("--upgrades")
    ap.add_argument("--remove")
    ap.add_argument("--apply-removals")
    a = ap.parse_args()

    if a.apply_removals:
        ids = {l.strip() for l in Path(a.apply_removals).read_text().splitlines() if l.strip()}
        text = Path(a.registry).read_text(encoding="utf-8")
        Path(a.registry).write_text(remove_blocks(text, ids), encoding="utf-8")
        print(f"removed {len(ids)} entr{'y' if len(ids) == 1 else 'ies'}")
        return 0

    entries = (yaml.safe_load(Path(a.registry).read_text(encoding="utf-8")) or {}).get("entries") or []
    found, mods = findings(a.vulns), module_versions(a.modules)
    alerts = {}
    if a.code_alerts:
        alerts = {str(x["number"]): x["state"] for x in json.loads(Path(a.code_alerts).read_text(encoding="utf-8"))}
    fixable, retrace, expiring = classify(entries, found, mods, a.goversion, dt.date.today(), alerts)

    with open(a.upgrades, "w") as up, open(a.remove, "w") as rm:
        for e, f in fixable:
            rm.write(str(e["id"]) + "\n")
            if f and f["fixed"] and e.get("kind") == "go-module":
                up.write(f"{e['package']}@{f['fixed']}\n")
    with open(a.report, "w") as r:
        if retrace:
            r.write("## Re-trace needed (the version in use changed)\n\n")
            for e, v in retrace:
                r.write(f"- `{e['id']}` {e['package']}: accepted at {e['version']}, now {v}\n")
            r.write("\n")
        if expiring:
            r.write("## Past expiry or due within 14 days\n\n")
            for e, exp in expiring:
                r.write(f"- `{e['id']}` {e['package']}: expires {exp}\n")
            r.write("\nRe-trace each entry, then renew it (`added` and `expires`; never `first_added`) or fix it.\n")
    print(f"risk-watch: {len(entries)} entries; fixable {len(fixable)}, re-trace {len(retrace)}, expiring {len(expiring)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
