#!/usr/bin/env python3
"""Summarise an s3-tests run and name the promotion candidates.

A promotion candidate is a test that passed but is not yet in
implemented_tests.txt. Moving it onto that list turns today's accident into
tomorrow's guarantee, because the gate then runs it on every pull request.

Usage: report.py <results.xml> [implemented_tests.txt]
"""
import sys
import xml.etree.ElementTree as ET


def load_whitelist(path):
    try:
        with open(path, encoding="utf-8") as fh:
            return {
                line.strip()
                for line in fh
                if line.strip() and not line.lstrip().startswith("#")
            }
    except FileNotFoundError:
        return set()


def node_id(case):
    """Rebuild the pytest node id pytest would accept back on the command line."""
    f = (case.get("file") or "").strip()
    name = case.get("name") or ""
    if f:
        return f"{f}::{name}"
    # Fall back to classname, which is dotted rather than a path.
    cls = (case.get("classname") or "").replace(".", "/")
    return f"{cls}.py::{name}" if cls else name


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    results, whitelist_path = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else None)

    root = ET.parse(results).getroot()
    suite = root if root.tag == "testsuite" else root.find("testsuite")
    if suite is None:
        print("report: no testsuite in the results file", file=sys.stderr)
        return 2

    passed, failed, errored, skipped = [], [], [], []
    for case in suite.findall("testcase"):
        nid = node_id(case)
        if case.find("failure") is not None:
            failed.append(nid)
        elif case.find("error") is not None:
            errored.append(nid)
        elif case.find("skipped") is not None:
            skipped.append(nid)
        else:
            passed.append(nid)

    executed = len(passed) + len(failed) + len(errored)
    print("s3-tests summary")
    print("================")
    print(f"  passed   {len(passed)}")
    print(f"  failed   {len(failed)}")
    print(f"  errored  {len(errored)}")
    print(f"  skipped  {len(skipped)}")
    if executed:
        print(f"  pass rate of executed: {len(passed) / executed * 100:.1f}%")

    if not whitelist_path:
        return 0

    whitelist = load_whitelist(whitelist_path)
    candidates = sorted(set(passed) - whitelist)
    regressions = sorted(whitelist & (set(failed) | set(errored)))

    if regressions:
        print(f"\nREGRESSIONS ({len(regressions)}) -- these are on the whitelist and no longer pass:")
        for nid in regressions:
            print(f"  {nid}")

    print(f"\npromotion candidates ({len(candidates)}) -- passing but not yet gated:")
    for nid in candidates:
        print(f"  {nid}")
    if candidates:
        print("\nAdd them to implemented_tests.txt to gate them from now on.")

    # A regression is the only thing worth failing a sweep over.
    return 1 if regressions else 0


if __name__ == "__main__":
    sys.exit(main())
