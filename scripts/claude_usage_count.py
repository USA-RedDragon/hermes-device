#!/usr/bin/env python3
"""Count Claude Code token usage straight from the transcripts, for checking
the hub's Claude Code panel. It reads only the numeric and id fields of each
line and prints only sums, never message text.

Dedupe follows ccusage's Claude loader: a line with message.id and requestId
counts once across all files; without requestId the key is message.id,
session and, for ids that do not start with msg_ or resp_, the timestamp; a
sidechain copy of a message in the same session is dropped in favor of the
main-thread line. Of duplicates, the line with the most tokens wins.

Usage: claude_usage_count.py [--dir DIR ...] [--range today|7d|30d|mtd]
                             [--model M] [--group account|model|project]

"daily" is the total tokens of each America/Chicago day, as the hub's
chart counts them.
"""
import argparse
import datetime as dt
import json
import os
import re
import sys
from zoneinfo import ZoneInfo

TZ = ZoneInfo("America/Chicago")


def day_range(name, now):
    today = now.astimezone(TZ).date()
    start = {"today": today, "7d": today - dt.timedelta(days=6), "30d": today - dt.timedelta(days=29),
             "mtd": today.replace(day=1)}[name]
    lo = dt.datetime.combine(start, dt.time(), TZ)
    hi = dt.datetime.combine(today + dt.timedelta(days=1), dt.time(), TZ)
    return lo, hi


def parse_ts(s):
    try:
        return dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return None


def files(root):
    proj = os.path.join(root, "projects")
    for dp, _, fs in os.walk(proj):
        for f in sorted(fs):
            if f.endswith(".jsonl"):
                yield os.path.join(dp, f)


def file_session(path):
    parent = os.path.dirname(path)
    if os.path.basename(parent) == "subagents":
        return os.path.basename(os.path.dirname(parent))
    return os.path.basename(path)[:-len(".jsonl")]


def counts(us):
    cc = us.get("cache_creation")
    if isinstance(cc, dict):
        cw = cc.get("ephemeral_5m_input_tokens", 0) + cc.get("ephemeral_1h_input_tokens", 0)
    else:
        cw = us.get("cache_creation_input_tokens", 0) or 0
    return (us["input_tokens"], us["output_tokens"], cw, us.get("cache_read_input_tokens", 0) or 0)


def detail(us):
    """Breaks ties between duplicates with equal totals, as ccusage does."""
    return ({"standard": 1, "fast": 2}.get(us.get("speed"), 0), isinstance(us.get("cache_creation"), dict))


def total(u):
    return u[0] + u[1] + u[2] + u[3]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", action="append")
    ap.add_argument("--range", default="7d", choices=["today", "7d", "30d", "mtd"])
    ap.add_argument("--model")
    ap.add_argument("--group", default="model", choices=["account", "model", "project"])
    a = ap.parse_args()
    dirs = a.dir or [os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")]
    lo, hi = day_range(a.range, dt.datetime.now(dt.timezone.utc))

    owners = {}
    kept = []
    exact = {}
    routes = {}
    stats = {"lines": 0, "kept": 0}

    def push(e):
        mid, rid, sid, ts = e["mid"], e["rid"], e["sid"], e["ts"]
        if mid is None:
            kept.append(e)
            return
        if rid is not None:
            key = ("r", mid, rid)
        elif mid.startswith(("msg_", "resp_")):
            key = ("s", mid, sid)
        else:
            key = ("t", mid, sid, ts)
        idx = exact.get(key)
        if idx is None:
            for j in routes.get((mid, sid), ()):
                x = kept[j]
                if not (e["side"] or x["side"]):
                    continue
                if (rid is None and x["rid"] is None) or x["ts"] == ts:
                    idx = j
                    break
        if idx is None:
            exact[key] = len(kept)
            routes.setdefault((mid, sid), []).append(len(kept))
            kept.append(e)
            return
        x = kept[idx]
        if idx not in routes.setdefault((mid, sid), []):
            routes[(mid, sid)].append(idx)
        if e["side"] != x["side"]:
            replace = x["side"]
        elif total(e["u"]) != total(x["u"]):
            replace = total(e["u"]) > total(x["u"])
        else:
            replace = e["detail"] > x["detail"]
        if replace:
            kept[idx] = e
            exact.setdefault(key, idx)
    for root in dirs:
        for path in files(root):
            with open(path, "rb") as fh:
                for line in fh:
                    if b'"session-owner"' in line:
                        try:
                            d = json.loads(line)
                        except ValueError:
                            continue
                        if d.get("type") == "session-owner" and d.get("sessionId"):
                            owners[d["sessionId"]] = d.get("accountUuid") or ""
                        continue
                    if b'"usage":{' not in line:
                        continue
                    try:
                        d = json.loads(line)
                    except ValueError:
                        continue
                    m = d.get("message")
                    if not isinstance(m, dict) or not isinstance(m.get("usage"), dict):
                        continue
                    us = m["usage"]
                    if type(us.get("input_tokens")) is not int or type(us.get("output_tokens")) is not int:
                        continue
                    ts = parse_ts(d.get("timestamp"))
                    if ts is None:
                        continue
                    if any(d.get(k) == "" for k in ("sessionId", "requestId")) or m.get("id") == "" or m.get("model") == "":
                        continue
                    v = d.get("version")
                    if v is not None and not re.match(r"\d+\.\d+\.\d", v):
                        continue
                    sid = d.get("sessionId")
                    if sid is None:
                        sid = file_session(path)
                    model = m["model"] if m.get("model") is not None else d.get("requestedModel") or ""
                    if model == "<synthetic>":
                        model = ""
                    if us.get("speed") == "fast" and model:
                        model += "-fast"
                    cwd = d.get("cwd") or ""
                    e = {"u": counts(us), "detail": detail(us), "ts": ts, "sid": sid, "model": model,
                         "project": os.path.basename(cwd.rstrip("/")) or "unknown",
                         "side": d.get("isSidechain") is True, "mid": m.get("id"), "rid": d.get("requestId")}
                    stats["lines"] += 1
                    push(e)
                    n = 0
                    for it in us.get("iterations") or []:
                        if not isinstance(it, dict) or it.get("type") != "advisor_message" or not it.get("model") or \
                                type(it.get("input_tokens")) is not int or type(it.get("output_tokens")) is not int:
                            continue
                        x = dict(e, u=counts(it), detail=detail(it), model=it["model"])
                        if x["mid"] is not None:
                            x["mid"] = e["mid"] + ":advisor:" + str(n)
                        n += 1
                        push(x)

    out = {}
    for e in kept:
        if not (lo <= e["ts"] < hi):
            continue
        if a.model and e["model"] != a.model:
            continue
        g = {"account": owners.get(e["sid"], ""), "model": e["model"], "project": e["project"]}[a.group]
        r = out.setdefault(g, {"input": 0, "output": 0, "cache_write": 0, "cache_read": 0, "requests": 0, "daily": {}})
        day = e["ts"].astimezone(TZ).date().isoformat()
        r["daily"][day] = r["daily"].get(day, 0) + sum(e["u"])
        r["input"] += e["u"][0]
        r["output"] += e["u"][1]
        r["cache_write"] += e["u"][2]
        r["cache_read"] += e["u"][3]
        r["requests"] += 1
    stats["kept"] = len(kept)
    json.dump({"range": [lo.isoformat(), hi.isoformat()], "stats": stats, "groups": out}, sys.stdout, indent=1, sort_keys=True)
    print()


if __name__ == "__main__":
    main()
