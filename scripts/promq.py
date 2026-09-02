#!/usr/bin/env python3
"""Tiny Prometheus HTTP API client for the Makefile.

Exists so PromQL results are readable in a terminal without opening the web UI.
Everything here is a thin wrapper over /api/v1/*, which is worth knowing about
in its own right: it is the same API Grafana uses, and it is how you script
against Prometheus in CI or a runbook.
"""

import json
import sys
import urllib.parse
import urllib.request


def fetch(base, path, params=None):
    url = base.rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    with urllib.request.urlopen(url, timeout=15) as resp:
        return json.load(resp)


def fmt_labels(metric, drop=("__name__",)):
    items = [(k, v) for k, v in sorted(metric.items()) if k not in drop]
    if not items:
        return "{}"
    inner = ", ".join(f'{k}="{v}"' for k, v in items)
    return "{" + inner + "}"


def cmd_query(base, expr):
    data = fetch(base, "/api/v1/query", {"query": expr})
    if data.get("status") != "success":
        print("  error:", data.get("error", "unknown"), file=sys.stderr)
        return 1

    result = data["data"]["result"]
    rtype = data["data"]["resultType"]

    if not result:
        print("  (no data)")
        return 0

    if rtype == "scalar":
        print(f"  {float(result[1]):.6g}")
        return 0

    name_w = max(len(fmt_labels(s["metric"])) for s in result)
    name_w = min(max(name_w, 20), 88)

    for s in sorted(result, key=lambda s: fmt_labels(s["metric"])):
        name = s["metric"].get("__name__", "")
        labels = fmt_labels(s["metric"])
        try:
            val = f"{float(s['value'][1]):.6g}"
        except (ValueError, KeyError):
            val = str(s.get("value", ["", "?"])[1])
        prefix = f"{name}{labels}" if name else labels
        print(f"  {prefix:<{name_w + len(name)}}  {val}")
    return 0


def cmd_targets(base):
    data = fetch(base, "/api/v1/targets")
    targets = data["data"]["activeTargets"]
    if not targets:
        print("  (no active targets)")
        return 0
    for t in targets:
        job = t["labels"].get("job", "?")
        inst = t["labels"].get("instance", "?")
        health = t["health"]
        dur = t.get("lastScrapeDuration", 0) * 1000
        err = t.get("lastError", "")
        mark = "OK " if health == "up" else "DOWN"
        line = f"  [{mark}] {job:<12} {inst:<22} {t['scrapeUrl']:<44} {dur:6.1f}ms"
        if err:
            line += f"  error={err}"
        print(line)
    return 0


def cmd_series_count(base):
    """Where are my series actually going?"""
    data = fetch(base, "/api/v1/query", {"query": "count by (__name__) ({__name__=~'.+'})"})
    rows = sorted(
        ((int(float(s["value"][1])), s["metric"]["__name__"]) for s in data["data"]["result"]),
        reverse=True,
    )
    total = sum(n for n, _ in rows)
    print(f"  {total} series across {len(rows)} metric names\n")
    print(f"  {'series':>7}  metric")
    for n, name in rows[:25]:
        print(f"  {n:>7}  {name}")
    return 0


def cmd_rules(base):
    data = fetch(base, "/api/v1/rules")
    for g in data["data"]["groups"]:
        rec = [r for r in g["rules"] if r["type"] == "recording"]
        alr = [r for r in g["rules"] if r["type"] == "alerting"]
        print(f"\n  {g['name']}  ({len(rec)} recording, {len(alr)} alerting, every {g['interval']}s)")
        for r in rec:
            health = "" if r["health"] == "ok" else f"  !! {r['health']} {r.get('lastError','')}"
            print(f"    record  {r['name']}{health}")
        for r in alr:
            state = r.get("state", "?")
            mark = {"firing": "FIRING ", "pending": "pending", "inactive": "  ok   "}.get(state, state)
            sev = r["labels"].get("severity", "-")
            health = "" if r["health"] == "ok" else f"  !! {r['health']} {r.get('lastError','')}"
            print(f"    [{mark}] {r['name']:<38} severity={sev:<9} for={r.get('duration',0):.0f}s{health}")
    return 0


def cmd_alerts(base):
    data = fetch(base, "/api/v1/alerts")
    alerts = data["data"]["alerts"]
    if not alerts:
        print("  no pending or firing alerts")
        return 0
    for a in sorted(alerts, key=lambda a: (a["state"], a["labels"].get("alertname", ""))):
        name = a["labels"].get("alertname", "?")
        sev = a["labels"].get("severity", "-")
        val = a.get("value", "")
        try:
            val = f"{float(val):.4g}"
        except (TypeError, ValueError):
            pass
        print(f"  [{a['state'].upper():<7}] {name:<38} severity={sev:<9} value={val}")
        summary = a.get("annotations", {}).get("summary")
        if summary:
            print(f"            {summary}")
    return 0


def cmd_am_alerts(base):
    """Alertmanager's own view — after grouping, silencing and inhibition."""
    data = fetch(base, "/api/v2/alerts")
    if not data:
        print("  Alertmanager holds no alerts")
        return 0
    for a in data:
        labels = a.get("labels", {})
        status = a.get("status", {})
        state = status.get("state", "?")
        inhibited = status.get("inhibitedBy") or []
        silenced = status.get("silencedBy") or []
        flags = ""
        if inhibited:
            flags += "  INHIBITED"
        if silenced:
            flags += "  SILENCED"
        print(f"  [{state.upper():<9}] {labels.get('alertname','?'):<38} "
              f"severity={labels.get('severity','-'):<9}{flags}")
    return 0


def main():
    if len(sys.argv) < 3:
        print("usage: promq.py <base-url> <query|targets|top|rules|alerts|amalerts> [expr]",
              file=sys.stderr)
        return 2

    base, cmd = sys.argv[1], sys.argv[2]
    try:
        if cmd == "targets":
            return cmd_targets(base)
        if cmd == "top":
            return cmd_series_count(base)
        if cmd == "rules":
            return cmd_rules(base)
        if cmd == "alerts":
            return cmd_alerts(base)
        if cmd == "amalerts":
            return cmd_am_alerts(base)
        if cmd == "query":
            if len(sys.argv) < 4 or not sys.argv[3].strip():
                print("  (empty query — pass Q='...')", file=sys.stderr)
                return 2
            return cmd_query(base, sys.argv[3])
    except urllib.error.URLError as e:
        print(f"  cannot reach Prometheus at {base}: {e}", file=sys.stderr)
        return 1

    print(f"unknown command: {cmd}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
