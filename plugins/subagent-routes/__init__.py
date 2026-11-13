"""subagent-routes: the model routes a chat's subagents may use, chosen per chat in hermes-hub.

It registers the ``model_map`` delegation backend in place of the advanced-delegation plugin's (which must be
disabled): the same ``delegation.backends.model_map.options`` and the same per-task ``option``, so the
``delegate_task`` schema stays the same for every chat and chats built on the old backend route here at once.
On top of that, each chat has a choice (routes.py) that the hub sets through dashboard/plugin_api.py:

* ``auto``: the task's ``option`` picks the route; without one, ``default`` or the chat's own route.
* a route name: every subagent runs on that route, whatever ``option`` says.
* ``help-<route>``: subagents run on the chat's own route; ``option: <route>`` is accepted for one subagent at a
  time, and any other option is refused.
* ``none``: every task is refused.

Each turn of a chat with a choice gets one line in the user's message (``pre_llm_call``) that says the rule, since
the schema is the same in every chat. The schema is fixed when a chat starts, so a chat that began before a route
existed does not list it; ``route_task`` takes any current route all the same, and such a chat in Auto is told the
current routes once.

With ``plugins.entries.subagent-routes.settings`` ``wait_guidance: true``, every subagent turn, and each chat once,
is told how to wait without sleep polling; with ``sleep_guard: true``, a foreground ``terminal`` command that waits by
sleeping is refused (``pre_tool_call``) with what to do instead (waiting.py).
"""

from __future__ import annotations

import importlib.util
import json
import logging
import sys
import threading
import time
from pathlib import Path
from typing import Any, Mapping, Optional

logger = logging.getLogger("subagent_routes")
_CORE = "hermes_subagent_routes_core"
_WAITING = "hermes_subagent_routes_waiting"
PARAMETER = "option"
HELP_LABEL = " (help)"
ROUTES_MARK = "[Subagent routes now:"
_CTX: Any = None


def _load(name: str, filename: str):
    module = sys.modules.get(name)
    if module is None:
        spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    return module


def _core():
    return _load(_CORE, "routes.py")


def _waiting():
    return _load(_WAITING, "waiting.py")


def _setting(name: str) -> bool:
    try:
        return _CTX is not None and _CTX.get_config(name, False) is True
    except Exception:
        return False


def _session_id() -> str:
    try:
        from gateway.session_context import get_session_env
        return get_session_env("HERMES_SESSION_ID", "") or ""
    except Exception:
        return ""


# --- helpers that are running or starting, per chat ---

_starting_lock = threading.Lock()
_starting: dict[str, float] = {}


def _live_helpers(ids: set[str], label: str) -> int:
    """Running subagents of the chats ``ids`` whose route has ``label``."""
    try:
        from tools import delegate_tool_registry as registry
    except ImportError:
        return 0
    with registry._active_subagents_lock:
        records = list(registry._active_subagents.values())
    count = 0
    for record in records:
        route = getattr(record.get("agent"), "_delegate_route", None)
        if record.get("owner_agent_session_id") in ids and isinstance(route, dict) and route.get("label") == label:
            count += 1
    return count


def _helper_busy(ids: list[str], label: str) -> bool:
    if _live_helpers(set(ids), label):
        with _starting_lock:
            for sid in ids:
                _starting.pop(sid, None)
        return True
    now = time.monotonic()
    with _starting_lock:
        return any(now - _starting.get(sid, -1e9) < _core().HELPER_START_GRACE for sid in ids)


def _helper_started(session_id: str) -> None:
    with _starting_lock:
        _starting[session_id] = time.monotonic()


def _on_subagent_stop(parent_session_id: Any = None, **_: Any) -> None:
    if isinstance(parent_session_id, str):
        with _starting_lock:
            _starting.pop(parent_session_id, None)


def _on_session_delete(session_id: Any = None, **_: Any) -> None:
    if isinstance(session_id, str) and session_id:
        _core().forget(session_id)


def _noted(history: Any, mark: str) -> bool:
    """Whether a user row of ``history`` already carries ``mark`` (context rides its row's ``api_content``)."""
    for message in history if isinstance(history, list) else ():
        if isinstance(message, Mapping) and message.get("role") == "user":
            if any(mark in str(message.get(key) or "") for key in ("api_content", "content")):
                return True
    return False


_listed_lock = threading.Lock()
_listed_cache: dict[str, tuple[float, Optional[list[str]]]] = {}
LISTED_TTL = 600.0


def _listed_routes(session_id: str) -> Optional[list[str]]:
    """``_stored_routes``, kept for ``LISTED_TTL`` per session: a stored schema changes only at compaction."""
    now = time.monotonic()
    with _listed_lock:
        hit = _listed_cache.get(session_id)
        if hit is not None and now - hit[0] < LISTED_TTL:
            return hit[1]
    listed = _stored_routes(session_id)
    with _listed_lock:
        if len(_listed_cache) >= 256:
            _listed_cache.clear()
        _listed_cache[session_id] = (now, listed)
    return listed


def _stored_routes(session_id: str) -> Optional[list[str]]:
    """The ``option`` values this session's stored ``delegate_task`` schema lists (``[]`` without the parameter),
    or None when the session has no stored delegate_task definition."""
    core = _core()
    try:
        with core._db() as db:
            row = db.get_session(session_id) or {}
        pin = json.loads(row.get("tool_names") or "null")
    except Exception:
        return None
    tools = pin.get("tools") if isinstance(pin, dict) else None
    for tool in tools if isinstance(tools, list) else ():
        function = tool.get("function") if isinstance(tool, dict) else None
        if not isinstance(function, dict) or function.get("name") != "delegate_task":
            continue
        try:
            option = function["parameters"]["properties"]["tasks"]["items"]["properties"].get(PARAMETER) or {}
        except (KeyError, TypeError, AttributeError):
            return []
        enum = option.get("enum") if isinstance(option, dict) else None
        return [str(v) for v in enum] if isinstance(enum, list) else []
    return None


def routes_note(listed: list[str], opts: Mapping[str, dict]) -> Optional[str]:
    """For an Auto chat whose stored schema misses current routes: the routes it may pass now."""
    auto = [name for name, o in opts.items() if o["auto"]]
    if not [name for name in auto if name not in listed]:
        return None
    current = ", ".join(f"`{name}` ({opts[name]['title']})" for name in auto)
    old = ", ".join(f"`{name}`" for name in listed) or "none"
    return (f"{ROUTES_MARK} this chat's delegate_task tool was fixed when the chat began and lists {old}, but "
            f"`option` takes any current route: {current}. Each result names the route and model it ran on.]")


def _on_pre_llm_call(session_id: Any = None, platform: Any = "", conversation_history: Any = None,
                     **_: Any) -> Any:
    if not isinstance(session_id, str) or not session_id:
        return None
    waiting = _waiting()
    if platform == "subagent":
        return {"context": waiting.GUIDANCE} if _setting("wait_guidance") else None
    core = _core()
    parts = []
    choice, _ = core.stored_choice(session_id)
    from tools.delegate_tool_config import _load_config
    opts = core.options(_load_config())
    choice = choice if choice and core.valid_choice(choice, opts) else (core.AUTO if choice else None)
    if choice:
        parts.append(core.note(choice, opts, core.guidance(_load_config())))
    if choice in (None, core.AUTO) and opts and not _noted(conversation_history, ROUTES_MARK):
        listed = _listed_routes(session_id)
        note = routes_note(listed, opts) if listed is not None else None
        if note:
            parts.append(note)
    if _setting("wait_guidance") and not waiting.has_guidance(conversation_history):
        parts.append(waiting.GUIDANCE)
    return {"context": "\n".join(parts)} if parts else None


def _on_pre_tool_call(tool_name: Any = None, args: Any = None, **_: Any) -> Any:
    if tool_name != "terminal" or not _setting("sleep_guard"):
        return None
    message = _waiting().refusal(tool_name, args)
    return {"action": "block", "message": message} if message else None


def _provider():
    from agent.delegation_provider import DelegationProvider, DelegationRouteError

    class SubagentRoutesProvider(DelegationProvider):
        """``delegation.backend: model_map`` with a per-chat choice of routes."""

        @property
        def name(self) -> str:
            return _core().BACKEND

        @property
        def display_name(self) -> str:
            return "Model routes (per chat)"

        @property
        def summary(self) -> str:
            return "The agent picks a named model route per subagent, within what each chat allows."

        def task_parameters(self, config: Mapping[str, Any]) -> dict[str, dict[str, Any]]:
            opts = _core().options(config)
            if not opts:
                return {}
            lines = ["Which model route runs this subagent. Omit for the chat's default route.",
                     *(f"{name}: {o['description']}" + ("" if o["auto"] else " Only when the user pins this chat's subagents to it.")
                       for name, o in opts.items())]
            return {PARAMETER: {"type": "string", "enum": list(opts), "description": "\n".join(lines)}}

        def description(self, config: Mapping[str, Any]) -> str:
            if not _core().options(config):
                return ""
            prefer = _core().guidance(config)["description"]
            return ("Each task may set `option` to run that subagent on one of the listed routes. "
                    + (prefer + " " if prefer else "") + "The user can pin a chat's subagents to one route, allow only one "
                    "helper, or turn subagents off; a note in the user's message then says so, and that rule wins.")

        def route_task(self, task: Mapping[str, Any], config: Mapping[str, Any]) -> dict[str, Any]:
            core = _core()
            opts = core.options(config)
            sid = _session_id()
            choice, ids = core.stored_choice(sid)
            if not choice or not core.valid_choice(choice, opts):
                choice = core.AUTO
            chosen = task.get(PARAMETER) or ""
            if choice == core.NONE:
                raise DelegationRouteError(
                    "The user turned subagents off for this chat, so no subagent was started. Do this work yourself.")
            if choice in opts:
                return {**opts[choice]["route"], "label": choice}
            if choice.startswith(core.HELP):
                helper = choice[len(core.HELP):]
                if not chosen:
                    return {}
                if chosen != helper:
                    raise DelegationRouteError(
                        f"In this chat subagents run on your own model: leave `option` out. The only other route is "
                        f"`{helper}`, for one helper when you are stuck.")
                label = helper + HELP_LABEL
                if _helper_busy(ids or [sid], label):
                    raise DelegationRouteError(
                        f"A `{helper}` helper is already running or starting in this chat, and only one runs at a "
                        f"time. Wait for its result, or leave `option` out to use your own model.")
                _helper_started(sid)
                return {**opts[helper]["route"], "label": label}
            if chosen in opts and not opts[chosen]["auto"]:
                allowed = ", ".join(f"`{n}`" for n, o in opts.items() if o["auto"])
                raise DelegationRouteError(
                    f"Route `{chosen}` runs only when the user pins this chat's subagents to it. Pick one of {allowed}, "
                    f"or leave `option` out.")
            if not chosen:
                chosen = core.default_route(config, opts)
            if not chosen:
                return {}
            return {**opts[chosen]["route"], "label": chosen}

    return SubagentRoutesProvider()


def register(ctx) -> None:
    global _CTX
    _CTX = ctx
    try:
        provider = _provider()
    except ModuleNotFoundError as exc:
        if exc.name != "agent.delegation_provider":
            raise
        logger.warning("subagent-routes needs a Hermes with pluggable delegation providers; inactive.")
        return
    ctx.register_delegation_provider(provider)
    ctx.register_hook("pre_llm_call", _on_pre_llm_call)
    ctx.register_hook("pre_tool_call", _on_pre_tool_call)
    ctx.register_hook("subagent_stop", _on_subagent_stop)
    ctx.register_hook("on_session_delete", _on_session_delete)
