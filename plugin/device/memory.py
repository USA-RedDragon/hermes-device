"""Built-in memory routes for the hub's Memory page: MEMORY.md and USER.md entries and the
memory writes Hermes staged for approval (docs/protocol.md section 9.1).

Every change goes through Hermes's own MemoryStore and write-approval functions, so the char
limits, the MEMORY.md.lock file lock, the threat scan and the entry pinning of staged writes
hold exactly as they do for /memory.
"""
from __future__ import annotations

import logging
import re
from typing import Any, Optional

TARGETS = ("memory", "user")
FILES = {"memory": "MEMORY.md", "user": "USER.md"}
PENDING_ID_RE = re.compile(r"[A-Za-z0-9_-]{1,64}")
MAX_TEXT = 64 * 1024


class MemoryRefused(Exception):
    """A refused memory change: an HTTP status, an error code and a message for the user."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status, self.code, self.message = status, code, message


def _mt():
    from tools import memory_tool
    return memory_tool


def _wa():
    from tools import write_approval
    return write_approval


def _store():
    mt = _mt()
    mt.FAILURE_CLASS.set("other")
    return mt.load_on_disk_store()


def _entries(store, target: str) -> list[str]:
    return list(store.user_entries if target == "user" else store.memory_entries)


def _limit(store, target: str) -> int:
    return int(store.user_char_limit if target == "user" else store.memory_char_limit)


def _text(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _pending_json(rec: dict[str, Any]) -> dict[str, Any]:
    payload = rec.get("payload") if isinstance(rec.get("payload"), dict) else {}
    ops = payload.get("operations") if payload.get("action") == "batch" else [payload]
    changes = []
    for op in ops if isinstance(ops, list) else []:
        if isinstance(op, dict):
            changes.append({"action": _text(op.get("action")),
                            "content": _text(op.get("content")) or _text(op.get("new_text")),
                            "old_text": _text(op.get("old_text")), "entry": _text(op.get("matched_entry"))})
    target = payload.get("target")
    created = rec.get("created_at")
    return {"id": _text(rec.get("id")), "target": target if target in TARGETS else "memory",
            "summary": _text(rec.get("summary")), "origin": _text(rec.get("origin")),
            "created_at": created if isinstance(created, (int, float)) else 0, "changes": changes}


def snapshot() -> dict[str, Any]:
    """Both stores, the approval setting and the pending memory writes."""
    mt, wa = _mt(), _wa()
    store = _store()
    targets = {}
    for t in TARGETS:
        entries = _entries(store, t)
        targets[t] = {"file": str(mt.get_memory_dir() / FILES[t]), "limit": _limit(store, t),
                      "used": len(mt.ENTRY_DELIMITER.join(entries)), "enabled": bool(store.target_enabled(t)),
                      "entries": entries}
    pending = [_pending_json(r) for r in wa.list_pending(wa.MEMORY)]
    return {"approval": bool(wa.write_approval_enabled(wa.MEMORY)), "targets": targets,
            "pending": [p for p in pending if PENDING_ID_RE.fullmatch(p["id"])]}


def _check(target: str, *texts: str):
    if target not in TARGETS:
        raise MemoryRefused(404, "unknown_target", "target must be memory or user")
    for t in texts:
        if not isinstance(t, str) or not t.strip():
            raise MemoryRefused(400, "bad_request", "the text must not be empty")
        if len(t) > MAX_TEXT:
            raise MemoryRefused(413, "too_large", "the text is too long")
    store = _store()
    if not store.target_enabled(target):
        raise MemoryRefused(409, "disabled", f"{FILES[target]} is turned off in Hermes's memory settings")
    return store


def _result(store, target: str, result: dict[str, Any]) -> dict[str, Any]:
    if result.get("success"):
        return snapshot()
    failure = _mt().FAILURE_CLASS.get()
    raise MemoryRefused(*_failure(failure, target, _limit(store, target), _text(result.get("error"))))


def _failure(failure: str, target: str, limit: int, error: str) -> tuple[int, str, str]:
    name = FILES[target]
    if failure in {"over_budget", "retry_cap"}:
        return 409, "over_limit", (f"That would put {name} over its limit of {limit:,} characters. "
                                   "Shorten or delete a note first.")
    if failure in {"stale_entry", "no_match", "ambiguous"}:
        return 409, "changed", f"{name} changed since you opened it. Reload and try again."
    if failure == "scan_blocked":
        return 422, "blocked", f"Hermes refused this text: {error}"
    if failure == "would_empty":
        return 409, "would_empty", f"{name} cannot be emptied in one edit. Delete the last notes one at a time."
    if failure == "disabled":
        return 409, "disabled", f"{name} is turned off in Hermes's memory settings"
    return 409, failure or "refused", error or "Hermes did not accept that change"


def add(target: str, content: str) -> dict[str, Any]:
    store = _check(target, content)
    return _result(store, target, store.add(target, content))


def edit(target: str, entry: str, content: str) -> dict[str, Any]:
    """Replaces exactly the entry the user edited, or refuses when it is gone."""
    store = _check(target, entry, content)
    if content.strip() == entry.strip():
        return snapshot()
    return _result(store, target, store.replace(target, entry, content, entry))


def remove(target: str, entry: str) -> dict[str, Any]:
    store = _check(target, entry)
    return _result(store, target, store.remove(target, entry, entry))


def parse_text(text: str) -> list[str]:
    """Entries of a whole-file edit: lines that hold only § separate entries."""
    entries, cur = [], []
    for line in text.replace("\r\n", "\n").split("\n"):
        if line.strip() == "§":
            entries.append("\n".join(cur).strip())
            cur = []
        else:
            cur.append(line)
    entries.append("\n".join(cur).strip())
    return list(dict.fromkeys(e for e in entries if e))


def replace_all(target: str, text: str, base: list[str]) -> dict[str, Any]:
    """Applies a whole-file edit as one all-or-nothing batch. *base* is the entries the editor
    showed: entries of *base* the text no longer has are removed, pinned to those entries, and
    entries of the text that *base* did not have are added. An entry Hermes wrote meanwhile is
    kept; one it changed or removed meanwhile makes the edit fail with 409 changed. Kept entries
    keep their place; new ones go last."""
    if not isinstance(text, str) or len(text) > MAX_TEXT:
        raise MemoryRefused(413 if isinstance(text, str) else 400, "bad_request", "the text is too long or missing")
    if not isinstance(base, list) or not all(isinstance(e, str) for e in base) or len(base) > 10000:
        raise MemoryRefused(400, "bad_request", 'the body must have "base", the list of entries the editor showed')
    store = _check(target)
    current, new = _entries(store, target), parse_text(text)
    base = [e.strip() for e in base if e.strip()]
    gone = [e for e in base if e not in new]
    ops: list[dict[str, Any]] = [{"action": "remove", "old_text": e, "matched_entry": e} for e in gone]
    ops += [{"action": "add", "content": e} for e in new if e not in base]
    if not ops:
        return snapshot()
    if current and not new and set(current) <= set(gone):
        raise MemoryRefused(*_failure("would_empty", target, _limit(store, target), ""))
    return _result(store, target, store.apply_batch(target, ops))


def _pending_id(pending_id: str) -> str:
    if not isinstance(pending_id, str) or not PENDING_ID_RE.fullmatch(pending_id):
        raise MemoryRefused(404, "unknown_pending", "no such pending memory write")
    return pending_id


def approve(pending_id: str) -> dict[str, Any]:
    """Applies one staged write the way /memory approve does, then drops its record."""
    wa = _wa()
    rec = wa.get_pending(wa.MEMORY, _pending_id(pending_id))
    if not rec:
        raise MemoryRefused(404, "unknown_pending", "no such pending memory write")
    payload = rec.get("payload") if isinstance(rec.get("payload"), dict) else {}
    target = payload.get("target") if payload.get("target") in TARGETS else "memory"
    store = _store()
    try:
        result = _mt().apply_memory_pending(payload, store)
    except Exception as exc:
        logging.getLogger("hermes_device").exception("device: applying pending memory write %s failed", pending_id)
        raise MemoryRefused(500, "apply_failed", "Hermes could not apply this write; see its log") from exc
    if not result.get("success"):
        raise MemoryRefused(*_failure(_mt().FAILURE_CLASS.get(), target, _limit(store, target),
                                    _text(result.get("error"))))
    wa.discard_pending(wa.MEMORY, pending_id)
    return snapshot()


def reject(pending_id: str) -> dict[str, Any]:
    wa = _wa()
    if not wa.discard_pending(wa.MEMORY, _pending_id(pending_id)):
        raise MemoryRefused(404, "unknown_pending", "no such pending memory write")
    return snapshot()


def body_list(body: Optional[dict], key: str) -> list:
    value = body.get(key) if isinstance(body, dict) else None
    if not isinstance(value, list):
        raise MemoryRefused(400, "bad_request", f'the body must be a JSON object with a list "{key}"')
    return value


def body_text(body: Optional[dict], key: str) -> str:
    value = body.get(key) if isinstance(body, dict) else None
    if not isinstance(value, str):
        raise MemoryRefused(400, "bad_request", f'the body must be a JSON object with a string "{key}"')
    return value
