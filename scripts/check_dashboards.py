#!/usr/bin/env python3
"""Run every panel query in every dashboard against Prometheus.

A dashboard panel that silently returns nothing is the most common failure in
observability work: someone renames a metric, and the panel shows an empty
graph that looks exactly like "the system is idle". This script is the cheap
guard — run it in CI after any change to metric names, recording rules or
dashboards.

Template variables are substituted with a permissive default so the query is
at least syntactically checkable.

    python3 scripts/check_dashboards.py http://localhost:9095 grafana/dashboards
"""

import glob
import json
import os
import re
import sys
import urllib.parse
import urllib.request

# Panels that are EXPECTED to be empty on a healthy system: they filter on
# error conditions that should not be occurring.
EXPECT_EMPTY = {
    "Query errors by SQLSTATE",
    "Firing alerts",
    # Only the FAILED series of this panel is filtered with `> 0`; the "sent"
    # series must always return data, and does.
    "Is alert DELIVERY healthy?",
}


def query(base, expr):
    url = base.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    with urllib.request.urlopen(url, timeout=30) as resp:
        return json.load(resp)


def expand(expr):
    """Replace Grafana template vars so the expression parses."""
    expr = expr.replace("$__rate_interval", "5m").replace("$__interval", "1m")
    # =~"$var" becomes a match-anything regex
    expr = re.sub(r'=~\s*"\$\w+"', '=~".*"', expr)
    expr = re.sub(r'=\s*"\$\w+"', '=~".*"', expr)
    return expr


def walk_panels(panels):
    for p in panels:
        yield p
        for sub in p.get("panels", []) or []:
            yield sub


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:9095"
    dashdir = sys.argv[2] if len(sys.argv) > 2 else "grafana/dashboards"

    total = ok = empty = failed = 0
    problems = []

    for path in sorted(glob.glob(os.path.join(dashdir, "*.json"))):
        dash = json.load(open(path))
        print(f"\n  {dash['title']}")

        for panel in walk_panels(dash.get("panels", [])):
            title = panel.get("title", "(untitled)")
            for t in panel.get("targets", []) or []:
                expr = t.get("expr")
                if not expr:
                    continue
                total += 1
                try:
                    data = query(base, expand(expr))
                except Exception as e:  # noqa: BLE001
                    failed += 1
                    problems.append((dash["title"], title, f"request failed: {e}"))
                    print(f"    ERROR   {title}  ({e})")
                    continue

                if data.get("status") != "success":
                    failed += 1
                    err = data.get("error", "unknown")
                    problems.append((dash["title"], title, err))
                    print(f"    BAD     {title:<42} {err}")
                    continue

                n = len(data["data"]["result"])
                if n == 0:
                    empty += 1
                    mark = "empty  " if title in EXPECT_EMPTY else "EMPTY !"
                    if title not in EXPECT_EMPTY:
                        problems.append((dash["title"], title, "returned no series"))
                    print(f"    {mark} {title:<42} 0 series")
                else:
                    ok += 1
                    print(f"    ok      {title:<42} {n} series")

    print(f"\n  {total} queries: {ok} with data, {empty} empty, {failed} invalid")

    unexpected = [p for p in problems if p[2] != "returned no series" or p[1] not in EXPECT_EMPTY]
    if unexpected:
        print("\n  needs attention:")
        for dash, panel, why in unexpected:
            print(f"    {dash} / {panel}: {why}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
