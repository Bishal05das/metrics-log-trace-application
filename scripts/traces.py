#!/usr/bin/env python3
"""Read traces from Jaeger without opening the UI.

The same reasoning as scripts/elk.py: the UI is better for exploring, this is
for when you are in a terminal with a trace ID, and it lets the trace pipeline
be verified from CI, which a browser cannot do.

It replaces scripts/waterfall.py, which parsed spans out of the stdout
exporter's output — a workaround for having no backend, and no longer needed.

    python3 scripts/traces.py health <jaeger-admin> <es>
    python3 scripts/traces.py show   <jaeger>  <trace_id>
    python3 scripts/traces.py slow   <jaeger>  ['operation']
"""

import json
import sys
import urllib.error
import urllib.parse
import urllib.request

DIM, BOLD, RED, YELLOW, GREEN, OFF = (
    "\033[2m", "\033[1m", "\033[31m", "\033[33m", "\033[32m", "\033[0m")


def get(url, timeout=20):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.load(r)


def get_text(url, timeout=20):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.read().decode()


def waterfall(spans, width=44):
    """Render spans as a waterfall, sorted by start time and nested by parent."""
    if not spans:
        print("  no spans")
        return

    t0 = min(s["startTime"] for s in spans)
    total = max(s["startTime"] + s["duration"] for s in spans) - t0
    total = total or 1

    # depth by walking CHILD_OF references
    by_id = {s["spanID"]: s for s in spans}
    def depth(s, seen=()):
        for r in s.get("references", []):
            if r.get("refType") == "CHILD_OF" and r["spanID"] in by_id:
                if r["spanID"] in seen:
                    return 0
                return 1 + depth(by_id[r["spanID"]], seen + (s["spanID"],))
        return 0

    for s in sorted(spans, key=lambda x: x["startTime"]):
        off = int((s["startTime"] - t0) / total * width)
        ln = max(1, int(s["duration"] / total * width))
        bar = " " * off + "█" * min(ln, width - off)
        err = any(t.get("key") == "error" and t.get("value")
                  for t in s.get("tags", []))
        colour = RED if err else ""
        name = "  " * depth(s) + s["operationName"]
        print(f"  {colour}{name:<34}{OFF} {s['duration']/1000:8.2f}ms "
              f"{DIM}|{bar:<{width}}|{OFF}")


def cmd_show(jaeger, trace_id):
    if not trace_id:
        print("  usage: make trace T=<trace_id>")
        return 2
    try:
        d = get(f"{jaeger}/api/traces/{urllib.parse.quote(trace_id)}")
    except urllib.error.HTTPError as e:
        print(f"  Jaeger returned {e.code}")
        return 1
    if not d.get("data"):
        print(f"  trace {trace_id} not found "
              f"{DIM}(spans are batched; allow ~5s after the request){OFF}")
        return 1
    t = d["data"][0]
    svc = {p: v["serviceName"] for p, v in t.get("processes", {}).items()}
    print(f"\n  trace {BOLD}{t['traceID']}{OFF} — {len(t['spans'])} spans "
          f"{DIM}({', '.join(sorted(set(svc.values())))}){OFF}\n")
    waterfall(t["spans"])
    return 0


def cmd_slow(jaeger, operation):
    params = {"service": "orders", "limit": "200", "lookback": "1h"}
    if operation:
        params["operation"] = operation
    url = f"{jaeger}/api/traces?" + urllib.parse.urlencode(params)
    try:
        d = get(url)
    except urllib.error.HTTPError as e:
        print(f"  Jaeger returned {e.code}")
        return 1

    traces = d.get("data") or []
    if not traces:
        print("  no traces in the last hour")
        return 0

    rows = []
    for t in traces:
        root = min(t["spans"], key=lambda s: s["startTime"])
        rows.append((root["duration"], root["operationName"], t["traceID"],
                     len(t["spans"])))
    rows.sort(reverse=True)

    print(f"\n  slowest of {len(traces)} traces in the last hour"
          + (f" for {operation}" if operation else "") + "\n")
    print(f"  {'duration':>10}  {'spans':>5}  {'operation':<26} trace")
    for dur, op, tid, n in rows[:15]:
        colour = RED if dur > 500_000 else (YELLOW if dur > 100_000 else "")
        print(f"  {colour}{dur/1000:>8.2f}ms{OFF}  {n:>5}  {op:<26} {DIM}{tid}{OFF}")
    print(f"\n  {DIM}make trace T=<trace>{OFF}")
    return 0


def cmd_health(admin, es):
    print()
    try:
        raw = get_text(f"{admin}/metrics")
    except Exception as e:  # noqa: BLE001
        print(f"  jaeger          {RED}unreachable{OFF} ({e})")
        return 1

    def metric(prefix, **want):
        total = 0.0
        for line in raw.splitlines():
            if line.startswith("#") or not line.startswith(prefix):
                continue
            if all(f'{k}="{v}"' in line for k, v in want.items()):
                try:
                    total += float(line.rsplit(" ", 1)[1])
                except ValueError:
                    pass
        return total

    received = metric("jaeger_collector_spans_received_total", svc="orders")
    saved = metric("jaeger_collector_spans_saved_by_svc_total", svc="orders",
                   result="ok")
    dropped = metric("jaeger_collector_spans_dropped_total")

    print(f"  spans received  {BOLD}{received:.0f}{OFF}  {DIM}(collector got them){OFF}")
    print(f"  spans saved     {BOLD}{saved:.0f}{OFF}  {DIM}(Elasticsearch accepted them){OFF}")
    colour = RED if dropped else GREEN
    print(f"  spans dropped   {colour}{dropped:.0f}{OFF}")

    if received and not saved:
        print(f"\n  {RED}spans arrive but none are stored — check Elasticsearch{OFF}")
    if dropped:
        print(f"\n  {YELLOW}the collector queue overflowed; traces have holes in them{OFF}")

    try:
        c = get(f"{es}/jaeger-span-*/_count")
        print(f"  documents       {BOLD}{c['count']}{OFF} in jaeger-span-*")
    except urllib.error.HTTPError:
        print(f"  documents       {YELLOW}no jaeger-span-* index yet{OFF}")
    except Exception as e:  # noqa: BLE001
        print(f"  elasticsearch   {RED}unreachable{OFF} ({e})")
        return 1

    if not received:
        print(f"\n  {YELLOW}nothing received yet{OFF} — run `make load`, then re-run")
    return 0


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    cmd, target = sys.argv[1], sys.argv[2].rstrip("/")
    arg = sys.argv[3].strip() if len(sys.argv) > 3 else ""

    if cmd == "health":
        return cmd_health(target, arg.rstrip("/") or "http://localhost:9201")
    if cmd == "show":
        return cmd_show(target, arg)
    if cmd == "slow":
        return cmd_slow(target, arg)
    print(f"unknown command {cmd}")
    return 2


if __name__ == "__main__":
    sys.exit(main())
