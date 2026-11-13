"""Shared core of subagent-routes: the model routes, the per-chat choices and where they are kept.

The routes are ``delegation.backends.model_map.options`` in config.yaml, the same entries the advanced-delegation
plugin's model_map backend reads, plus display fields for hermes-hub (``title``; ``helper: true`` offers the route
as "<name> when stuck"; ``via`` names what serves it in the picker line; ``line`` and ``emphasis`` replace that
line and its bold part; ``auto: false`` keeps Auto from picking it). ``delegation.backends.model_map.guidance``
(``picker``, ``note``, ``description``) says in the deployment's own words how the model should pick in Auto.
Nothing here names a model or a provider. A chat's choice is one of ``auto``, a route name, ``help-<route>`` or ``none``. Choices
live in ``$HERMES_HOME/subagent-routes/choices.json``, keyed by session id. A write goes to the tip of the
session's compression chain, and a read walks from a session up its compression parents, so a choice follows the
chat across context compression. The gateway (routing) and the dashboard (the hub's routes) are separate
processes; both read and write that one file.
"""

from __future__ import annotations

import contextlib
import fcntl
import json
import logging
import os
import threading
import time
from pathlib import Path
from typing import Any, Mapping, Optional

logger = logging.getLogger("subagent_routes")

BACKEND = "model_map"
AUTO, NONE, HELP = "auto", "none", "help-"
ROUTE_FIELDS = ("provider", "model", "base_url", "api_key", "api_mode", "reasoning_effort", "service_tier")
#: How long a helper the router allowed counts as running before its subagent shows in the registry.
HELPER_START_GRACE = 120.0
_LINEAGE_CAP = 64


def _text(entry: Mapping[str, Any], key: str) -> str:
    value = entry.get(key)
    return " ".join(value.split()) if isinstance(value, str) else ""


def options(delegation: Mapping[str, Any]) -> dict[str, dict[str, Any]]:
    """``{name: option}`` from ``delegation.backends.model_map.options`` in config order. An entry without a name,
    description, model, or provider/base_url, or with a repeated name, is skipped."""
    backends = delegation.get("backends") if isinstance(delegation, Mapping) else None
    settings = backends.get(BACKEND) if isinstance(backends, Mapping) else None
    raw = settings.get("options") if isinstance(settings, Mapping) else None
    out: dict[str, dict[str, Any]] = {}
    for entry in raw if isinstance(raw, list) else []:
        if not isinstance(entry, Mapping):
            continue
        name, description, model = _text(entry, "name"), _text(entry, "description"), _text(entry, "model")
        if not (name and description and model and (_text(entry, "provider") or _text(entry, "base_url"))):
            continue
        if name in out or name.startswith(HELP) or name in (AUTO, NONE):
            continue
        route = {k: entry[k].strip() for k in ROUTE_FIELDS if isinstance(entry.get(k), str) and entry[k].strip()}
        if isinstance(entry.get("fallback_providers"), list):
            route["fallback_providers"] = list(entry["fallback_providers"])
        out[name] = {"name": name, "description": description, "route": route, "model": model,
                     "title": _text(entry, "title") or model, "helper": entry.get("helper") is True,
                     "provider": _text(entry, "provider"), "via": _text(entry, "via"),
                     "line": _text(entry, "line"), "emphasis": _text(entry, "emphasis"),
                     "auto": entry.get("auto") is not False}
    return out


def default_route(delegation: Mapping[str, Any], opts: Mapping[str, dict]) -> str:
    backends = delegation.get("backends") if isinstance(delegation, Mapping) else None
    settings = backends.get(BACKEND) if isinstance(backends, Mapping) else None
    value = settings.get("default") if isinstance(settings, Mapping) else None
    value = value.strip() if isinstance(value, str) else ""
    return value if value in opts else ""


GUIDANCE_KEYS = ("picker", "note", "description")


def guidance(delegation: Mapping[str, Any]) -> dict[str, str]:
    """The optional ``delegation.backends.model_map.guidance`` texts, which say how the model should pick a route in
    Auto: ``picker`` (the line under the hub's picker, after "The model picks a route for each subagent:"), ``note``
    (the per-chat note, after "Pick each subagent's `option`:") and ``description`` (a sentence the delegate_task
    description gets). A missing key falls back to a generic text that points at the route descriptions."""
    backends = delegation.get("backends") if isinstance(delegation, Mapping) else None
    settings = backends.get(BACKEND) if isinstance(backends, Mapping) else None
    raw = settings.get("guidance") if isinstance(settings, Mapping) else None
    raw = raw if isinstance(raw, Mapping) else {}
    return {key: _text(raw, key) for key in GUIDANCE_KEYS}


def valid_choice(choice: str, opts: Mapping[str, dict]) -> bool:
    if choice in (AUTO, NONE) or choice in opts:
        return True
    return choice.startswith(HELP) and opts.get(choice[len(HELP):], {}).get("helper") is True


def _short(title: str) -> str:
    return title.split()[0] if title.split() else title


def choices(opts: Mapping[str, dict], texts: Optional[Mapping[str, str]] = None) -> list[dict[str, str]]:
    """What the hub's Subagents picker offers, in order, each with the line it shows under the pickers. ``em`` is
    the part of the line the hub shows in bold. A route's own ``line`` and ``emphasis`` replace both."""
    picker = (texts or {}).get("picker") or "from the route descriptions"
    out = [{"id": AUTO, "label": "Auto (model chooses)", "group": "",
            "line": f"The model picks a route for each subagent: {picker}.", "em": ""}]
    for o in opts.values():
        if o["line"]:
            line, em = o["line"], o["emphasis"]
        elif o["via"]:
            line, em = f"Every subagent runs on {o['title']}, through {o['via']}.", o["title"]
        elif o["provider"]:
            line, em = f"Every subagent runs on {o['title']}, through {o['provider']}.", o["title"]
        else:
            line, em = f"Every subagent runs on {o['title']}.", o["title"]
        out.append({"id": o["name"], "label": o["title"], "group": "Every subagent on", "line": line, "em": em})
    for o in opts.values():
        if o["helper"]:
            out.append({"id": HELP + o["name"], "label": f"{_short(o['title'])} when stuck", "group": "Ask for help",
                        "line": f"Subagents run on the chat's own model; when stuck, it may ask {o['title']} for help, "
                                f"at most one at a time.", "em": o["title"]})
    out.append({"id": NONE, "label": "No subagents", "group": "", "line": "This chat does not start subagents.",
                "em": ""})
    return out


def note(choice: str, opts: Mapping[str, dict], texts: Optional[Mapping[str, str]] = None) -> str:
    """The line the model reads in the user's message while a chat has a choice set. The newest one is the rule."""
    if choice == NONE:
        return ("[Subagents in this chat (this replaces any earlier subagent note): off. The user turned subagents off for this chat, so do not call "
                "delegate_task; do the work yourself.]")
    if choice in opts:
        o = opts[choice]
        return (f"[Subagents in this chat (this replaces any earlier subagent note): every subagent runs on {o['title']} (route `{choice}`), whatever `option` "
                f"says, so leave `option` out.]")
    if choice.startswith(HELP) and choice[len(HELP):] in opts:
        name = choice[len(HELP):]
        return (f"[Subagents in this chat (this replaces any earlier subagent note): subagents run on your own model, so leave `option` out. Only when you "
                f"are stuck, you may start one subagent with option `{name}` ({opts[name]['title']}) to help; only one "
                f"such helper runs at a time.]")
    pick = (texts or {}).get("note") or "by the route descriptions"
    return f"[Subagents in this chat (this replaces any earlier subagent note): auto. Pick each subagent's `option`: {pick}.]"


# --- the choices file ---

_cache_lock = threading.Lock()
_cache: dict[str, Any] = {"key": None, "data": {}}


def store_path() -> Path:
    from hermes_constants import get_hermes_home
    return Path(get_hermes_home()) / "subagent-routes" / "choices.json"


def _read() -> dict[str, dict[str, Any]]:
    path = store_path()
    try:
        st = path.stat()
    except FileNotFoundError:
        return {}
    key = (st.st_mtime_ns, st.st_size, st.st_ino)
    with _cache_lock:
        if _cache["key"] == key:
            return _cache["data"]
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        logger.warning("subagent-routes: %s is unreadable; every chat is on auto", path)
        data = {}
    sessions = data.get("sessions") if isinstance(data, dict) else None
    sessions = {k: v for k, v in (sessions or {}).items() if isinstance(v, dict) and isinstance(v.get("choice"), str)}
    with _cache_lock:
        _cache.update(key=key, data=sessions)
    return sessions


@contextlib.contextmanager
def _locked():
    path = store_path()
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(path.with_suffix(".lock"), "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield path
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _write(session_id: str, choice: Optional[str]) -> None:
    with _locked() as path:
        sessions = dict(_read())
        if choice is None:
            sessions.pop(session_id, None)
        else:
            sessions[session_id] = {"choice": choice, "at": int(time.time())}
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps({"sessions": sessions}, sort_keys=True))
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)


# --- session lineage ---

@contextlib.contextmanager
def _db():
    from hermes_state import SessionDB
    db = SessionDB(read_only=True)
    try:
        yield db
    finally:
        with contextlib.suppress(Exception):
            db.close()


def lineage(session_id: str) -> list[str]:
    """``session_id`` and its compression parents, nearest first (a branch, reset or subagent child stops it)."""
    ids = [session_id] if session_id else []
    if not session_id:
        return ids
    try:
        with _db() as db:
            row = db.get_session(session_id)
            while row and len(ids) < _LINEAGE_CAP and db._is_compression_child_row(row):
                parent = row.get("parent_session_id")
                if not parent or parent in ids:
                    break
                ids.append(parent)
                row = db.get_session(parent)
    except Exception as exc:
        logger.warning("subagent-routes: could not read the lineage of %s: %s", session_id, exc)
    return ids


def tip(session_id: str) -> str:
    try:
        with _db() as db:
            return db.get_compression_tip(session_id) or session_id
    except Exception as exc:
        logger.warning("subagent-routes: could not find the compression tip of %s: %s", session_id, exc)
        return session_id


def stored_choice(session_id: str) -> tuple[Optional[str], list[str]]:
    """The choice set for the chat ``session_id`` belongs to (None when none is) and the chat's ids from its
    compression tip back, the newest choice first."""
    sessions = _read()
    if not session_id:
        return None, []
    if not sessions:
        return None, [session_id]
    ids = lineage(tip(session_id))
    for sid in ids:
        if sid in sessions:
            return sessions[sid]["choice"], ids
    return None, ids


def choice_for(session_id: str, opts: Mapping[str, dict]) -> str:
    choice, _ = stored_choice(session_id)
    return choice if choice and valid_choice(choice, opts) else AUTO


def forget(session_id: str) -> None:
    """Drop a deleted session's choice."""
    if session_id in _read():
        _write(session_id, None)


def set_choice(session_id: str, choice: str) -> str:
    """Keep ``choice`` for the chat ``session_id`` belongs to. Returns the id it was kept at. ``auto`` is kept too, so
    the model reads that the chat went back to auto."""
    at = tip(session_id)
    _write(at, choice)
    return at
