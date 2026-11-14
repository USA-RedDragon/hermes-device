"""browser_batch: one call runs a list of browser steps and reads the page once at the end.

The steps are the single tools' actions (click, type, press, select, hover, scroll, wait_for,
navigate). The approval rules are the single tools' rules, applied per step, and a batch asks
the user once: ``plan`` (run from the pre_tool_call hook) looks at the steps on the page as it is
now and returns one approval that lists them in plain words. ``run`` (the handler) then checks
every step again on the live page, because the page changes between steps; a step that needs an
approval the batch did not cover stops the batch.

Refs stay usable while the page re-renders. The plugin remembers the role and name of every ref
that a snapshot gave out (``remember``). When a step finds its ref stale, the batch reads the page
again, finds the element again by role, name and position among elements with the same role and
name, and goes on with the new ref. The agent never sees these internal reads.
"""
from __future__ import annotations

import json
import re
import threading
import time
from typing import Any, Callable, Optional
from urllib.parse import urlsplit

ACTIONS = ("click", "type", "press", "select", "hover", "scroll", "wait_for", "navigate")
ALIASES = {"wait": "wait_for", "goto": "navigate", "open": "navigate", "key": "press", "fill": "type"}
MAX_STEPS = 25
MAX_BATCH_S = 240
SNAPSHOT_CHOICES = ("interactive", "outline", "text", "both", "none")

_TARGET = {"ref", "name", "role"}
STEP_KEYS = {
    "click": _TARGET,
    "type": _TARGET | {"text", "submit"},
    "press": _TARGET | {"key"},
    "select": _TARGET | {"value", "text"},
    "hover": _TARGET,
    "scroll": _TARGET | {"direction", "pages", "to_text"},
    "wait_for": {"text", "selector", "seconds"},
    "navigate": {"url"},
}
_REF = re.compile(r"@?(f\d{1,6})?e\d{1,6}")
_STALE = ("stale ref", "bad ref", "unknown frame", "the frame is gone", "unknown or stale")
_LINE = re.compile(r'^\s*- ([A-Za-z]+)(?: ("(?:[^"\\]|\\.)*"))?(.*)\[ref=((?:f\d+)?e\d+)\]\s*$')


class StepError(Exception):
    """A step failed. ``code`` is declined (the user said no), approval (the batch did not cover an
    approval the step needs), gone (the element is no longer on the page) or failed."""

    def __init__(self, message: str, code: str = "failed"):
        super().__init__(message)
        self.code = code


# --- steps -------------------------------------------------------------------------------------

def parse_steps(raw: Any) -> list[dict]:
    """Validated steps from the ``steps`` argument. Raises ValueError that names the step."""
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            raise ValueError("steps must be a list of step objects, such as "
                             '[{"action": "type", "ref": "e1", "text": "hi"}, {"action": "press", "key": "Enter"}]') from None
    if not isinstance(raw, list) or not raw:
        raise ValueError("steps must be a non-empty list of step objects, such as "
                         '[{"action": "type", "ref": "e1", "text": "hi"}, {"action": "press", "key": "Enter"}]')
    if len(raw) > MAX_STEPS:
        raise ValueError(f"at most {MAX_STEPS} steps per batch; split a longer sequence into two calls")
    steps = []
    for i, item in enumerate(raw, 1):
        if not isinstance(item, dict):
            raise ValueError(f"step {i} must be an object with an action, such as {{\"action\": \"click\", \"ref\": \"e3\"}}")
        action = str(item.get("action") or "").strip().lower()
        action = ALIASES.get(action, action)
        if action not in ACTIONS:
            raise ValueError(f"step {i}: action must be one of {', '.join(ACTIONS)}, not {str(item.get('action'))[:30]!r}")
        extra = sorted(k for k in item if k != "action" and k not in STEP_KEYS[action])
        if extra:
            raise ValueError(f"step {i} ({action}): {', '.join(extra)} does not apply to {action}; it takes "
                             f"{', '.join(sorted(STEP_KEYS[action]))}")
        step: dict = {"action": action}
        for key in STEP_KEYS[action]:
            value = item.get(key)
            if value is None or value == "":
                continue
            step[key] = value
        _check_step(i, step)
        steps.append(step)
    return steps


def _check_step(i: int, step: dict) -> None:
    action = step["action"]
    where = f"step {i} ({action})"
    ref = step.get("ref")
    if ref is not None:
        step["ref"] = str(ref).strip()
        if not _REF.fullmatch(step["ref"]):
            raise ValueError(f"{where}: ref must be an element ref from the last browser_snapshot, such as e12 or f1e3")
    if "name" in step:
        step["name"] = " ".join(str(step["name"]).split())
    if "role" in step:
        step["role"] = str(step["role"]).strip().lower()
    if action in ("click", "hover", "type", "select") and not (step.get("ref") or step.get("name")):
        raise ValueError(f"{where}: give ref (from the last snapshot) or name (the element's label on the page)")
    if action == "type":
        if "text" not in step:
            step["text"] = ""
        step["text"] = str(step["text"])
        step["submit"] = step.get("submit") is True or str(step.get("submit")).lower() in ("true", "1", "yes")
    if action == "select":
        value = step.pop("value", step.pop("text", None)) if "value" in step else step.pop("text", None)
        if value is None:
            raise ValueError(f"{where}: value is required (the option's text or value)")
        step["value"] = str(value)
        step.pop("text", None)
    if action == "press":
        key = str(step.get("key") or "").strip()
        if not key or len(key) > 40:
            raise ValueError(f"{where}: key is required, such as Enter, Tab, Escape, ArrowDown or Shift+Tab")
        step["key"] = key
    if action == "scroll":
        direction = str(step.get("direction") or "").strip().lower()
        if direction and direction not in ("down", "up", "top", "bottom"):
            raise ValueError(f"{where}: direction must be down, up, top or bottom")
        if direction:
            step["direction"] = direction
        if not (direction or step.get("ref") or step.get("name") or step.get("to_text")):
            raise ValueError(f"{where}: give direction, ref or name (to bring an element into view), or to_text")
    if action == "wait_for":
        if not (step.get("text") or step.get("selector")):
            raise ValueError(f"{where}: give text or selector to wait for")
        try:
            step["seconds"] = min(max(float(step.get("seconds", 10)), 0.0), 30.0)
        except (TypeError, ValueError):
            raise ValueError(f"{where}: seconds must be a number") from None
    if action == "navigate" and not str(step.get("url") or "").strip():
        raise ValueError(f"{where}: url is required")


def short(text: Any, n: int = 40) -> str:
    text = " ".join(str(text).split())
    return text if len(text) <= n else text[: n - 1] + "..."


def _quote(text: str) -> str:
    return json.dumps(short(text), ensure_ascii=False)


# --- outlines and ref labels -------------------------------------------------------------------

def parse_outline(snapshot: str) -> list[tuple[str, str, str]]:
    """``(role, name, ref)`` of every line of an outline that has a ref."""
    out = []
    for line in str(snapshot or "").splitlines():
        m = _LINE.match(line)
        if not m:
            continue
        name = ""
        if m.group(2):
            try:
                name = json.loads(m.group(2))
            except ValueError:
                name = m.group(2)[1:-1]
        out.append((m.group(1).lower(), " ".join(str(name).split()), m.group(4)))
    return out


def _positions(entries: list[tuple[str, str, str]]) -> dict[str, tuple[str, str, int, Optional[int]]]:
    """ref -> (role, name, nth among the same role and name, how many there are)."""
    seen: dict[tuple[str, str], int] = {}
    totals: dict[tuple[str, str], int] = {}
    for role, name, _ in entries:
        totals[(role, name)] = totals.get((role, name), 0) + 1
    out = {}
    for role, name, ref in entries:
        nth = seen.get((role, name), 0)
        seen[(role, name)] = nth + 1
        out[ref] = (role, name, nth, totals[(role, name)])
    return out


_LABELS: dict[tuple[str, int], dict] = {}
_LABELS_LOCK = threading.Lock()
_MAX_LABELS = 64


# What the agent's refs were before the batch itself read the page. The extension numbers refs
# again on every full read, so once ``plan`` has read the outline (to find a step by name) the
# agent's refs no longer match the page; the runner maps them from these labels instead.
_AGENT_LABELS: dict[tuple[str, int], dict] = {}


def remember(device_id: str, tab: int, url: str, snapshot: str, scoped: bool = False, internal: bool = False) -> None:
    """Keep what the refs of a snapshot are. A full snapshot replaces the refs of the page (the
    extension numbers them again); a scoped one keeps the old refs and adds new ones, whose counts
    are not known (None). ``internal`` is a read the batch made for itself, not one the agent saw."""
    refs = _positions(parse_outline(snapshot))
    with _LABELS_LOCK:
        old = _LABELS.pop((device_id, tab), None)
        if internal:
            if old and old["refs"] and (device_id, tab) not in _AGENT_LABELS:
                _AGENT_LABELS[(device_id, tab)] = old
        else:
            _AGENT_LABELS.pop((device_id, tab), None)
        if scoped and old:
            merged = dict(old["refs"])
            for ref, (role, name, nth, _count) in refs.items():
                merged.setdefault(ref, (role, name, nth, None))
            refs, url = merged, old["url"]
        elif scoped:
            refs = {ref: (r, n, nth, None) for ref, (r, n, nth, _c) in refs.items()}
        _LABELS[(device_id, tab)] = {"url": url, "refs": refs}
        while len(_LABELS) > _MAX_LABELS:
            del _LABELS[next(iter(_LABELS))]


def agent_labels(device_id: str, tab: int) -> Optional[dict]:
    """The labels of the refs the agent holds, when a read of the batch's own has numbered the page again."""
    with _LABELS_LOCK:
        entry = _AGENT_LABELS.get((device_id, tab))
        return {"url": entry["url"], "refs": dict(entry["refs"])} if entry else None


def labels(device_id: str, tab: int) -> dict:
    with _LABELS_LOCK:
        entry = _LABELS.get((device_id, tab))
        return {"url": entry["url"], "refs": dict(entry["refs"])} if entry else {"url": "", "refs": {}}


def page_key(url: str) -> str:
    """The part of a URL that says which page it is: scheme, host and path."""
    parts = urlsplit(url or "")
    return f"{parts.scheme}://{parts.netloc}{parts.path}".rstrip("/")


def rematch(label: tuple, entries: list[tuple[str, str, str]]) -> str:
    """The ref in ``entries`` of the element that ``label`` (role, name, nth, count) described.
    Raises StepError when it is gone or cannot be told from its twins."""
    role, name, nth, count = label
    same = [ref for r, n, ref in entries if r == role and n == name]
    what = f'{role} {_quote(name)}' if name else role
    if not same:
        raise StepError(f"the {what} is no longer on the page", "gone")
    if len(same) == 1 and count in (1, None):
        return same[0]
    if count is not None and len(same) == count:
        return same[nth]
    raise StepError(f"the page has {len(same)} elements like the {what} now, and it had "
                    f"{count if count is not None else 'an unknown number'} before, so the batch cannot tell which one "
                    f"it meant. Use name and role, or take a browser_snapshot", "gone")


def find_named(entries: list[tuple[str, str, str]], name: str, role: str = "") -> str:
    """The ref of the one element whose name is ``name`` (whole name first, then part of it,
    ignoring case). Raises StepError when none or several match."""
    want = " ".join(name.split()).casefold()
    pool = [(r, n, ref) for r, n, ref in entries if not role or r == role]
    hits = [e for e in pool if e[1].casefold() == want] or [e for e in pool if want and want in e[1].casefold()]
    if len(hits) == 1:
        return hits[0][2]
    shown = ", ".join(f"{r} {_quote(n)}" for r, n, _ in (hits or pool)[:8])
    if not hits:
        raise StepError(f"no element named {_quote(name)}{' with role ' + role if role else ''} on the page"
                        f"{'; elements: ' + shown if shown else ''}", "gone")
    raise StepError(f"{len(hits)} elements match {_quote(name)} ({shown}); add role or use a ref", "gone")


# --- words -------------------------------------------------------------------------------------

def target_words(step: dict, label: Optional[tuple] = None) -> str:
    """How a step names its element: the quoted label, else the role, else the ref."""
    name = step.get("name") or (label[1] if label else "")
    role = step.get("role") or (label[0] if label else "")
    if name:
        return _quote(name)
    return f"the {role}" if role else (step.get("ref") or "the focused element")


def step_words(step: dict, label: Optional[tuple] = None, past: bool = False) -> str:
    """One step in plain words, never with typed text (it can be a secret)."""
    action, t = step["action"], target_words(step, label)
    if action == "click":
        return f"{'Clicked' if past else 'Click'} {t}"
    if action == "type":
        base = f"{'Typed into' if past else 'Type into'} {t}"
        return base + (", then pressed Enter" if past else ", then press Enter") if step.get("submit") else base
    if action == "select":
        return f"{'Selected' if past else 'Select'} {_quote(step.get('value', ''))} in {t}"
    if action == "press":
        on = f" on {t}" if step.get("ref") or step.get("name") else ""
        return f"{'Pressed' if past else 'Press'} {step['key']}{on}"
    if action == "hover":
        return f"{'Hovered over' if past else 'Hover over'} {t}"
    if action == "scroll":
        if step.get("to_text"):
            return f"{'Scrolled' if past else 'Scroll'} to {_quote(step['to_text'])}"
        if step.get("direction") and not (step.get("ref") or step.get("name")):
            return f"{'Scrolled' if past else 'Scroll'} {step['direction']}"
        return f"{'Scrolled' if past else 'Scroll'} {t} into view"
    if action == "wait_for":
        what = _quote(step["text"]) if step.get("text") else f"the element {short(step.get('selector'))}"
        return f"{'Waited for' if past else 'Wait for'} {what}"
    host = urlsplit(str(step.get("url") or "")).hostname or short(step.get("url"))
    return f"{'Opened' if past else 'Open'} {host}"


def rule_words(rule_key: str) -> str:
    """What an approval rule lets the agent do, for the batch's one question."""
    _, kind, site = (rule_key.split(":", 2) + ["", ""])[:3]
    return {"submit": f"submit a form to {site}", "visit": f"open {site}",
            "click": f"click, submit and press Enter on {site} (Salesforce)"}.get(kind, rule_key)


# --- the plan: one approval for the whole batch ------------------------------------------------

_SUB_TOOL = {"click": "browser_click", "type": "browser_type", "select": "browser_type", "press": "browser_press",
             "navigate": "browser_navigate"}


def sub_args(step: dict, tab: Optional[int], ref: str = "") -> dict:
    """The arguments the single tool of a step would get, for the approval rules."""
    args: dict = {}
    if tab is not None:
        args["tab_id"] = tab
    if ref:
        args["ref"] = ref
    if step["action"] == "type" and step.get("submit"):
        args["submit"] = True
    if step["action"] == "press":
        args["key"] = step["key"]
    if step["action"] == "navigate":
        args["url"] = step["url"]
    return args


def changes_page(step: dict, grant: dict) -> bool:
    """The step probably loads another page, so the steps after it cannot be checked yet."""
    if step["action"] == "navigate":
        return True
    if grant.get("submit_hosts") or grant.get("visit_hosts"):
        return True
    if step["action"] == "type" and step.get("submit"):
        return True
    if step["action"] == "press":
        return step["key"].strip().lower() in ("enter", "return", "space", "spacebar", " ")
    return False


def plan(b: Any, args: dict, conn: Any) -> tuple[dict, Optional[dict]]:
    """``(grant, approval)`` for browser_batch, as ``decide`` returns them for a single tool.

    Steps are checked on the page as it is now, up to the first one that probably loads another
    page; the rest are checked when they run. All approvals the steps need are asked as one."""
    try:
        steps = parse_steps(args.get("steps"))
    except ValueError:
        return {}, None  # the handler reports it
    tab = b._tab_id(args)
    where = f"the Firefox on device {conn.name}"
    started = time.monotonic()
    budget = max(5.0, b._hook_timeout() / 2 or 15.0)
    pending: list[tuple[int, str]] = []
    confirms: list[int] = []
    entries: Optional[list] = None
    for i, step in enumerate(steps, 1):
        if time.monotonic() - started > budget:
            break
        action = step["action"]
        sub = _SUB_TOOL.get(action)
        if sub is None:
            continue
        ref = step.get("ref", "")
        try:
            if action != "navigate" and tab is None:
                break
            if action != "navigate" and not ref and step.get("name"):
                if entries is None:
                    entries = _outline_entries(b, conn, tab)
                ref = find_named(entries, step["name"], step.get("role", ""))
            if action == "press" and not (ref or step.get("name")):
                ref = ""
            grant, approval = b.decide(sub, sub_args(step, tab, ref), conn)
        except StepError:
            break
        except b._core().BrowserError:
            break
        if approval and approval.get("action") == "block":
            return {}, {"action": "block", "message": f"browser_batch step {i} ({step_words(step)}): {approval['message']}"}
        if approval and approval.get("rule_key") and not b.approved(approval["rule_key"]):
            if approval["rule_key"] not in [k for _, k in pending]:
                pending.append((i, approval["rule_key"]))
        if grant.get("confirm"):
            confirms.append(i)
        if changes_page(step, grant):
            break
    grant = {"batch_rules": [k for _, k in pending]}
    if not pending:
        return grant, None
    lines = [f"{n}. {step_words(s)}" for n, s in enumerate(steps, 1)]
    needs = "; ".join(f"step {i} would {rule_words(k)}" for i, k in pending)
    first = rule_words(pending[0][1])
    tail = (f"Approving for the session (or always) lets the agent {first} again without asking."
            if len(pending) == 1 else
            f"Approving for the session (or always) lets the agent {first} again without asking; the other "
            f"approvals count for this batch only.")
    extra = (f" Steps {', '.join(map(str, confirms))} ask you again when they run (the page has password, payment "
             f"or one-time code fields).") if confirms else ""
    later = (" Steps after one that loads a new page are checked when they run, and the batch stops at one that "
             "needs an approval not listed here.")
    return grant, {"action": "approve", "rule_key": pending[0][1],
                   "message": f"Run {len(steps)} browser steps in {where}:\n" + "\n".join(lines) +
                              f"\nThis needs your OK: {needs}. {tail}{extra}{later}"}


def _outline_entries(b: Any, conn: Any, tab: int, sid: str = "") -> list:
    """The outline of the tab, read for the batch itself (not shown, not cached for offsets)."""
    snap = b.call(conn, "snapshot", {"tab_id": tab, "interactive_only": False, "sessions": b.page_sessions(sid)})
    snap = snap if isinstance(snap, dict) else {}
    text = str(snap.get("snapshot") or "")
    remember(conn.device_id, tab, str(snap.get("url") or ""), text, internal=True)
    return parse_outline(text)


# --- the run -----------------------------------------------------------------------------------

class Runner:
    def __init__(self, b: Any, args: dict, conn: Any, sid: str, steps: list[dict], grant: dict):
        self.b, self.args, self.conn, self.sid, self.steps = b, args, conn, sid, steps
        self.tab = b._tab_id(args)
        self.covered = set(grant.get("batch_rules") or [])
        stored = labels(conn.device_id, self.tab) if self.tab is not None else {"url": "", "refs": {}}
        self.pending_refresh = False
        held = agent_labels(conn.device_id, self.tab) if self.tab is not None else None
        if held:
            stored, self.pending_refresh = held, True
        self.orig: dict[str, tuple] = stored["refs"]
        self.orig_url = stored["url"]
        self.live: dict[str, str] = {}        # the agent's ref -> the ref on the page now
        self.gone: dict[str, str] = {}        # the agent's ref -> why it cannot be found
        self.entries: Optional[list] = None   # the outline read for the batch, while it is current
        self.renumbered = False
        self.opened_tab = False
        self.refreshes = 0
        self.last: dict = {}

    # refs

    def _norm(self, ref: str) -> str:
        return ref.lstrip("@")

    def refresh(self) -> None:
        """Read the page again; the extension numbers the refs again, so map every ref the agent
        was given to its element by role, name and position."""
        if self.refreshes >= 12:
            raise StepError("the page keeps changing under the batch; take a browser_snapshot and try again", "gone")
        self.refreshes += 1
        self.entries = _outline_entries(self.b, self.conn, self.tab, self.sid)
        self.renumbered = True
        now = labels(self.conn.device_id, self.tab)["url"]
        self.live, self.gone = {}, {}
        for ref, label in self.orig.items():
            if self.orig_url and now and page_key(now) != page_key(self.orig_url):
                self.gone[ref] = (f"the page changed from {page_key(self.orig_url)} to {page_key(now)} since the "
                                  f"snapshot that gave {ref}")
                continue
            try:
                self.live[ref] = rematch(label, self.entries)
            except StepError as exc:
                self.gone[ref] = str(exc)

    def locate(self, step: dict, fresh_ok: bool = True) -> str:
        """The ref on the page now for a step's ref or name."""
        name, ref = step.get("name"), step.get("ref")
        if ref:
            ref = self._norm(ref)
            if ref in self.gone:
                raise StepError(self.gone[ref], "gone")
            if ref in self.live:
                return self.live[ref]
            if self.renumbered and ref not in self.orig:
                raise StepError(f"{ref} was not in a snapshot this plugin read, and the page was read again during "
                                f"the batch, so refs may have changed. Use name instead, or take a browser_snapshot "
                                f"first", "gone")
            return ref
        if self.entries is None:
            self.refresh()
        return find_named(self.entries, name, step.get("role", ""))

    # steps

    def probe(self, op_step: dict, ref: str, focus: bool = False) -> Any:
        """The inspect info of an element, or None when it is stale."""
        req: dict = {"tab_id": self.tab}
        if ref:
            req["ref"] = ref
        elif focus:
            req["focus"] = True
        try:
            info = self.b.call(self.conn, "inspect", req)
        except self.b._core().BrowserError as exc:
            if any(m in str(exc).lower() for m in _STALE):
                return None
            raise
        info = info if isinstance(info, dict) else {}
        if info.get("error"):
            if any(m in str(info["error"]).lower() for m in _STALE):
                return None
            raise StepError(str(info["error"]))
        return info

    def allow(self, tool: str, args: dict, info: Optional[dict]) -> dict:
        """Check a step on the live page with the single tool's rules; returns the grant."""
        b = self.b
        grant, approval = b.decide(tool, args, self.conn, inspected=info)
        if approval:
            if approval.get("action") == "block":
                raise StepError(approval["message"])
            key = str(approval.get("rule_key") or "")
            if key not in self.covered and not b.approved(key):
                raise StepError(f"needs the user's approval, which the batch did not ask for because the page was not "
                                f"known when it started ({rule_words(key)}). Run this step with its own tool, then "
                                f"start a new batch", "approval")
        if grant.get("confirm"):
            if b.no_user():
                raise StepError("needs the user's OK on a page with secret fields, and no user is present", "approval")
            if not b._confirm(grant.get("confirm_text") or "Allow this browser action?"):
                raise StepError("the user did not allow this action", "declined")
        return grant

    def element_step(self, step: dict) -> dict:
        action = step["action"]
        focus = action == "press" and not (step.get("ref") or step.get("name"))
        ref = "" if focus else self.locate(step)
        sub = _SUB_TOOL.get(action)
        info = None
        for attempt in (0, 1):
            if action in ("hover", "scroll") or focus and sub is None:
                break
            info = self.probe(step, ref, focus)
            if info is not None:
                break
            orig = self._norm(step.get("ref", ""))
            if attempt or not step.get("ref") or orig not in self.orig:
                raise StepError(f"{step.get('ref')} is stale and was not in a snapshot this plugin read; take a "
                                f"browser_snapshot and use its refs", "gone")
            self.refresh()
            ref = self.locate(step)
        b = self.b
        if action in ("hover", "scroll"):
            return self.plain(step, ref)
        grant = self.allow(sub, sub_args(step, self.tab, ref), info)
        res = self.send_action(step, ref, grant)
        return res

    def send_action(self, step: dict, ref: str, grant: dict) -> dict:
        b, action = self.b, step["action"]
        op = "type" if action == "select" else action
        a = {"tab_id": self.tab, "ref": ref, "key": step.get("key"), "text": step.get("text", ""),
             "submit": step.get("submit")}
        req = b.act_req("press" if action == "press" else "type" if action in ("type", "select") else "click", a)
        if action == "press" and not ref:
            req.pop("ref", None)
        req["guard"] = b.guard_of(grant)
        if action == "select":
            req.pop("text", None)
            req.pop("submit", None)
            req["value"] = step["value"]
            try:
                return b.call(self.conn, "select", req) or {}
            except b._core().BrowserError as exc:
                if "unknown browser operation" not in str(exc) and "unknown op" not in str(exc):
                    raise
                req.pop("value")
                req.update(text=step["value"], submit=False)
                res = dict(b.call(self.conn, "type", req) or {})
                res["note"] = "this device or extension is older than select; the value was set as the option's value"
                return res
        return b.call(self.conn, op, req) or {}

    def plain(self, step: dict, ref: str) -> dict:
        b = self.b
        if step["action"] == "hover":
            return b.call(self.conn, "hover", {"tab_id": self.tab, "ref": ref}) or {}
        a: dict = {"tab_id": self.tab}
        for key in ("direction", "pages", "to_text"):
            if key in step:
                a[key] = step[key]
        if ref:
            a["ref"] = ref
        out = json.loads(b._scroll(a, self.conn, self.sid))
        return out if isinstance(out, dict) else {}

    def wait_step(self, step: dict) -> dict:
        seconds = step["seconds"]
        res = self.b.call(self.conn, "wait", {"tab_id": self.tab, "text": str(step.get("text") or ""),
                                              "selector": str(step.get("selector") or ""),
                                              "timeout_ms": int(seconds * 1000)}, seconds + 30)
        res = res if isinstance(res, dict) else {}
        if not res.get("found"):
            raise StepError(f"{'the text' if step.get('text') else 'the element'} did not show within "
                            f"{seconds:g} seconds")
        return res

    def navigate_step(self, step: dict) -> dict:
        b = self.b
        url = str(step["url"]).strip()
        a: dict = {"url": url}
        if self.tab is not None:
            a["tab_id"] = self.tab
        grant = self.allow("browser_navigate", a, None)
        if self.tab is None:
            b._put_grant(b._grant_key(b._session(self.sid), "browser_navigate", a), grant)
            res = json.loads(b._navigate(a, self.conn, self.sid))
            self.tab = b._tab_id(res)
            self.opened_tab = self.tab is not None and not res.get("reused_tab")
            if self.tab is None:
                raise StepError("the tab could not be opened")
            return res
        return b.call(self.conn, "navigate", {"tab_id": self.tab, "url": url,
                                              "guard": {"visit_hosts": grant.get("visit_hosts") or []}}) or {}

    def run_step(self, step: dict) -> dict:
        action = step["action"]
        if action == "navigate":
            res = self.navigate_step(step)
        elif action == "wait_for":
            res = self.wait_step(step)
        else:
            if self.tab is None:
                raise StepError("no tab: start the batch with a navigate step or give tab_id", "failed")
            res = self.element_step(step)
        if action not in ("hover", "scroll"):
            self.entries = None  # typing, clicking and loading change the page
        if action == "navigate":
            self.orig, self.orig_url, self.live, self.gone, self.renumbered = {}, "", {}, {}, True
        return res if isinstance(res, dict) else {}

    def outcome(self, step: dict, res: dict) -> str:
        label = self.orig.get(self._norm(step["ref"])) if step.get("ref") else None
        text = step_words(step, label, past=True)
        if step["action"] == "press" and res.get("default_action"):
            text += f" ({res['default_action']})"
        if step["action"] == "navigate" and res.get("title"):
            text += f": {short(res['title'], 60)}"
        if step["action"] == "scroll" and res.get("at_bottom"):
            text += " (at the bottom)"
        if res.get("note"):
            text += f" [{res['note']}]"
        return text

    def go(self, stop_on_error: bool) -> tuple[list[str], Optional[dict]]:
        lines: list[str] = []
        failed: Optional[dict] = None
        started = time.monotonic()
        if self.pending_refresh and self.orig:
            try:
                self.refresh()  # plan numbered the page again: map the agent's refs onto the new numbers
            except (StepError, self.b._core().BrowserError):
                pass
        for i, step in enumerate(self.steps, 1):
            label = self.orig.get(self._norm(step["ref"])) if step.get("ref") else None
            if time.monotonic() - started > MAX_BATCH_S:
                lines.append(f"{i}. Not run: the batch ran out of time ({MAX_BATCH_S} seconds)")
                failed = failed or {"step": i, "error": "the batch ran out of time", "code": "timeout"}
                continue
            if failed and stop_on_error:
                lines.append(f"{i}. Not run: {step_words(step, label)}")
                continue
            try:
                res = self.run_step(step)
                self.last = res if res.get("url") else self.last
                lines.append(f"{i}. {self.outcome(step, res)}")
            except StepError as exc:
                lines.append(f"{i}. Failed: {step_words(step, label)}: {exc}")
                failed = failed or {"step": i, "error": str(exc), "code": exc.code}
                if exc.code == "declined":
                    stop_on_error = True
            except self.b._core().BrowserError as exc:
                lines.append(f"{i}. Failed: {step_words(step, label)}: {exc}")
                failed = failed or {"step": i, "error": f"browser: {exc}", "code": exc.code}
                if exc.code in ("not_connected", "timeout", "cancelled"):
                    stop_on_error = True
        return lines, failed


def run(b: Any, args: dict, conn: Any, sid: str) -> str:
    steps = parse_steps(args.get("steps"))
    snap_mode = str(args.get("snapshot") or "interactive").strip().lower()
    if snap_mode not in SNAPSHOT_CHOICES:
        raise ValueError(f"snapshot must be one of {', '.join(SNAPSHOT_CHOICES)}")
    stop = args.get("stop_on_error", True)
    stop = not (stop is False or str(stop).strip().lower() in ("false", "0", "no"))
    if b._tab_id(args) is None and steps[0]["action"] != "navigate":
        raise ValueError("tab_id is required: this session has no agent tab yet. Start the steps with a navigate step "
                         "(it opens a tab), or open one with browser_navigate")
    grant = b._take_grant(b._grant_key(b._session(sid), "browser_batch", args))
    runner = Runner(b, args, conn, sid, steps, grant)
    lines, failed = runner.go(stop)
    out: dict = {"tab_id": runner.tab, "ok": failed is None, "steps": lines,
                 "completed": sum(1 for ln in lines if not ln.split(". ", 1)[1].startswith(("Failed", "Not run")))
                 if lines else 0, "total": len(steps)}
    if runner.opened_tab:
        out["opened_tab"] = True
    if failed:
        out["failed"] = failed
        if failed["code"] == "gone":
            out["next"] = f"Call browser_snapshot with tab_id={runner.tab}, then use its refs or element names."
        elif failed["code"] == "approval":
            out["next"] = "Run the step that needs an approval with its own tool (the user is asked), then continue."
        elif failed["code"] == "declined":
            out["next"] = "The user said no. Do not retry; ask the user how to go on."
    if runner.renumbered and snap_mode == "none":
        out["note"] = "The page was read again during the batch, so refs from before it may point elsewhere. " \
                      "Take a browser_snapshot before you use a ref."
    if snap_mode != "none" and runner.tab is not None and (failed is None or failed.get("code") not in
                                                          ("not_connected", "timeout", "cancelled")):
        read: dict = {"tab_id": runner.tab, "mode": snap_mode}
        for key in ("max_chars", "wait_for", "wait_selector", "wait_seconds"):
            if args.get(key) not in (None, ""):
                read[key] = args[key]
        try:
            page = json.loads(b._snapshot(read, conn, sid))
            page.pop("tab_id", None)
            out["page"] = page
            out["note"] = b.UNTRUSTED_NOTE
        except b._core().BrowserError as exc:
            out["page_error"] = f"the final read failed: {exc}"
        except ValueError as exc:
            out["page_error"] = str(exc)
    return json.dumps(out, ensure_ascii=False)
