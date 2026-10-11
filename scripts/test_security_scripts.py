#!/usr/bin/env python3
"""Tests for the accepted-risks scripts (run: python3 scripts/test_security_scripts.py)."""
import datetime as dt
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import check_accepted_risks as car  # noqa: E402
import risk_watch as rw  # noqa: E402
import vuln_gate as vg  # noqa: E402


def entry(**over):
    e = {"id": "osv/GO-2026-0001", "kind": "go-module", "package": "example.com/m", "version": "v1.0.0",
         "severity": "moderate", "impact": {"traced": True, "reachable": "no", "analysis": "not called", "method": "govulncheck"},
         "first_added": "2026-10-01", "added": "2026-10-01", "expires": "2026-12-30", "validated_by": "test", "reason": "no fix"}
    e.update(over)
    return e


def stream(*finds):
    f = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    f.write(json.dumps({"config": {"go_version": "go1.27.2"}}) + "\n")
    for osv, level, fixed in finds:
        tr = {"module": "example.com/m", "version": "v1.0.0"}
        if level in ("imported", "called"):
            tr["package"] = "example.com/m/p"
        if level == "called":
            tr["function"] = "F"
        f.write(json.dumps({"finding": {"osv": osv, "fixed_version": fixed, "trace": [tr]}}, indent=2) + "\n")
    f.close()
    return f.name


class Registry(unittest.TestCase):
    def test_valid(self):
        errs, esc = car.check([entry()], today=dt.date(2026, 10, 11))
        self.assertEqual((errs, esc), ([], []))

    def test_rules(self):
        cases = [
            (entry(severity="bad"), "severity"),
            (entry(kind="docker"), "kind"),
            (entry(id="GO-1"), "id must start"),
            (entry(expires="2027-06-01"), "more than 90 days"),
            (entry(impact={"traced": False, "reachable": "no", "analysis": "x"}, expires="2026-12-01"), "more than 30 days"),
            (entry(impact={"traced": True, "reachable": "no", "analysis": "x"}), "impact.method"),
            (entry(first_added="2026-10-05"), "first_added is after added"),
            (entry(reason=""), "missing reason"),
        ]
        for e, want in cases:
            errs, _ = car.check([e], today=dt.date(2026, 10, 11))
            self.assertTrue(any(want in x for x in errs), (want, errs))
        errs, _ = car.check([entry(), entry()])
        self.assertTrue(any("duplicate" in x for x in errs))

    def test_base_and_escalate(self):
        errs, _ = car.check([entry(first_added="2026-09-01", added="2026-10-01")], base_entries=[entry()])
        self.assertTrue(any("first_added changed" in x for x in errs))
        _, esc = car.check([entry(severity="high", impact={"traced": True, "reachable": "yes", "analysis": "x", "method": "m"})])
        self.assertTrue(esc)


class Gate(unittest.TestCase):
    def test_gate(self):
        reg = tempfile.NamedTemporaryFile("w", suffix=".yml", delete=False)
        reg.write("entries:\n  - id: osv/GO-2026-0001\n    package: example.com/m\n")
        reg.close()
        self.assertEqual(vg.findings(stream(("GO-2026-0002", "called", "v1.1.0")))["GO-2026-0002"]["level"], "called")
        sys.argv = ["vuln_gate.py", stream(("GO-2026-0002", "called", "v1.1.0")), reg.name]
        self.assertEqual(vg.main(), 1)
        sys.argv = ["vuln_gate.py", stream(("GO-2026-0001", "called", ""), ("GO-2026-0003", "required", "")), reg.name]
        self.assertEqual(vg.main(), 0)


class Watch(unittest.TestCase):
    def test_classify(self):
        found = vg.findings(stream(("GO-2026-0001", "required", "v1.2.0")))
        today = dt.date(2026, 12, 20)
        fix, retrace, exp = rw.classify([entry()], found, {"example.com/m": "v1.0.0"}, "go1.27.2", today)
        self.assertEqual((len(fix), len(retrace), len(exp)), (1, 0, 1))
        fix, retrace, _ = rw.classify([entry()], vg.findings(stream(("GO-2026-0001", "required", ""))), {"example.com/m": "v1.0.5"}, "", today)
        self.assertEqual((len(fix), len(retrace)), (0, 1))
        fix, _, _ = rw.classify([entry()], {}, {}, "", today)  # no longer reported
        self.assertEqual(len(fix), 1)

    def test_code_scanning(self):
        e = entry(id="code-scanning/7", kind="code", package="go/log-injection", version="internal/x.go")
        today = dt.date(2026, 10, 11)
        self.assertEqual(len(rw.classify([e], {}, {}, "", today, {"7": "dismissed"})[0]), 0)
        self.assertEqual(len(rw.classify([e], {}, {}, "", today, {"7": "fixed"})[0]), 1)
        self.assertEqual(len(rw.classify([e], {}, {}, "", today, {"7": "open"})[1]), 1)
        errs, _ = car.check([e], today=today)
        self.assertEqual(errs, [])

    def test_remove_blocks(self):
        text = "# head\nentries:\n  - id: osv/A\n    package: a\n\n  - id: osv/B\n    package: b\n"
        self.assertEqual(rw.remove_blocks(text, {"osv/A"}), "# head\nentries:\n  - id: osv/B\n    package: b\n")


if __name__ == "__main__":
    unittest.main()
