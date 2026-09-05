#!/usr/bin/env python3
"""Render stdout-exported OTel spans as a waterfall.

A stand-in for what Jaeger or Tempo draws. The point of a trace is the SHAPE:
you do not read the numbers first, you look at which bar is long.

    docker compose logs api --no-log-prefix | python3 scripts/waterfall.py [trace-id]
"""
import json
import re
import sys
from datetime import datetime

def parse_ts(s):
    # Go emits 9-digit nanoseconds; fromisoformat accepts at most 6.
    s = re.sub(r"(\.\d{6})\d+", r"\1", s).replace("Z", "+00:00")
    return datetime.fromisoformat(s)

def load(text):
    spans, buf, depth = [], "", 0
    for line in text.split("\n"):
        if depth == 0 and line.startswith("{") and not line.endswith("}"):
            buf, depth = line, 1
            continue
        if depth:
            buf += "\n" + line
            if line == "}":
                try:
                    spans.append(json.loads(buf))
                except ValueError:
                    pass
                buf, depth = "", 0
    return spans

def main():
    spans = load(sys.stdin.read())
    want = sys.argv[1] if len(sys.argv) > 1 else None
    if want:
        spans = [s for s in spans if s.get("SpanContext", {}).get("TraceID") == want]
    if not spans:
        print("  no spans found")
        return 1

    groups = {}
    for s in spans:
        groups.setdefault(s["SpanContext"]["TraceID"], []).append(s)

    for tid, ss in list(groups.items())[:3]:
        ss.sort(key=lambda s: s["StartTime"])
        byid = {s["SpanContext"]["SpanID"]: s for s in ss}
        t0 = parse_ts(ss[0]["StartTime"])
        total = max((parse_ts(s["EndTime"]) - t0).total_seconds() * 1000 for s in ss) or 1

        print(f"\n  trace {tid}   {len(ss)} spans")
        for s in ss:
            d, p = 0, s.get("Parent", {}).get("SpanID")
            while p and p in byid and d < 8:
                d += 1
                p = byid[p].get("Parent", {}).get("SpanID")

            start = (parse_ts(s["StartTime"]) - t0).total_seconds() * 1000
            dur = (parse_ts(s["EndTime"]) - parse_ts(s["StartTime"])).total_seconds() * 1000
            width = 46
            off = int(start / total * width)
            ln = max(1, int(dur / total * width))
            bar = " " * off + "█" * ln
            status = s.get("Status", {}).get("Code", "")
            mark = " ERR" if status == "Error" else ""
            print("  %-30s %8.2fms |%-46s|%s" % ("  " * d + s["Name"], dur, bar[:width], mark))
    return 0

sys.exit(main())
