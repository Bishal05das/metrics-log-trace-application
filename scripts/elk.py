#!/usr/bin/env python3
"""Read the shipped logs without opening Kibana.

Kibana is the right tool for exploring. This is for the other case: you are in a
terminal, you have a trace ID, and you want the lines. It also exists so the
pipeline can be VERIFIED from CI, which a browser UI cannot do.

    python3 scripts/elk.py health  <es> <logstash>
    python3 scripts/elk.py search  <es> 'level:ERROR'
    python3 scripts/elk.py trace   <es> <trace_id>
    python3 scripts/elk.py mapping <es>
"""

import json
import sys
import urllib.error
import urllib.parse
import urllib.request

INDEX = "orders-logs-*"

DIM = "\033[2m"
BOLD = "\033[1m"
RED = "\033[31m"
YELLOW = "\033[33m"
GREEN = "\033[32m"
OFF = "\033[0m"

LEVEL_COLOR = {"ERROR": RED, "WARN": YELLOW, "INFO": GREEN, "DEBUG": DIM}


def get(url, timeout=15):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.load(r)


def post(url, body, timeout=15):
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def render(hits):
    if not hits:
        print("  no matching documents")
        return
    for h in hits:
        s = h["_source"]
        lvl = s.get("level", "?")
        col = LEVEL_COLOR.get(lvl, "")
        line = f"  {DIM}{s.get('@timestamp','')[:23]}{OFF} {col}{lvl:<5}{OFF} {s.get('message','')}"

        extras = []
        for k in ("method", "route", "status", "duration_ms", "query", "order_id", "stage"):
            if k in s:
                extras.append(f"{k}={s[k]}")
        if extras:
            line += f"  {DIM}{' '.join(extras)}{OFF}"
        print(line)

        tid = s.get("trace_id")
        if tid:
            print(f"        {DIM}trace_id={tid} request_id={s.get('request_id','-')}{OFF}")
        if "error" in s:
            print(f"        {RED}error={s['error']}{OFF}")


def cmd_search(es, query):
    q = {"match_all": {}} if not query else {
        "query_string": {"query": query, "default_field": "message"}
    }
    body = {
        "size": 20,
        "sort": [{"@timestamp": "desc"}],
        "query": q,
    }
    try:
        res = post(f"{es}/{INDEX}/_search", body)
    except urllib.error.HTTPError as e:
        if e.code == 404:
            print("  no orders-logs-* index yet — has Logstash shipped anything?")
            print("  try: make elk-health")
            return 1
        raise
    total = res["hits"]["total"]["value"]
    print(f"\n  {total} matching document(s), newest 20:\n")
    render(res["hits"]["hits"])
    return 0


def cmd_trace(es, trace_id):
    if not trace_id:
        print("  usage: make logs-trace T=<trace_id>")
        return 2
    body = {
        "size": 100,
        "sort": [{"@timestamp": "asc"}],
        "query": {"term": {"trace_id": trace_id}},
    }
    res = post(f"{es}/{INDEX}/_search", body)
    total = res["hits"]["total"]["value"]
    print(f"\n  trace {trace_id} — {total} log line(s)\n")
    render(res["hits"]["hits"])
    return 0


def cmd_health(es, ls):
    print()
    try:
        h = get(f"{es}/_cluster/health")
        colour = GREEN if h["status"] in ("green", "yellow") else RED
        print(f"  elasticsearch   {colour}{h['status']}{OFF}  "
              f"nodes={h['number_of_nodes']} shards={h['active_shards']}")
    except Exception as e:  # noqa: BLE001
        print(f"  elasticsearch   {RED}unreachable{OFF} ({e})")
        return 1

    # Document count is the only honest answer to "is the pipeline working?".
    # A healthy cluster with an empty index means the shipper is broken.
    try:
        c = get(f"{es}/{INDEX}/_count")
        n = c["count"]
        print(f"  documents       {BOLD}{n}{OFF} in {INDEX}")
    except urllib.error.HTTPError:
        n = 0
        print(f"  documents       {YELLOW}0 — index does not exist yet{OFF}")

    try:
        st = get(f"{ls}/_node/stats")
        ev = st.get("events", {})
        print(f"  logstash        in={ev.get('in',0)} filtered={ev.get('filtered',0)} "
              f"out={ev.get('out',0)}")
        # Events in but nothing out means the pipeline is dropping or stuck.
        if ev.get("in", 0) > 0 and ev.get("out", 0) == 0:
            print(f"  {YELLOW}logstash is reading but not emitting — check the filter/output{OFF}")
    except Exception as e:  # noqa: BLE001
        print(f"  logstash        {RED}unreachable{OFF} ({e})")
        return 1

    if n == 0:
        print(f"\n  {YELLOW}nothing shipped yet{OFF} — generate traffic with `make load`, "
              "then re-run")
    return 0


def cmd_mapping(es):
    """Show the mapping Elasticsearch actually applied.

    Worth checking after any pipeline change. A field that came out as `text`
    when you meant `keyword` costs double the storage and cannot be aggregated;
    one that came out as `keyword` when you meant a number sorts
    lexicographically, where "9" > "100".
    """
    try:
        res = get(f"{es}/{INDEX}/_mapping")
    except urllib.error.HTTPError:
        print("  no orders-logs-* index yet")
        return 1

    for index, body in sorted(res.items()):
        print(f"\n  {BOLD}{index}{OFF}")
        props = body["mappings"].get("properties", {})
        for field, spec in sorted(props.items()):
            t = spec.get("type", "object")
            note = ""
            if t == "text" and field not in ("message", "error", "panic", "stack"):
                note = f"  {YELLOW}← text: doubles storage, cannot aggregate{OFF}"
            if spec.get("index") is False:
                note = f"  {DIM}(stored, not indexed){OFF}"
            print(f"    {field:<20} {t}{note}")
    return 0


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    cmd, es = sys.argv[1], sys.argv[2].rstrip("/")
    arg = sys.argv[3].strip() if len(sys.argv) > 3 else ""

    if cmd == "health":
        return cmd_health(es, arg.rstrip("/") or "http://localhost:9601")
    if cmd == "search":
        return cmd_search(es, arg)
    if cmd == "trace":
        return cmd_trace(es, arg)
    if cmd == "mapping":
        return cmd_mapping(es)
    print(f"unknown command {cmd}")
    return 2


if __name__ == "__main__":
    sys.exit(main())
