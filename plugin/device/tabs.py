"""Automatic cleanup of the agent's Firefox tabs (docs/browser.md section 3.4).

The model does not have to remember to close tabs. The plugin records which chat (session) and
turn opened each agent tab, and a reaper closes them:

* a tab that is not kept closes ``grace`` seconds after its chat's turn ended, and after the last
  time any tool used it (any use resets the grace);
* a chat that was idle for ``idle`` seconds loses every tab that is not kept; a deleted or
  archived chat loses all of them, kept ones too;
* a kept tab (the agent showed it to the user, it waits on the user, or the agent asked for it)
  stays until the user closes it, the agent releases it, the chat is deleted, or the chat was
  idle for ``keep`` seconds (0: never);
* a subagent's tabs close when the subagent ends, kept ones move to the parent chat;
* a tab with a tool call in flight is never closed, and only tabs this ledger recorded as opened
  by an agent tool are ever closed: user tabs are not known to it, and the extension refuses
  to close a tab outside its tab group anyway.

State is in memory, and in ``$HERMES_HOME/device/tabs.json`` so that tabs of a Hermes process that
died are still closed by the next one. A process adopts only records of dead processes.
"""
from __future__ import annotations

import json
import os
import tempfile
import threading
import time
from pathlib import Path
from typing import Any, Callable, Optional

GRACE_S = 120.0
IDLE_S = 1800.0
KEEP_S = 86400.0
SWEEP_S = 15.0
FAILS_MAX = 20
VERSION = 1


def _alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    return True


class Ledger:
    def __init__(self, path: Optional[Path] = None, clock: Callable[[], float] = time.time,
                 limits: Optional[Callable[[], tuple[float, float, float]]] = None, pid: Optional[int] = None):
        self._path = path
        self._clock = clock
        self._limits = limits or (lambda: (GRACE_S, IDLE_S, KEEP_S))
        self._pid = pid if pid is not None else os.getpid()
        self._lock = threading.RLock()
        self._tabs: dict[tuple[str, int], dict] = {}
        self._sessions: dict[str, dict] = {}
        self._calls: dict[str, int] = {}
        self._tab_calls: dict[tuple[str, int], int] = {}

    # --- recording ------------------------------------------------------------------------------

    def _session(self, sid: str) -> dict:
        # active: None until a turn start or end is seen for the chat. A chat the hooks never named
        # (an older Hermes, or a session id that changed mid-turn) counts as running until idle.
        return self._sessions.setdefault(sid, {"turn": "", "active": None, "seen": self._clock()})

    def opened(self, sid: str, device: str, tab: int, turn: str = "", keep: bool = False, reason: str = "") -> None:
        """An agent tool opened this tab for the chat. A record of the same id is stale (Firefox
        restarted and reused the id) and is replaced."""
        if not sid:
            return
        now = self._clock()
        with self._lock:
            sess = self._session(sid)
            sess["seen"] = now
            self._tabs[(device, tab)] = {
                "session": sid, "turn": turn or sess.get("turn", ""), "opened": now, "used": now,
                "keep": bool(keep), "reason": reason if keep else "", "expire": False, "fails": 0, "pid": self._pid}
            self.save()

    def get(self, device: str, tab: int) -> Optional[dict]:
        with self._lock:
            rec = self._tabs.get((device, tab))
            return dict(rec) if rec else None

    def mine(self, sid: str, device: str, tab: int) -> bool:
        rec = self.get(device, tab)
        return bool(rec and rec["session"] == sid and not rec["expire"])

    def touch(self, sid: str, device: str, tab: Optional[int]) -> None:
        now = self._clock()
        with self._lock:
            if sid:
                self._session(sid)["seen"] = now
            rec = self._tabs.get((device, tab)) if tab is not None else None
            if rec is not None and rec["session"] == sid:
                rec["used"] = now

    def keep(self, sid: str, device: str, tab: int, reason: str = "keep") -> bool:
        with self._lock:
            rec = self._tabs.get((device, tab))
            if rec is None or rec["session"] != sid or rec["expire"]:
                return False
            rec["keep"], rec["reason"], rec["used"] = True, reason, self._clock()
            self.save()
            return True

    def release(self, sid: str, device: str, tab: int) -> bool:
        with self._lock:
            rec = self._tabs.get((device, tab))
            if rec is None or rec["session"] != sid or rec["expire"]:
                return False
            rec["keep"], rec["reason"], rec["used"] = False, "", self._clock()
            self.save()
            return True

    def forget(self, device: str, tab: int) -> None:
        with self._lock:
            if self._tabs.pop((device, tab), None) is not None:
                self.save()

    def tabs_of(self, sid: str) -> list[tuple[str, int]]:
        with self._lock:
            return [k for k, r in self._tabs.items() if r["session"] == sid and not r["expire"]]

    # --- calls in flight ------------------------------------------------------------------------

    def begin_call(self, sid: str, device: str, tab: Optional[int] = None) -> None:
        with self._lock:
            self._calls[sid] = self._calls.get(sid, 0) + 1
            if tab is not None:
                self._tab_calls[(device, tab)] = self._tab_calls.get((device, tab), 0) + 1
            if sid:
                self._session(sid)["seen"] = self._clock()

    def end_call(self, sid: str, device: str, tab: Optional[int] = None) -> None:
        with self._lock:
            n = self._calls.get(sid, 0) - 1
            if n > 0:
                self._calls[sid] = n
            else:
                self._calls.pop(sid, None)
            if tab is not None:
                m = self._tab_calls.get((device, tab), 0) - 1
                if m > 0:
                    self._tab_calls[(device, tab)] = m
                else:
                    self._tab_calls.pop((device, tab), None)
            self.touch(sid, device, tab)

    # --- lifecycle from Hermes hooks ------------------------------------------------------------

    def turn_start(self, sid: str, turn: str = "") -> None:
        if not sid:
            return
        with self._lock:
            sess = self._session(sid)
            sess.update(turn=turn, active=True, seen=self._clock())

    def turn_end(self, sid: str) -> None:
        """The chat's turn ended: the grace of its tabs starts now."""
        if not sid:
            return
        now = self._clock()
        with self._lock:
            sess = self._session(sid)
            sess.update(active=False, seen=now)
            for rec in self._tabs.values():
                if rec["session"] == sid and not rec["keep"]:
                    rec["used"] = max(rec["used"], now)

    def session_gone(self, sids: Any) -> int:
        """Deleted or archived chats: all their tabs, kept ones too, close at the next sweep."""
        ids = {s for s in sids if s}
        n = 0
        with self._lock:
            for rec in self._tabs.values():
                if rec["session"] in ids and not rec["expire"]:
                    rec["expire"] = True
                    n += 1
            for s in ids:
                self._sessions.pop(s, None)
            if n:
                self.save()
        return n

    def child_done(self, child: str, parent: str) -> int:
        """A subagent ended: its tabs close, but a kept one is the user's to see and goes to the parent."""
        n = 0
        with self._lock:
            for rec in self._tabs.values():
                if rec["session"] != child or rec["expire"]:
                    continue
                if rec["keep"] and parent:
                    rec["session"] = parent
                else:
                    rec["expire"] = True
                n += 1
            self._sessions.pop(child, None)
            if n:
                self.save()
        return n

    # --- what is due ----------------------------------------------------------------------------

    def due(self) -> list[tuple[str, int, str]]:
        """``(device, tab, why)`` of the tabs to close now."""
        grace, idle, keep = self._limits()
        now = self._clock()
        out = []
        with self._lock:
            for (device, tab), rec in self._tabs.items():
                sid = rec["session"]
                if self._calls.get(sid) or self._tab_calls.get((device, tab)):
                    continue
                if rec["expire"]:
                    out.append((device, tab, "chat deleted or subagent ended"))
                    continue
                sess = self._sessions.get(sid) or {"active": None, "seen": rec["used"]}
                quiet = now - max(sess["seen"], rec["used"])
                if rec["keep"]:
                    if keep > 0 and quiet >= keep:
                        out.append((device, tab, "kept tab, chat idle"))
                    continue
                if quiet >= idle:
                    out.append((device, tab, "chat idle"))
                elif sess["active"] is False and now - rec["used"] >= grace:
                    out.append((device, tab, "turn ended"))
        return out

    def closed(self, device: str, tab: int) -> None:
        self.forget(device, tab)

    def failed(self, device: str, tab: int) -> None:
        with self._lock:
            rec = self._tabs.get((device, tab))
            if rec is not None:
                rec["fails"] += 1
                if rec["fails"] >= FAILS_MAX:
                    self._tabs.pop((device, tab), None)
                    self.save()

    def count(self) -> int:
        with self._lock:
            return len(self._tabs)

    # --- persistence ----------------------------------------------------------------------------

    def _read(self) -> dict:
        try:
            raw = json.loads(self._path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {}
        if not isinstance(raw, dict) or raw.get("version") != VERSION or not isinstance(raw.get("tabs"), list):
            return {}
        return raw

    def save(self) -> None:
        """Write this process's records, keeping those of other processes."""
        if not self._path:
            return
        import fcntl
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            with open(str(self._path) + ".lock", "a", encoding="utf-8") as lock:
                fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
                others = [t for t in self._read().get("tabs", []) if isinstance(t, dict) and t.get("pid") != self._pid]
                mine = [dict(rec, device=d, tab=t) for (d, t), rec in sorted(self._tabs.items())]
                fd, tmp = tempfile.mkstemp(prefix=".tabs-", suffix=".tmp", dir=str(self._path.parent))
                try:
                    with os.fdopen(fd, "w", encoding="utf-8") as fh:
                        json.dump({"version": VERSION, "tabs": others + mine}, fh)
                    os.chmod(tmp, 0o600)
                    os.replace(tmp, self._path)
                except BaseException:
                    try:
                        os.unlink(tmp)
                    except OSError:
                        pass
                    raise
        except OSError:
            pass

    def adopt(self) -> int:
        """Take over the records of processes that died, so their tabs close. Their grace starts now."""
        if not self._path or not self._path.exists():
            return 0
        import fcntl
        n = 0
        now = self._clock()
        try:
            with open(str(self._path) + ".lock", "a", encoding="utf-8") as lock:
                fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
                kept = []
                for t in self._read().get("tabs", []):
                    if not isinstance(t, dict) or not isinstance(t.get("device"), str) or not isinstance(t.get("tab"), int):
                        continue
                    if t.get("pid") != self._pid and _alive(int(t.get("pid") or 0)):
                        kept.append(t)
                        continue
                    key = (t["device"], t["tab"])
                    with self._lock:
                        if key not in self._tabs:
                            self._tabs[key] = {
                                "session": str(t.get("session") or ""), "turn": str(t.get("turn") or ""),
                                "opened": float(t.get("opened") or now), "used": now, "keep": bool(t.get("keep")),
                                "reason": str(t.get("reason") or ""), "expire": False, "fails": 0, "pid": self._pid}
                            n += 1
                with self._lock:
                    mine = [dict(rec, device=d, tab=t) for (d, t), rec in sorted(self._tabs.items())]
                fd, tmp = tempfile.mkstemp(prefix=".tabs-", suffix=".tmp", dir=str(self._path.parent))
                with os.fdopen(fd, "w", encoding="utf-8") as fh:
                    json.dump({"version": VERSION, "tabs": kept + mine}, fh)
                os.chmod(tmp, 0o600)
                os.replace(tmp, self._path)
        except OSError:
            pass
        return n


class Reaper:
    """Runs :meth:`Ledger.due` on a timer and closes what is due through ``close(device, tab)``."""

    def __init__(self, ledger: Ledger, sweep: Callable[[Ledger], None], interval: float = SWEEP_S):
        self.ledger = ledger
        self._sweep = sweep
        self._interval = interval
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._lock = threading.Lock()

    def start(self) -> None:
        with self._lock:
            if self._thread and self._thread.is_alive():
                return
            self._stop.clear()
            self._thread = threading.Thread(target=self._run, name="talaria-tab-reaper", daemon=True)
            self._thread.start()

    def stop(self) -> None:
        self._stop.set()

    def _run(self) -> None:
        while not self._stop.wait(self._interval):
            try:
                self._sweep(self.ledger)
            except Exception:
                import logging
                logging.getLogger("hermes_device").warning("device-browser: tab cleanup sweep failed", exc_info=True)
