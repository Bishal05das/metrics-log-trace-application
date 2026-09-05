#!/usr/bin/env python3
"""Render our JSON log lines as one readable line each.

JSON is right for machines; it is miserable to read in a terminal. This is the
local-development convenience — set LOG_FORMAT=text for the same effect from
the app itself, at the cost of a format your log pipeline cannot parse.
"""
import json
import sys

C = {"DEBUG": "\033[35m", "INFO": "\033[36m", "WARN": "\033[33m", "ERROR": "\033[31m"}
DIM, RESET = "\033[2m", "\033[0m"
SKIP = {"time", "level", "msg", "service", "version", "env"}

for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        d = json.loads(line)
    except ValueError:
        print(line)
        continue

    lvl = d.get("level", "?")
    ts = d.get("time", "")[11:23]
    rest = " ".join(f"{k}={v}" for k, v in d.items() if k not in SKIP)
    print(f"{DIM}{ts}{RESET} {C.get(lvl,'')}{lvl:<5}{RESET} {d.get('msg',''):<24} {DIM}{rest}{RESET}",
          flush=True)
