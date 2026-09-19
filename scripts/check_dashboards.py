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

# An empty result is AMBIGUOUS, and that ambiguity was making this script fail
# on a perfectly healthy system.
#
# Two completely different things produce zero series:
#
#   1. Someone renamed or deleted a metric.   ← a real bug, must fail CI
#   2. Nothing is wrong right now.            ← healthy: no 5xx, no alerts firing
#
# The original approach — a hand-maintained set of panel titles expected to be
# empty — could not tell them apart. It went stale whenever a panel was renamed,
# exempted every target on a panel when only one was error-filtered, and still
# red-lighted `make check` on an idle service.
#
# So instead of guessing from the title, ask Prometheus directly: does every
# metric name in this expression exist? A rename makes the name vanish, which is
# detectable no matter what the query returns. A healthy service still has all
# its metrics; they just have no matching samples right now.
#
# Names Prometheus synthesises rather than stores. ALERTS only exists once an
# alert has fired, so on a healthy server it is legitimately absent from
# __name__ values — treating that as a rename would be exactly the false
# positive this rewrite removes.
SYNTHETIC_METRICS = {"ALERTS", "ALERTS_FOR_STATE"}

# PromQL keywords, modifiers and aggregation clauses that look like identifiers
# but are not metric names.
PROMQL_KEYWORDS = {
    "by", "without", "on", "ignoring", "group_left", "group_right",
    "offset", "bool", "and", "or", "unless", "start", "end", "step",
    "inf", "nan",
}


def query(base, expr):
    url = base.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    with urllib.request.urlopen(url, timeout=30) as resp:
        return json.load(resp)


def known_metric_names(base):
    """Every metric name Prometheus currently holds, including recording rules."""
    url = base.rstrip("/") + "/api/v1/label/__name__/values"
    with urllib.request.urlopen(url, timeout=30) as resp:
        data = json.load(resp)
    if data.get("status") != "success":
        raise RuntimeError(f"could not list metric names: {data.get('error')}")
    return set(data["data"])


# Strip the parts of an expression where an identifier is NOT a metric name:
# string literals, label matchers inside {}, range/offset selectors, and the
# label lists in by()/without()/on()/ignoring()/group_left()/group_right().
_STRIP = [
    re.compile(r'"[^"]*"'),
    re.compile(r"'[^']*'"),
    re.compile(r"\{[^{}]*\}"),
    re.compile(r"\[[^\[\]]*\]"),
    re.compile(r"\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^()]*\)",
               re.IGNORECASE),
]

_IDENT = re.compile(r"[a-zA-Z_:][a-zA-Z0-9_:]*")


def metric_names(expr):
    """Best-effort extraction of the metric names an expression selects."""
    for pattern in _STRIP:
        # Repeat: removing a {...} can expose a by(...) that was not adjacent
        # before, and vice versa.
        prev = None
        while prev != expr:
            prev = expr
            expr = pattern.sub(" ", expr)

    names = set()
    for m in _IDENT.finditer(expr):
        name = m.group(0)
        # A trailing "(" means this is a function call, not a selector.
        rest = expr[m.end():].lstrip()
        if rest.startswith("("):
            continue
        if name.lower() in PROMQL_KEYWORDS:
            continue
        names.add(name)
    return names


def expand(expr):
    """Replace Grafana template vars so the expression parses."""
    expr = expr.replace("$__rate_interval", "5m").replace("$__interval", "1m")
    # =~"$var" becomes a match-anything regex
    expr = re.sub(r'=~\s*"\$\w+"', '=~".*"', expr)
    expr = re.sub(r'=\s*"\$\w+"', '=~".*"', expr)
    return expr


def panel_ds(panel, target):
    """Which datasource does this target use? Panel-level unless overridden."""
    ds = target.get("datasource") or panel.get("datasource") or {}
    return (ds.get("type") or "").lower()


def walk_panels(panels):
    for p in panels:
        yield p
        for sub in p.get("panels", []) or []:
            yield sub


def main():
    base = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:9095"
    dashdir = sys.argv[2] if len(sys.argv) > 2 else "grafana/dashboards"

    try:
        known = known_metric_names(base)
    except Exception as e:  # noqa: BLE001
        print(f"  cannot reach Prometheus at {base}: {e}")
        return 2

    total = ok = empty = failed = skipped = 0
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

                # Only Prometheus targets can be checked here. LogQL panels
                # need a Loki endpoint and a different query API.
                if panel_ds(panel, t) != "prometheus":
                    skipped += 1
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
                if n:
                    ok += 1
                    print(f"    ok      {title:<42} {n} series")
                    continue

                # Empty. The question is WHY — see the note on SYNTHETIC_METRICS.
                empty += 1
                missing = sorted(
                    m for m in metric_names(expr)
                    if m not in known and m not in SYNTHETIC_METRICS
                )
                if missing:
                    why = "unknown metric(s): " + ", ".join(missing)
                    problems.append((dash["title"], title, why))
                    print(f"    BROKEN  {title:<42} {why}")
                else:
                    # Every metric exists; there is simply nothing matching
                    # right now. That is the healthy state for an error panel.
                    print(f"    empty   {title:<42} 0 series (metrics exist)")

    print(f"\n  {total} prometheus queries: {ok} with data, {empty} empty, {failed} invalid"
          + (f"  ({skipped} non-prometheus targets skipped)" if skipped else ""))

    if problems:
        print("\n  needs attention:")
        for dash, panel, why in problems:
            print(f"    {dash} / {panel}: {why}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
