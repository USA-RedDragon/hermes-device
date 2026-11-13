"""Agent messages: the ``device-agents`` toolset lets a chat or a subagent list its peers and send
one of them a message through the Talaria hub (docs/protocol.md section 12).

The tools never reach another session themselves. A tool call becomes a request on an in-process
bus; the hub takes requests with a long poll on ``GET /agents/requests`` (behind dashboard
sign-in), checks and records them, and answers. The hub delivers a message to a chat with
``prompt.submit``; for a subagent, or for a parent chat whose turn is running, it calls
``POST /agents/deliver``, which steers the child (from its parent only) or puts the text in an
inbox that ``transform_tool_result`` adds to the target's next tool result.

The caller is always the session (and subagent) of the running tool call, taken from the
host-set ``session_id`` keyword and the active agent, never from the model's arguments.
"""
from __future__ import annotations

import json
import logging
import secrets
import sys
import threading
import time
from collections import deque
from typing import Any, Callable, Optional

logger = logging.getLogger("hermes_device")

TOOLSET = "device-agents"
EMOJI = "\U0001f4e8"
# How long a tool call waits for the hub's answer.
ANSWER_TIMEOUT_S = 30.0
# A hub that has not polled for this long counts as not connected.
HUB_STALE_S = 60.0
# Longest long poll the route allows.
MAX_WAIT_S = 25.0
MAX_TEXT = 8000
MAX_NAME = 40
MAX_QUEUE = 64
# Requests one session may have waiting, so one looping agent cannot fill the queue.
MAX_PER_SESSION = 4
# Notices of unread inbox messages, kept apart from tool calls so a full queue never loses one.
MAX_NOTICES = 256
MAX_TAKE = 16
# An inbox message that no tool result picked up within this time goes back to the hub, which
# queues it as a new input of the chat (or records it as not delivered for a subagent).
INBOX_TTL_S = 90.0
MAX_INBOX = 20
MAX_GOAL = 160

OP_PEERS = "peers"
OP_SEND = "send"
OP_NAME = "name"
OP_UNDELIVERED = "undelivered"


class HubUnavailable(Exception):
    pass


class HubRefused(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


class Bus:
    """Requests from tool calls to the hub. Safe for concurrent use."""

    def __init__(self, now: Callable[[], float] = time.monotonic):
        self.now = now
        self._cond = threading.Condition()
        self._queue: deque[dict] = deque()
        self._notices: deque[dict] = deque(maxlen=MAX_NOTICES)
        self._waiting: dict[str, dict] = {}
        self._last_poll: Optional[float] = None

    def connected(self) -> bool:
        with self._cond:
            return self._last_poll is not None and self.now() - self._last_poll <= HUB_STALE_S

    def _push(self, req: dict) -> None:
        sid = req["caller"].get("session_id")
        if len(self._queue) >= MAX_QUEUE or sum(1 for r in self._queue if r["caller"].get("session_id") == sid) \
                >= MAX_PER_SESSION:
            raise HubUnavailable("too many agent message requests are waiting for the Talaria hub")
        self._queue.append(req)
        self._cond.notify_all()

    def call(self, op: str, caller: dict, args: dict, timeout: float = ANSWER_TIMEOUT_S) -> Any:
        if not self.connected():
            raise HubUnavailable("the Talaria hub is not connected to this Hermes, so agent messages are unavailable")
        rid = "ar_" + secrets.token_hex(12)
        slot = {"event": threading.Event(), "answer": None}
        with self._cond:
            self._waiting[rid] = slot
            try:
                self._push({"id": rid, "op": op, "caller": caller, "args": args})
            except HubUnavailable:
                self._waiting.pop(rid, None)
                raise
        got = slot["event"].wait(timeout)
        with self._cond:
            self._waiting.pop(rid, None)
            if not got:
                for i, req in enumerate(self._queue):
                    if req["id"] == rid:
                        del self._queue[i]
                        break
        if not got:
            raise HubUnavailable("the Talaria hub did not answer in time")
        answer = slot["answer"] or {}
        err = answer.get("error")
        if isinstance(err, dict):
            raise HubRefused(str(err.get("code") or "refused"), str(err.get("message") or "refused"))
        return answer.get("result")

    def notify(self, op: str, caller: dict, args: dict) -> None:
        """A request that no tool call waits for. The oldest is dropped past MAX_NOTICES."""
        with self._cond:
            if len(self._notices) == MAX_NOTICES:
                logger.warning("device: an agent message notice was dropped: too many are waiting for the Talaria hub")
            self._notices.append({"id": "ar_" + secrets.token_hex(12), "op": op, "caller": caller, "args": args})
            self._cond.notify_all()

    def take(self, wait: float) -> list[dict]:
        deadline = self.now() + max(0.0, min(wait, MAX_WAIT_S))
        with self._cond:
            self._last_poll = self.now()
            while not self._queue and not self._notices:
                left = deadline - self.now()
                if left <= 0:
                    break
                self._cond.wait(min(left, 1.0))
            out = []
            while self._notices and len(out) < MAX_TAKE:
                out.append(self._notices.popleft())
            while self._queue and len(out) < MAX_TAKE:
                out.append(self._queue.popleft())
            self._last_poll = self.now()
            return out

    def answer(self, rid: str, body: dict) -> bool:
        with self._cond:
            slot = self._waiting.get(rid)
            if slot is None:
                return False
            slot["answer"] = body
            slot["event"].set()
            return True


class Inbox:
    """Messages for running agents, keyed by agent session id, that the next tool result carries."""

    def __init__(self, bus: Bus, now: Callable[[], float] = time.monotonic):
        self.bus = bus
        self.now = now
        self._lock = threading.Lock()
        self._items: dict[str, list[dict]] = {}
        self._running: dict[str, float] = {}

    def turn_started(self, session_id: str) -> None:
        if session_id:
            with self._lock:
                self._running[session_id] = self.now()

    def turn_ended(self, session_id: str) -> None:
        if not session_id:
            return
        with self._lock:
            self._running.pop(session_id, None)
            left = self._items.pop(session_id, [])
        for item in left:
            self._return(session_id, item)

    def running(self, session_id: str) -> bool:
        with self._lock:
            return session_id in self._running

    def put(self, session_id: str, message_id: str, text: str, running_only: bool = False) -> str:
        """Keep text for the session's next tool result: ``queued``, ``full``, or ``not_running``
        when ``running_only`` and no turn of the session runs (checked under the same lock as
        the turn's end, so a message never lands after its turn ended)."""
        with self._lock:
            if running_only and session_id not in self._running:
                return "not_running"
            items = self._items.setdefault(session_id, [])
            if len(items) >= MAX_INBOX:
                return "full"
            items.append({"message_id": message_id, "text": text, "at": self.now()})
            return "queued"

    def take(self, session_id: str) -> list[dict]:
        self.sweep()
        with self._lock:
            return self._items.pop(session_id, [])

    def sweep(self) -> None:
        stale = []
        with self._lock:
            cutoff = self.now() - INBOX_TTL_S
            for sid in list(self._items):
                keep = [i for i in self._items[sid] if i["at"] >= cutoff]
                stale += [(sid, i) for i in self._items[sid] if i["at"] < cutoff]
                if keep:
                    self._items[sid] = keep
                else:
                    del self._items[sid]
        for sid, item in stale:
            self._return(sid, item)

    def _return(self, session_id: str, item: dict) -> None:
        self.bus.notify(OP_UNDELIVERED, {"session_id": session_id},
                        {"message_id": item["message_id"], "session_id": session_id})


BUS = Bus()
INBOX = Inbox(BUS)


# --- the calling agent and its subagent tree ------------------------------------------------------

# Hermes's SubagentLifecycleService (PluginContext.subagent_lifecycle), when it has list_active and
# steer_active (Talaria Hermes patch 0005). Without them the plugin reads tools.delegate_tool_registry.
_LIFECYCLE: Any = None


def _lifecycle(name: str) -> Optional[Callable]:
    fn = getattr(_LIFECYCLE, name, None)
    return fn if callable(fn) else None


def _registry() -> list[dict]:
    """Live subagents: ``{subagent_id, session_id, parent_id, depth, goal, owner, accepting}``."""
    list_active = _lifecycle("list_active")
    if list_active is not None:
        try:
            snapshots = list(list_active())
        except Exception:
            logger.warning("device: subagent_lifecycle.list_active failed", exc_info=True)
            return []
        return [{"subagent_id": str(a.subagent_id), "session_id": str(a.session_id),
                 "parent_id": str(a.parent_subagent_id or ""), "depth": a.depth or 0,
                 "goal": str(a.goal or "")[:MAX_GOAL], "owner": str(a.parent_session_id or ""),
                 "accepting": bool(a.steerable)}
                for a in snapshots if a.session_id and a.subagent_id]
    try:
        from tools import delegate_tool_registry as reg
    except Exception:
        return []
    out = []
    with reg._active_subagents_lock:
        records = list(reg._active_subagents.values())
    for r in records:
        agent = r.get("agent")
        sid = str(getattr(agent, "session_id", "") or "")
        if not sid or not r.get("subagent_id"):
            continue
        out.append({"subagent_id": str(r["subagent_id"]), "session_id": sid,
                    "parent_id": str(r.get("parent_id") or ""), "depth": r.get("depth") or 0,
                    "goal": str(r.get("goal") or "")[:MAX_GOAL], "owner": str(r.get("owner_agent_session_id") or ""),
                    "accepting": bool(r.get("accepting_steer", True))})
    return out


def _steer(subagent_id: str, text: str) -> bool:
    steer_active = _lifecycle("steer_active")
    if steer_active is not None:
        return bool(steer_active(subagent_id, text))
    try:
        from tools.delegate_tool_registry import steer_subagent
    except Exception:
        return False
    return bool(steer_subagent(subagent_id, text))


def _root_of(session_id: str, records: list[dict]) -> str:
    by_sid = {r["session_id"]: r for r in records}
    seen = set()
    sid = session_id
    while sid in by_sid and sid not in seen:
        seen.add(sid)
        sid = by_sid[sid]["owner"] or sid
    return sid


def cron_job_of(session_id: str) -> Optional[dict]:
    """``{id, name}`` of the scheduled job whose run the session belongs to, else None. The job
    shows in the hub as the sender, so a chat reads "cron job <name>" and not an unknown session."""
    try:
        browser = sys.modules.get("hermes_device_browser")
        job_id = browser.cron_job_id(session_id) if browser is not None and session_id else ""
    except Exception:
        logger.debug("device: cron job of %s not readable", session_id, exc_info=True)
        return None
    if not job_id:
        return None
    name = ""
    try:
        from cron.jobs import get_job
        name = str((get_job(job_id) or {}).get("name") or "")
    except Exception:
        logger.debug("device: cron job %s not readable", job_id, exc_info=True)
    return {"id": job_id, "name": name[:MAX_NAME * 2]}


def caller_of(session_id: str) -> dict:
    """The calling chat or subagent, and the live subagents of its tree."""
    records = _registry()
    me = next((r for r in records if r["session_id"] == session_id), None)
    root = _root_of(session_id, records) if session_id else ""
    tree = [{k: r[k] for k in ("subagent_id", "session_id", "parent_id", "depth", "goal")}
            for r in records if r["accepting"] and _root_of(r["session_id"], records) == root]
    out: dict[str, Any] = {"session_id": session_id, "root_session_id": root, "tree": tree}
    if me is not None:
        out["subagent_id"] = me["subagent_id"]
        out["parent_session_id"] = me["owner"]
        out["parent_subagent_id"] = me["parent_id"]
    job = cron_job_of(session_id)
    if job is not None:
        out["cron_job"] = job
    return out


def _current_session(session_id: str) -> str:
    if session_id:
        return session_id
    try:
        from hermes_device_core import current_session_id
        return current_session_id()
    except Exception:
        return ""


# --- tools -----------------------------------------------------------------------------------------

def _ok(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False)


def _err(message: str, code: str) -> str:
    return json.dumps({"error": message, "code": code}, ensure_ascii=False)


def _run(op: str, args: dict, session_id: str) -> str:
    sid = _current_session(session_id)
    if not sid:
        return _err("this tool call has no session", "no_session")
    try:
        result = BUS.call(op, caller_of(sid), args)
    except HubUnavailable as exc:
        return _err(str(exc), "unavailable")
    except HubRefused as exc:
        return _err(exc.message, exc.code)
    return _ok(result if isinstance(result, dict) else {"result": result})


def _peers(args: dict, task_id: str = "", session_id: str = "", **_kwargs) -> str:
    return _run(OP_PEERS, {}, session_id)


def _send(args: dict, task_id: str = "", session_id: str = "", **_kwargs) -> str:
    args = args if isinstance(args, dict) else {}
    to = args.get("to")
    text = args.get("text")
    reply_to = args.get("reply_to")
    if not isinstance(to, str) or not to.strip():
        return _err("to must name a peer: a session id, a chat name or title, a subagent id, or parent", "bad_arguments")
    if not isinstance(text, str) or not text.strip():
        return _err("text must be a non-empty string", "bad_arguments")
    if len(text) > MAX_TEXT:
        return _err(f"text is longer than {MAX_TEXT} characters", "too_long")
    out = {"to": to.strip()[:256], "text": text}
    if isinstance(reply_to, str) and reply_to.strip():
        out["reply_to"] = reply_to.strip()[:64]
    return _run(OP_SEND, out, session_id)


def _name(args: dict, task_id: str = "", session_id: str = "", **_kwargs) -> str:
    args = args if isinstance(args, dict) else {}
    name = args.get("name")
    if not isinstance(name, str):
        return _err("name must be a string; an empty string removes the name", "bad_arguments")
    return _run(OP_NAME, {"name": name.strip()[:MAX_NAME * 2]}, session_id)


SCHEMAS = {
    "agent_peers": {
        "name": "agent_peers",
        "description": (
            "List the other agents you can message with agent_send: the user's other chats on this Hermes "
            "(name, title, computer and folder, and whether each is working now) and the subagents of your own "
            "delegation tree (your parent, children and siblings). Messages go through the Talaria hub, so it does not "
            "matter which computer a chat runs its commands on."),
        "parameters": {"type": "object", "properties": {}, "additionalProperties": False},
    },
    "agent_send": {
        "name": "agent_send",
        "description": (
            "Send a short message to another agent of the same user: another chat, your parent agent, or a "
            "subagent of your tree. It arrives in the target as a message marked as from you, never as the "
            "user's words. An idle chat starts a turn; a busy chat gets it after its current turn; a running "
            "subagent, or your running parent, gets it with its next tool result. Use it to coordinate work, "
            "not to chat: do not send thanks or acknowledgements, and do not answer a message that needs no "
            "answer. Messages are rate limited and a back-and-forth chain is cut off after a few hops."),
        "parameters": {
            "type": "object",
            "properties": {
                "to": {"type": "string", "description": (
                    "The target: a session id or chat name from agent_peers, an exact chat title, a subagent id "
                    "(sa-...), or parent")},
                "text": {"type": "string", "description": f"The message, at most {MAX_TEXT} characters"},
                "reply_to": {"type": "string", "description": "The id (am_...) of the message you answer, if any"},
            },
            "required": ["to", "text"],
            "additionalProperties": False,
        },
    },
    "agent_set_name": {
        "name": "agent_set_name",
        "description": (
            "Give this chat a short name that other agents can use with agent_send, such as build or research. "
            "Letters, digits, dash and underscore, at most 40. An empty name removes it."),
        "parameters": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"],
                       "additionalProperties": False},
    },
}

HANDLERS = {"agent_peers": _peers, "agent_send": _send, "agent_set_name": _name}


def enabled() -> bool:
    try:
        from hermes_device_core import settings
        return settings().get("agent_messages", True) is not False
    except Exception:
        return True


def offered() -> bool:
    """check_fn: the setting is on and a hub polled recently."""
    return enabled() and BUS.connected()


# --- hooks -----------------------------------------------------------------------------------------

def on_turn_start(session_id: str = "", **_kwargs) -> None:
    INBOX.turn_started(session_id)
    return None


def on_turn_end(session_id: str = "", **_kwargs) -> None:
    INBOX.turn_ended(session_id)


def transform_tool_result(tool_name: str = "", result: Any = None, session_id: str = "", **_kwargs) -> Optional[str]:
    note = None
    try:
        browser = sys.modules.get("hermes_device_browser")
        note = browser.schedule_note(tool_name, _kwargs.get("args"), result, session_id) if browser else None
    except Exception:
        logger.debug("device: schedule note failed", exc_info=True)
    if isinstance(note, str):
        result = note
    items = INBOX.take(session_id) if session_id else []
    if not items:
        return note
    base = result if isinstance(result, str) else json.dumps(result, ensure_ascii=False, default=str)
    return base + "".join("\n\n" + i["text"] for i in items)


# --- delivery (POST /agents/deliver) ---------------------------------------------------------------

def deliver(body: dict) -> dict:
    """Deliver a message the hub checked into a running agent of this process."""
    mode = body.get("mode")
    text = body.get("text")
    mid = body.get("message_id")
    sid = body.get("session_id")
    sub = body.get("subagent_id") or ""
    if mode not in ("steer", "inbox") or not isinstance(text, str) or not text.strip() \
            or not isinstance(mid, str) or not mid or not isinstance(sid, str) or not sid or not isinstance(sub, str):
        raise ValueError("the body must be {mode: steer or inbox, message_id, session_id, subagent_id?, text}")
    if len(text) > MAX_TEXT * 2:
        raise ValueError("text is too long")
    live = {r["subagent_id"]: r for r in _registry() if r["accepting"]}
    if sub:
        r = live.get(sub)
        if r is None or r["session_id"] != sid:
            return {"status": "gone"}
    if mode == "steer":
        if not sub:
            raise ValueError("steer needs a subagent_id")
        return {"status": "steered" if _steer(sub, text) else "gone"}
    return {"status": INBOX.put(sid, mid, text, running_only=not sub)}


# Hooks that bracket every turn, also one that fails or is stopped (Talaria Hermes patch 0005).
TURN_HOOKS = ("agent_turn_start", "agent_turn_end")
# The fallback: post_llm_call does not fire when a turn fails or is stopped.
LLM_CALL_HOOKS = ("pre_llm_call", "post_llm_call")


def _make_cron_toolsets(core: Any) -> Callable[[Callable], Callable]:
    def make(original: Callable) -> Callable:
        def _resolve_cron_enabled_toolsets(job: dict, cfg: dict) -> list[str]:
            out = original(job, cfg)
            # A job's toolset list names what the creator remembered; messaging another chat must
            # not depend on remembering this one. An empty list stays the explicit zero-tool
            # allowlist, and plugin setting cron_agents: false turns this off.
            if out and TOOLSET not in out and core.settings().get("cron_agents", True) is not False:
                return [*out, TOOLSET]
            return out
        return _resolve_cron_enabled_toolsets
    return make


CRON_NOTE = (" Scheduled jobs can message another chat: their agent has agent_send and agent_peers whatever "
             "enabled_toolsets says (not for [], the zero-tool allowlist), so a job whose prompt says to tell a "
             "chat needs no extra toolset. A job created in a chat with a browser computer has no browser until "
             "browser_use_device is called with its job_id, which asks the user.")


def install_cron_hooks(core: Any) -> None:
    """Give every scheduled run the agent tools, and say so in the cronjob_manage description."""
    try:
        import cron.scheduler
        core._wrap(cron.scheduler, "_resolve_cron_enabled_toolsets", _make_cron_toolsets(core))
    except Exception:
        logger.error("device: cannot wrap the cron toolset choice; a job needs device-agents in enabled_toolsets "
                     "to message a chat", exc_info=True)
    try:
        from tools.registry import registry
        entry = registry.get_entry("cronjob_manage")
        schema = entry.schema if entry is not None else None
        if isinstance(schema, dict) and CRON_NOTE not in str(schema.get("description") or ""):
            schema["description"] = str(schema.get("description") or "") + CRON_NOTE
    except Exception:
        logger.debug("device: cronjob_manage description not extended", exc_info=True)


def register(ctx: Any, core: Any) -> None:
    global _LIFECYCLE
    install_cron_hooks(core)
    for name, schema in SCHEMAS.items():
        ctx.register_tool(name, TOOLSET, schema, HANDLERS[name], check_fn=offered,
                          description=schema["description"], emoji=EMOJI)
    start, end = TURN_HOOKS if set(TURN_HOOKS) <= core.valid_hooks() else LLM_CALL_HOOKS
    ctx.register_hook(start, on_turn_start)
    ctx.register_hook(end, on_turn_end)
    ctx.register_hook("transform_tool_result", transform_tool_result)
    try:
        lifecycle = getattr(ctx, "subagent_lifecycle", None)
    except Exception:
        logger.debug("device: no subagent_lifecycle", exc_info=True)
        lifecycle = None
    has_api = all(callable(getattr(lifecycle, n, None)) for n in ("list_active", "steer_active"))
    _LIFECYCLE = lifecycle if has_api else None
