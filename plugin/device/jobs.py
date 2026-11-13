"""The computer a scheduled job runs on (docs/browser.md section 3.5).

A job can have a device: each run of the job then runs its commands, file tools and browser on
that computer, in the job's directory there, exactly as a chat bound to it does. The binding is
stored with the job's browser device choice in ``$HERMES_HOME/device/browser.json`` (``jobs``,
with a ``dir``), so one record says where a job runs.

- ``cronjob_manage`` create and update take ``device`` (a name or id, or ``none``) and
  ``device_dir``. A create without ``device`` in a chat bound to one of the user's computers
  gives the job that computer and the chat's directory.
- The user is asked once, when a job gets a device or moves to another one. The approval rule is
  ``browser:job-device:<device id>:<job id>`` (``new`` for a create), so an "always" answer is
  never asked again for the same job and device. Updates, runs and resumes of a job ask nothing.
- A run whose device is offline fails with that reason; it never falls back to the sandbox.
- A run that needs a site approval the user has not given notifies the job's chat once, where
  ``browser_allow_site`` asks the user for exactly that rule.
"""
from __future__ import annotations

import contextvars
import json
import posixpath
import re
import sys
import threading
import time
from typing import Any, Callable, Optional

DEVICE_ARG = "device"
DIR_ARG = "device_dir"
NONE_WORDS = frozenset({"none", "sandbox", "default", "off"})
ALLOW_TOOL = "browser_allow_site"
RULE_RE = re.compile(r"browser:(visit|submit|script|script-write|login|click):[a-z0-9][a-z0-9.-]{0,252}")
MAX_DIR = 4096

_core_ref: Any = None
_browser_ref: Any = None

# The device a running create gives its new job, set by the cronjob_manage handler around
# Hermes's own create and read by the wrapped create_job_with_scheduler_registration.
PENDING: contextvars.ContextVar[Optional[dict]] = contextvars.ContextVar("hermes_device_job_device", default=None)


def _core():
    if _core_ref is None:
        raise RuntimeError("device-jobs used before register()")
    return _core_ref


def _br():
    if _browser_ref is None:
        raise RuntimeError("device-jobs used before register()")
    return _browser_ref


def _label(rec: dict) -> str:
    return f"{rec.get('name') or '?'} [{rec['device_id']}]"


# --- plans -------------------------------------------------------------------------------------

def user_device(user: str, want: str) -> Any:
    """The user's one connected computer whose id or name is ``want`` (never a service device)."""
    core = _core()
    mine = [c for c in core.hub().devices() if c.user == user and c.user != core.SERVICE_USER]
    found = [c for c in mine if c.device_id == want] or [c for c in mine if c.name == want]
    if len(found) != 1:
        names = ", ".join(sorted(f"{c.name} [{c.device_id}]" for c in mine)) or "none"
        raise ValueError(f"{'several' if found else 'none'} of the user's connected computers "
                         f"{'are' if found else 'is'} named {want[:64]!r} (connected: {names}); use the device id")
    return found[0]


def wants_device(args: dict) -> bool:
    return args.get(DEVICE_ARG) is not None or bool(str(args.get(DIR_ARG) or "").strip())


def plan(sid: str, res: Any, args: dict, *, current: Optional[dict] = None, create: bool = False) -> Optional[dict]:
    """The device record that a create or update asks for: ``{device_id, name, user, dir}``, or
    None when the job is to have no device (``device`` none, or a create without ``device`` in
    a chat that no computer of the user serves). ``current`` is the job's record now. Raises
    ValueError or Unavailable for a request the handler refuses."""
    core, br = _core(), _br()
    want = str(args.get(DEVICE_ARG) or "").strip()
    want_dir = str(args.get(DIR_ARG) or "").strip()
    if want.lower() in NONE_WORDS:
        if want_dir:
            raise ValueError("device_dir needs a device")
        return None
    bound = None
    if res.state == core.STATE_BOUND and not res.sandbox and res.binding is not None:
        bound = res.binding
    if want:
        conn = user_device(br._need_user(br._session_user(res)), want)
    elif current is not None:
        conn = core.hub().device(current["device_id"])
        if conn is None:
            raise ValueError(f"the job's device {_label(current)} is not connected, so its directory cannot be "
                             f"changed now")
    elif create and bound is not None and br.cron_job_id(sid):
        return None  # a run creating a job: only the user gives a job a computer
    elif create and bound is not None:
        conn = core.hub().device(bound.device_id)
        if conn is None or conn.user == core.SERVICE_USER:
            return None
    elif create:
        if want_dir:
            raise ValueError("device_dir needs a device: this chat is not bound to a computer")
        return None
    else:
        raise ValueError("device_dir needs a device: the job has none")
    if conn.user != br._need_user(br._session_user(res)):
        raise br.Unavailable(f"talaria: device {conn.name} [{conn.device_id}] belongs to another user")
    if want_dir:
        directory = want_dir
    elif current is not None and current.get("device_id") == conn.device_id and current.get("dir"):
        directory = current["dir"]
    elif bound is not None and bound.device_id == conn.device_id:
        directory = bound.dir
    elif bound is not None and conn.within_roots(bound.dir):
        directory = bound.dir
    elif conn.home and (not conn.roots or conn.within_roots(conn.home)):
        directory = conn.home
    else:
        directory = conn.roots[0] if conn.roots else ""
    if not directory or not posixpath.isabs(directory) or len(directory) > MAX_DIR:
        raise ValueError(f"device_dir must be an absolute path on {conn.name} (got {directory[:200]!r})")
    directory = posixpath.normpath(directory)
    if conn.roots and not conn.within_roots(directory):
        raise ValueError(f"{directory} is outside the allowed roots of {conn.name} ({', '.join(conn.roots)})")
    return {"device_id": conn.device_id, "name": conn.name, "user": conn.user, "dir": directory}


def checked(rec: dict) -> dict:
    """``rec`` with its directory as the device reports it (``pwd -P``). Raises ValueError when
    the device refuses the directory or is offline."""
    core = _core()
    try:
        real = core.device_pwd(core.hub(), rec["device_id"], rec["dir"])
    except core.BindError as exc:
        raise ValueError(f"{rec['dir']} on {rec['name']}: {exc.message}") from None
    return dict(rec, dir=real)


def same(a: Optional[dict], b: Optional[dict]) -> bool:
    if a is None or b is None:
        return a is b
    return a.get("device_id") == b.get("device_id") and (a.get("dir") or "") == (b.get("dir") or "")


def rule_key(device_id: str, job_id: str) -> str:
    return f"browser:job-device:{device_id}:{job_id or 'new'}"


# --- approval gate -----------------------------------------------------------------------------

def gate(tool_name: str, args: dict, session_id: str) -> Optional[dict]:
    """The approve request for giving a job a device or moving it, else None. Updates that leave
    the device alone, runs, resumes and clearing a device ask nothing. Requests the handler
    refuses ask nothing either; the handler reports them."""
    br = _br()
    action = str(args.get("action") or "").strip().lower()
    raw = str(args.get("job_id") or "").strip()
    job_id = ""
    try:
        if tool_name == "browser_use_device":
            want = str(args.get("device") or "").strip()
            if not raw or not want or want == "default":
                return None
            sid, res = br.resolve(session_id)
            job_id = br._job_id(raw)
            if not job_id:
                return None
            br._check_job_owner(job_id, br._need_user(br._session_user(res)))
            current = br.choices().job(job_id)
            conn = [c for c in br.browser_devices(br._need_user(br._session_user(res)))
                    if c.device_id == want] or [c for c in br.browser_devices(br._session_user(res))
                                                 if c.name == want]
            if len(conn) != 1:
                return None
            want_rec = plan(sid, res, {DEVICE_ARG: conn[0].device_id}, current=current)
        elif tool_name == "cronjob_manage" and action == "create":
            sid, res = br.resolve(session_id)
            want_rec = plan(sid, res, args, create=True)
        elif tool_name == "cronjob_manage" and action == "update" and raw and wants_device(args):
            sid, res = br.resolve(session_id)
            job_id = br._job_id(raw)
            if not job_id:
                return None
            br._check_job_owner(job_id, br._need_user(br._session_user(res)))
            current = br.choices().job(job_id)
            want_rec = plan(sid, res, args, current=current)
            if want_rec is None or same(current, want_rec):
                return None
        else:
            return None
    except (br.Unavailable, ValueError):
        return None
    if want_rec is None:
        return None
    what = f"scheduled job {job_id}" if job_id else "the new scheduled job"
    message = (f"Let {what} run on {want_rec['name']} [{want_rec['device_id']}] in {want_rec['dir']}: its commands, "
               f"files and Firefox (with your logins) on that computer, in runs where nobody is watching? It can "
               f"open only the sites you approved with \"always\".")
    if br.no_user():
        return {"action": "block", "message": f"{tool_name} was not run: {message} Only the user can allow this, "
                                              f"in a chat."}
    more = ("Approving always lets every new job created on this computer have it without asking." if not job_id
            else "Approving always is not asked again for this job and computer.")
    return {"action": "approve", "rule_key": rule_key(want_rec["device_id"], job_id), "message": f"{message} {more}"}


# --- the cronjob_manage handler ----------------------------------------------------------------

def _err(message: str) -> str:
    return json.dumps({"success": False, "error": message}, ensure_ascii=False)


def _view(rec: Optional[dict]) -> Optional[dict]:
    return None if rec is None else {"name": rec.get("name"), "device_id": rec["device_id"], "dir": rec.get("dir")}


def _merge(result: Any, **extra: Any) -> Any:
    try:
        data = json.loads(result) if isinstance(result, str) else None
    except ValueError:
        data = None
    if not isinstance(data, dict):
        return result
    data.update(extra)
    return json.dumps(data, ensure_ascii=False)


def make_handler(original: Callable) -> Callable:
    """cronjob_manage with ``device`` and ``device_dir``: Hermes's handler for everything else,
    and the job's device recorded only when Hermes's create or update succeeded."""
    def cronjob_with_device(args, **kw):
        if not isinstance(args, dict):
            return original(args, **kw)
        action = str(args.get("action") or "").strip().lower()
        rest = {k: v for k, v in args.items() if k not in (DEVICE_ARG, DIR_ARG)}
        if action == "create":
            return _create(original, args, rest, kw)
        if action == "update" and wants_device(args):
            return _update(original, args, rest, kw)
        if action == "list":
            return _list(original(rest, **kw))
        return original(rest, **kw)
    return cronjob_with_device


def _session(kw: dict) -> str:
    return _core().current_session_id() or str(kw.get("session_id") or "")


def _create(original: Callable, args: dict, rest: dict, kw: dict) -> Any:
    br = _br()
    explicit = wants_device(args)
    note = ""
    try:
        sid, res = br.resolve(_session(kw))
        rec = plan(sid, res, args, create=True)
        if rec is not None:
            rec = checked(rec)
    except (br.Unavailable, ValueError) as exc:
        if explicit:
            return _err(f"the job was not created: {exc}")
        rec, note = None, f"The job runs in the sandbox: {exc}"
    token = PENDING.set({"rec": rec})
    try:
        out = original(rest, **kw)
    finally:
        PENDING.reset(token)
    extra: dict = {"device": _view(rec)}
    if rec is not None:
        extra["device_note"] = (f"Each run runs its commands, files and browser on {rec['name']} in {rec['dir']}. "
                                f"If that computer is offline at run time, the run reports it and does nothing.")
    else:
        extra["device_note"] = (note or "The job has no computer: its runs use the sandbox.") + (
            " To run it on one of the user's computers, update it with device; the user is asked once.")
    return _merge(out, **extra)


def _update(original: Callable, args: dict, rest: dict, kw: dict) -> Any:
    br = _br()
    try:
        sid, res = br.resolve(_session(kw))
        job_id = br._job_id(str(args.get("job_id") or ""))
        if not job_id:
            return _err(f"no scheduled job has the id or name {str(args.get('job_id'))[:64]!r}")
        br._check_job_owner(job_id, br._need_user(br._session_user(res)))
        current = br.choices().job(job_id)
        rec = plan(sid, res, args, current=current)
        if rec is not None and not same(current, rec):
            rec = checked(rec)
    except (br.Unavailable, ValueError) as exc:
        return _err(f"the job was not changed: {exc}")
    others = {k for k in rest if k not in ("action", "job_id")}
    out = None
    if others:
        out = original(rest, **kw)
        try:
            ok = bool(json.loads(out).get("success"))
        except Exception:
            ok = False
        if not ok:
            return out
    if not same(current, rec):
        br.choices().set_job(job_id, None if rec is None else dict(
            rec, session_id=sid, at=round(time.time(), 3)))
    view = {"device": _view(rec), "device_note": (
        f"Each run runs its commands, files and browser on {rec['name']} in {rec['dir']}." if rec is not None
        else "The job has no device now: its runs use the sandbox and no browser.")}
    if out is None:
        return json.dumps({"success": True, "job_id": job_id, **view}, ensure_ascii=False)
    return _merge(out, **view)


def _list(out: Any) -> Any:
    try:
        data = json.loads(out) if isinstance(out, str) else None
        if not isinstance(data, dict) or not isinstance(data.get("jobs"), list):
            return out
        for job in data["jobs"]:
            if isinstance(job, dict) and job.get("job_id" if "job_id" in job else "id"):
                rec = _br().choices().job(str(job.get("job_id") or job.get("id")))
                if rec is not None:
                    job["device"] = _view(rec)
        return json.dumps(data, ensure_ascii=False)
    except Exception:
        return out


def pending_record(job_id: str, sid: str) -> tuple[bool, Optional[dict]]:
    """``(set, record)``: whether a create through cronjob_manage runs now, and the record its
    new job gets (None for no device)."""
    pending = PENDING.get()
    if pending is None:
        return False, None
    rec = pending.get("rec")
    return True, (None if rec is None else dict(rec, session_id=sid, at=round(time.time(), 3)))


# --- runs ----------------------------------------------------------------------------------------

def resolution(res: Any) -> Any:
    """The resolution of a never-bound session that is a run of a job with a device: bound to
    that device and the job's directory. None when it is not one. A device that is offline or
    belongs to another user now gives a fail-closed resolution, never the sandbox."""
    core, br = _core(), _br()
    sid = res.session_id
    job_id = br.cron_job_id(sid)
    if not job_id:
        return None
    try:
        rec = br.choices().job(job_id)
    except br.Unavailable as exc:
        return core.Resolution(core.STATE_FAIL_CLOSED, sid, reason=f"is a run of scheduled job {job_id}: {exc}")
    if rec is None or not rec.get("dir"):
        return None
    conn = core.hub().device(rec["device_id"])
    if conn is not None and (conn.user != rec.get("user") or conn.user == core.SERVICE_USER):
        return core.Resolution(core.STATE_FAIL_CLOSED, sid,
                               reason=f"is a run of scheduled job {job_id}, whose device {_label(rec)} now belongs "
                                      f"to another user")
    root = res.root or sid
    generation = "job" + core.hashlib.sha256(f"{job_id}\0{rec['device_id']}\0{rec['dir']}".encode()).hexdigest()[:9]
    binding = core.Binding(session_id=root, device_id=rec["device_id"], dir=rec["dir"], generation=generation,
                           bound_at=float(rec.get("at") or 0.0))
    return core.Resolution(core.STATE_BOUND, sid, via=root, binding=binding, root=root,
                           reason=f"scheduled job {job_id} runs on it")


# --- site approvals for runs -------------------------------------------------------------------

def _job_chat(job_id: str) -> str:
    """The chat a job reports to: its origin chat, else the session that gave it its device or
    created it."""
    try:
        from cron.jobs import get_job
        origin = (get_job(job_id) or {}).get("origin") or {}
        if isinstance(origin, dict) and origin.get("platform") == "local" and origin.get("chat_id"):
            return str(origin["chat_id"])
    except Exception:
        _core().logger.debug("device: origin of job %s not readable", job_id, exc_info=True)
    br = _br()
    rec = br.choices().job(job_id) or {}
    return str(rec.get("session_id") or br.choices().job_owner_session(job_id) or "")


def ask_chat(session_id: str, rule: str, tool_name: str) -> str:
    """For a run of a job that needs ``rule``: tell the job's chat once, so the user can allow it
    there. Returns a sentence for the run's block message ("" when nothing was sent)."""
    br = _br()
    job_id = br.cron_job_id(session_id)
    if not job_id or not RULE_RE.fullmatch(rule or ""):
        return ""
    chat = _job_chat(job_id)
    if not chat or not br.choices().mark_ask(job_id, rule):
        return ""
    agents = sys.modules.get("hermes_device_agents")
    if agents is None:
        br.choices().unmark_ask(job_id, rule)
        return ""
    kind, site = rule.split(":", 2)[1:]
    text = (f"Scheduled job {job_id} could not run {tool_name} because the user has not approved \"{kind}\" on "
            f"{site} with \"always\" (rule {rule}), and nobody can answer an approval in a run. Tell the user, "
            f"and if they want it, call {ALLOW_TOOL} with rule \"{rule}\" and job_id \"{job_id}\": it asks the user, "
            f"and an \"Always\" answer lets the job's later runs do it. This message is sent once.")
    caller = agents.caller_of(session_id)

    def send() -> None:
        try:
            agents.BUS.call(agents.OP_SEND, caller, {"to": chat, "text": text})
        except Exception as exc:
            _core().logger.info("device: could not tell chat %s about job %s: %s", chat, job_id, exc)
            try:
                br.choices().unmark_ask(job_id, rule)
            except Exception:
                pass
    threading.Thread(target=send, daemon=True, name=f"device-job-ask-{job_id[:12]}").start()
    return f" The job's chat was told once and can ask the user there with {ALLOW_TOOL}."


def allow_gate(args: dict) -> Optional[dict]:
    br = _br()
    rule = str(args.get("rule") or "").strip()
    if not RULE_RE.fullmatch(rule):
        return None
    job = str(args.get("job_id") or "").strip()[:64]
    kind, site = rule.split(":", 2)[1:]
    if br.no_user():
        return {"action": "block", "message": f"{ALLOW_TOOL} was not run: only the user can allow this, in a chat."}
    what = {"visit": "open", "submit": "submit forms on", "script": "run read-only scripts on",
            "script-write": "run scripts that change data on", "login": "sign in with the saved login on",
            "click": "click on"}[kind]
    return {"action": "approve", "rule_key": rule,
            "message": (f"Let the agent {what} {site} in your Firefox with your logins"
                        f"{f', for scheduled job {job}' if job else ''}? Only \"Always\" also covers scheduled runs, "
                        f"where nobody is watching; it is not asked again.")}


def permanent(rule: str) -> bool:
    try:
        from tools import approval
        with approval._lock:
            return f"plugin_rule:{rule}" in approval._permanent_set()
    except Exception:
        return False


def allow_site(args: dict, task_id: str = "", session_id: str = "", **_kwargs) -> str:
    rule = str((args or {}).get("rule") or "").strip()
    if not RULE_RE.fullmatch(rule):
        return json.dumps({"error": "rule must be a browser rule such as browser:visit:example.com, as the job's "
                                    "message names it", "code": "bad_arguments"})
    always = permanent(rule)
    return json.dumps({"ok": True, "rule": rule, "always": always, "note": (
        "Scheduled runs may do it now." if always else
        "The user did not answer \"Always\", so scheduled runs still cannot do it.")}, ensure_ascii=False)


ALLOW_SCHEMA = {
    "name": ALLOW_TOOL,
    "description": ("Ask the user to allow a browser rule that a scheduled job's run needed (the job's message names "
                    "the rule). The user answers; only \"Always\" lets later runs use it. Example: "
                    "{\"rule\": \"browser:visit:example.com\", \"job_id\": \"abc123\"}"),
    "parameters": {"type": "object", "properties": {
        "rule": {"type": "string", "description": "The rule, such as browser:visit:example.com"},
        "job_id": {"type": "string", "description": "The scheduled job that needs it"}}, "required": ["rule"]},
}

SCHEMA_PROPS = {
    DEVICE_ARG: {"type": "string", "description": (
        "For create/update: the user's computer (name or id) whose terminal, files and Firefox each run uses, as a "
        "chat bound to it does; \"none\" for the sandbox. Default on create: the computer this chat is bound to. "
        "The user is asked once per job and computer.")},
    DIR_ARG: {"type": "string", "description": (
        "For create/update with a device: absolute directory on that computer the runs start in. Default: this "
        "chat's directory, else the device's home.")},
}


def install(registry: Any = None) -> bool:
    """Add ``device`` and ``device_dir`` to cronjob_manage and wrap its handler. Idempotent;
    False when the tool is not registered yet."""
    core = _core()
    if registry is None:
        try:
            from tools.registry import registry
        except Exception:
            return False
    entry = registry.get_entry("cronjob_manage")
    if entry is None:
        return False
    props = ((entry.schema or {}).get("parameters") or {}).get("properties")
    if isinstance(props, dict):
        for k, v in SCHEMA_PROPS.items():
            props.setdefault(k, v)
    try:
        import tools.cronjob_tools as ct
        base = ct.CRONJOB_SCHEMA["parameters"]["properties"]
        for k, v in SCHEMA_PROPS.items():
            base.setdefault(k, v)
    except Exception:
        core.logger.debug("device: CRONJOB_SCHEMA not extended", exc_info=True)
    if getattr(entry.handler, core._WRAPPED_MARK, None) is None:
        original = entry.handler
        wrapped = make_handler(original)
        setattr(wrapped, core._WRAPPED_MARK, original)
        entry.handler = wrapped
    return True


def register(ctx: Any, core: Any, browser: Any) -> None:
    global _core_ref, _browser_ref
    _core_ref, _browser_ref = core, browser
    ctx.register_tool(ALLOW_TOOL, browser.TOOLSET, ALLOW_SCHEMA, allow_site, check_fn=browser.offered,
                      description=ALLOW_SCHEMA["description"], emoji=browser.EMOJI)
    if not install():
        core.logger.info("device: cronjob_manage is not registered yet; its device field is added at session start")
    ctx.register_hook("on_session_start", on_session_start)


def on_session_start(**_kwargs) -> None:
    """Built-in tools can be imported after the plugin; add the device field once they are."""
    try:
        install()
    except Exception:
        _core().logger.warning("device: cronjob_manage device field not installed", exc_info=True)
