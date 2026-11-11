"""Read-only views of the Hermes session store for the hub: full-text search across chats, token
usage per chat and per model, and the files the agent wrote in a chat (docs/protocol.md section
9.2).

Every read goes through Hermes's own SessionDB: search_messages for the search (FTS5 with its
fallbacks), the compression helpers to find the chat a message belongs to, and plain SELECTs on a
reader connection for the counters.
"""
from __future__ import annotations

import json
import re
import threading
import time
from typing import Any, Optional

# Sources that session_search hides too: background work, not chats (tools/session_search_tool.py).
HIDDEN_SOURCES = ("kanban", "subagent", "tool")
# Searches can fall back to a LIKE scan of every message, so only a few run at once.
MAX_READS = 4
READS = threading.BoundedSemaphore(MAX_READS)
MAX_QUERY = 200
MAX_SEARCH_CHATS = 50
HITS_PER_CHAT = 3
SEARCH_SCAN = 300
MAX_SNIPPET = 400
MAX_USAGE_DAYS = 90
MAX_USAGE_ROWS = 5000
MAX_CHANGED_FILES = 500
MAX_PATH = 4096
WRITE_TOOLS = ("write_file", "patch")
_V4A_FILE = re.compile(r"^\*\*\* (?:Update|Add|Delete) File: (.+)$|^\*\*\* Move to: (.+)$", re.MULTILINE)
_CONTROL = re.compile(r"[\x00-\x08\x0b-\x1f\x7f]")


class Refused(Exception):
    """A refused read: an HTTP status, an error code and a message."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status, self.code, self.message = status, code, message


def _db():
    from hermes_cli.goals import _get_session_db
    db = _get_session_db()
    if db is None:
        raise Refused(503, "store_unavailable", "the Hermes session store is unavailable")
    return db


def _text(value: Any, limit: int) -> str:
    return _CONTROL.sub(" ", value)[:limit] if isinstance(value, str) else ""


def _num(value: Any) -> float:
    return float(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else 0.0


def _int(value: Any) -> int:
    return int(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else 0


class _Tips:
    """The chat each session belongs to: the live tip of its compression chain."""

    def __init__(self, db):
        self.db, self.memo = db, {}

    def __call__(self, sid: str) -> str:
        if sid not in self.memo:
            try:
                self.memo[sid] = self.db.get_compression_tip(sid) or sid
            except Exception:
                self.memo[sid] = sid
        return self.memo[sid]


def search(query: Any, limit: Any = 20, db=None) -> dict:
    """Search the messages of every chat for ``query`` (keywords, "phrases", AND/OR/NOT, prefix*).
    Returns ``{"results": [Chat]}``, best match first. Chat: ``{"session_id", "title", "source",
    "started_at", "last_active", "archived", "hits": [{"row_id", "session_id", "role", "snippet",
    "timestamp"}]}``. ``session_id`` is the chat to open, the tip of the compression chain that
    holds the message; a hit's own ``session_id`` may be an earlier part of it. Snippets mark the
    match with ``>>>`` and ``<<<``. Only user and assistant messages count, as for session_search."""
    q = query.strip() if isinstance(query, str) else ""
    if not q or len(q) > MAX_QUERY:
        raise Refused(400, "bad_query", f"the search must be 1 to {MAX_QUERY} characters")
    try:
        want = max(1, min(int(limit), MAX_SEARCH_CHATS))
    except (TypeError, ValueError):
        want = 20
    db = db or _db()
    rows = db.search_messages(q, exclude_sources=list(HIDDEN_SOURCES), role_filter=["user", "assistant"],
                              limit=SEARCH_SCAN)
    tip = _Tips(db)
    chats: dict[str, dict] = {}
    for r in rows or []:
        sid = r.get("session_id")
        if not isinstance(sid, str) or not sid or not isinstance(r.get("id"), int):
            continue
        chat_id = tip(sid)
        chat = chats.get(chat_id)
        if chat is None:
            if len(chats) >= want:
                continue
            meta = db.get_session(chat_id) or {}
            if meta.get("source") in HIDDEN_SOURCES:
                continue
            chat = chats[chat_id] = {
                "session_id": chat_id, "title": _text(meta.get("title"), 300), "source": _text(meta.get("source"), 40),
                "started_at": _num(meta.get("started_at")),
                "last_active": _num(meta.get("last_activity_at") or meta.get("ended_at") or meta.get("started_at")),
                "archived": bool(meta.get("archived")) or bool(meta.get("hidden")), "hits": []}
        if len(chat["hits"]) < HITS_PER_CHAT:
            chat["hits"].append({"row_id": r["id"], "session_id": sid, "role": _text(r.get("role"), 20),
                                 "snippet": _text(r.get("snippet"), MAX_SNIPPET), "timestamp": _num(r.get("timestamp"))})
    return {"results": list(chats.values())}


def usage(days: Any = 30, db=None, now: Optional[float] = None) -> dict:
    """Token counters of the sessions active in the last ``days`` days, and of each model.

    ``{"since", "sessions": [Session], "models": [Model], "truncated"}``. Session: ``{"session_id",
    "chat_id", "title", "source", "model", "provider", "started_at", "last_active", "input", "output",
    "cache_read", "cache_write", "reasoning", "calls", "cost_usd"}``, newest first; ``chat_id`` is the
    tip of the session's compression chain. Model: ``{"model", "provider", "input", "output",
    "cache_read", "cache_write", "reasoning", "calls", "cost_usd"}`` summed over the session_model_usage
    rows last seen in the window."""
    try:
        n = max(1, min(int(days), MAX_USAGE_DAYS))
    except (TypeError, ValueError):
        n = 30
    db = db or _db()
    since = (now if now is not None else time.time()) - n * 86400
    with db._read_ctx() as conn:
        srows = conn.execute(
            "SELECT id, title, source, model, billing_provider, started_at, "
            "COALESCE(last_activity_at, ended_at, started_at) AS last_active, input_tokens, output_tokens, "
            "cache_read_tokens, cache_write_tokens, reasoning_tokens, api_call_count, "
            "COALESCE(actual_cost_usd, estimated_cost_usd) AS cost FROM sessions "
            "WHERE COALESCE(last_activity_at, ended_at, started_at) >= ? "
            "AND (input_tokens > 0 OR output_tokens > 0 OR api_call_count > 0) "
            "ORDER BY last_active DESC LIMIT ?", (since, MAX_USAGE_ROWS + 1)).fetchall()
        mrows = conn.execute(
            "SELECT model, billing_provider, SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), "
            "SUM(cache_write_tokens), SUM(reasoning_tokens), SUM(api_call_count), "
            "SUM(CASE WHEN actual_cost_usd > 0 THEN actual_cost_usd ELSE estimated_cost_usd END) "
            "FROM session_model_usage WHERE COALESCE(last_seen, first_seen) >= ? "
            "GROUP BY model, billing_provider ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC LIMIT 200",
            (since,)).fetchall()
    tip = _Tips(db)
    sessions = []
    for r in srows[:MAX_USAGE_ROWS]:
        sid = r[0]
        sessions.append({
            "session_id": sid, "chat_id": tip(sid), "title": _text(r[1], 300), "source": _text(r[2], 40),
            "model": _text(r[3], 200), "provider": _text(r[4], 100), "started_at": _num(r[5]),
            "last_active": _num(r[6]), "input": _int(r[7]), "output": _int(r[8]), "cache_read": _int(r[9]),
            "cache_write": _int(r[10]), "reasoning": _int(r[11]), "calls": _int(r[12]), "cost_usd": _num(r[13])})
    models = [{"model": _text(r[0], 200), "provider": _text(r[1], 100), "input": _int(r[2]), "output": _int(r[3]),
               "cache_read": _int(r[4]), "cache_write": _int(r[5]), "reasoning": _int(r[6]), "calls": _int(r[7]),
               "cost_usd": _num(r[8])} for r in mrows]
    return {"since": since, "sessions": sessions, "models": models, "truncated": len(srows) > MAX_USAGE_ROWS}


def _arguments(call: Any) -> tuple[str, dict]:
    fn = call.get("function") if isinstance(call, dict) else None
    if not isinstance(fn, dict):
        return "", {}
    args = fn.get("arguments")
    if isinstance(args, str):
        try:
            args = json.loads(args)
        except ValueError:
            args = None
    return (fn.get("name") if isinstance(fn.get("name"), str) else ""), (args if isinstance(args, dict) else {})


def _written_paths(name: str, args: dict) -> list[str]:
    path = args.get("path")
    if isinstance(path, str) and path.strip():
        return [path.strip()]
    if name == "patch" and isinstance(args.get("patch"), str):
        return [(m.group(1) or m.group(2)).strip() for m in _V4A_FILE.finditer(args["patch"])]
    return []


def changes(session_id: str, db=None) -> dict:
    """The files the agent wrote in a chat with write_file and patch, from the tool calls of every
    part of its compression chain: ``{"session_id", "files": [{"path", "tools", "count",
    "last_at", "row_id"}], "truncated"}``, most recent first. ``path`` is as the agent gave it:
    absolute, ``~/...`` or relative to the chat's folder. It says what the agent asked for, not
    whether the write worked."""
    if not isinstance(session_id, str) or not session_id or len(session_id) > 200:
        raise Refused(400, "bad_session", "bad session id")
    db = db or _db()
    if db.get_session(session_id) is None:
        raise Refused(404, "unknown_session", f"session {session_id} does not exist")
    tip = _Tips(db)(session_id)
    try:
        chain = db.get_compression_lineage(tip) or [tip]
    except Exception:
        chain = [tip]
    files: dict[str, dict] = {}
    with db._read_ctx() as conn:
        for sid in chain:
            rows = conn.execute(
                "SELECT id, timestamp, tool_calls FROM messages WHERE session_id = ? AND role = 'assistant' "
                "AND tool_calls IS NOT NULL AND (tool_calls LIKE '%write\\_file%' ESCAPE '\\' "
                "OR tool_calls LIKE '%patch%') ORDER BY id", (sid,)).fetchall()
            for row_id, ts, raw in rows:
                try:
                    calls = json.loads(raw)
                except (TypeError, ValueError):
                    continue
                for call in calls if isinstance(calls, list) else []:
                    name, args = _arguments(call)
                    if name not in WRITE_TOOLS:
                        continue
                    for p in _written_paths(name, args):
                        if len(p) > MAX_PATH or _CONTROL.search(p):
                            continue
                        f = files.pop(p, None) or {"path": p, "tools": [], "count": 0}
                        if name not in f["tools"]:
                            f["tools"].append(name)
                        f["count"] += 1
                        f["last_at"], f["row_id"] = _num(ts), row_id
                        files[p] = f
    out = list(reversed(files.values()))
    return {"session_id": tip, "files": out[:MAX_CHANGED_FILES], "truncated": len(out) > MAX_CHANGED_FILES}
