"""Hand device commands to the next gateway across a graceful Hermes restart (docs/protocol.md
section 4.7).

Hermes core hands its background processes to the next gateway when ``device`` is in
``terminal.process_handoff_backends`` and the gateway stops for a restart. It calls this module,
registered as the reattach provider ``device``, from a worker thread:

- :func:`prepare` closes every exec leg with ``1001``, so the devices park their commands, and
  waits until the legs have detached. From then on the exec route refuses new legs with ``1013``.
- :func:`detach` takes one adopted device command out of this hub, so this process never fails
  it, and returns its device, id and output offset.
- :func:`commit` writes them to ``$HERMES_HOME/device/handoff.json``.

The next gateway's exec route and :func:`reattach` call :func:`load`, which parks the commands
in the new hub with the rest of the grace, so the device's first connection resumes them.
:func:`reattach` returns the command's new process handle.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import sys
import tempfile
import threading
import time
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("hermes_device")

FILE_NAME = "handoff.json"
FILE_VERSION = 1
CLOSE_HANDOFF = 1001
CLOSE_TRY_AGAIN = 1013
PREPARE_TIMEOUT_S = 5.0
DRAIN_TIMEOUT_S = 2.0
RID_RE = re.compile(r"[0-9a-f]{32}")

# Set by prepare for the rest of this process's life: the exec route refuses new legs.
handing_off = False
_handed: list[dict] = []
# Commands load put back, by request id, until reattach takes them.
_loaded: dict[str, Any] = {}
_lock = threading.Lock()


def _core():
    return sys.modules["hermes_device_core"]


def register(ctx: Any, core: Any) -> None:
    try:
        from tools.process_registry import process_registry
    except ImportError:
        return
    if hasattr(process_registry, "register_reattach_provider"):
        process_registry.register_reattach_provider(core.PLUGIN_ID, sys.modules[__name__])


def _path(hub: Any) -> Optional[Path]:
    return hub._state_path.with_name(FILE_NAME) if hub._state_path else None


def prepare() -> None:
    global handing_off
    core = _core()
    if core.grace_seconds() <= 0:
        return
    hub = core.hub()
    with hub._lock:
        handing_off = True
    closed: set[str] = set()
    deadline = time.monotonic() + PREPARE_TIMEOUT_S
    while True:
        conns = hub.devices()
        if not conns:
            return
        if time.monotonic() >= deadline:
            logger.warning("device: %d exec legs did not close for the handoff", len(conns))
            return
        for conn in conns:
            if conn.conn_id in closed or conn.close is None:
                continue
            closed.add(conn.conn_id)
            try:
                asyncio.run_coroutine_threadsafe(conn.close(CLOSE_HANDOFF, "gateway handoff"), conn.loop)
            except RuntimeError:
                logger.debug("device: exec leg loop of %s is closed", conn.device_id, exc_info=True)
        time.sleep(0.05)


def attach(hub: Any, conn: Any) -> Optional[list[dict]]:
    """``hub.attach(conn)``, after parking what the last gateway handed off; None while this
    gateway hands off, so the exec route refuses the leg before its welcome."""
    load(hub)
    with hub._lock:
        return None if handing_off else hub.attach(conn)


def _take(hub: Any, rid: str, proc: Any) -> Optional[str]:
    """Remove ``proc`` from the hub and return its device id."""
    with hub._lock:
        for device_id, procs in list(hub._parked.items()):
            if procs.get(rid) is not proc:
                continue
            del procs[rid]
            if not procs:
                del hub._parked[device_id]
                timer = hub._park_timers.pop(device_id, None)
                if timer is not None:
                    timer[0].cancel()
            return device_id
        for conn in hub._devices.values():
            with conn.lock:
                if conn.pending.get(rid) is proc:
                    del conn.pending[rid]
                    return conn.device_id
    return None


def _drain(proc: Any) -> None:
    """Wait until the registry's reader has read all output fed so far, so that the offset
    handed off matches the output tail Hermes keeps."""
    try:
        import fcntl
        import termios
    except ImportError:
        return
    deadline = time.monotonic() + DRAIN_TIMEOUT_S
    while time.monotonic() < deadline:
        try:
            buf = bytearray(4)
            fcntl.ioctl(proc.stdout.fileno(), termios.FIONREAD, buf)
            unread = int.from_bytes(buf, sys.byteorder)
        except (OSError, ValueError):
            return
        if proc._queue.empty() and unread == 0:
            return
        time.sleep(0.02)


def detach(session: Any) -> Optional[dict]:
    core = _core()
    proc = getattr(session, "process", None)
    if not handing_off or not isinstance(proc, core.DeviceProcess) or proc.poll() is not None:
        return None
    device_id = _take(core.hub(), proc.request_id, proc)
    if device_id is None:
        return None
    _drain(proc)
    entry = {"device_id": device_id, "rid": proc.request_id, "offset": proc.received, "cwd": proc.cwd}
    with _lock:
        _handed.append(entry)
    logger.info("device: command %s of %s handed to the next gateway at offset %d", proc.request_id, device_id,
                proc.received)
    return dict(entry)


def commit() -> None:
    with _lock:
        entries = list(_handed)
    path = _path(_core().hub())
    if not entries or path is None:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    doc = {"version": FILE_VERSION, "written_at": time.time(), "commands": entries}
    fd, tmp = tempfile.mkstemp(prefix=".handoff-", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(doc, fh)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _entries(raw: Any) -> list[tuple[str, str, int, str]]:
    core = _core()
    if not isinstance(raw, dict) or raw.get("version") != FILE_VERSION:
        raise ValueError("not a version 1 handoff file")
    out = []
    for e in raw.get("commands") or []:
        if not isinstance(e, dict):
            continue
        device_id, rid, offset, cwd = e.get("device_id"), e.get("rid"), e.get("offset"), e.get("cwd")
        if not (isinstance(device_id, str) and core.DEVICE_ID_RE.fullmatch(device_id)
                and isinstance(rid, str) and RID_RE.fullmatch(rid)
                and isinstance(offset, int) and not isinstance(offset, bool) and offset >= 0):
            continue
        out.append((device_id, rid, offset, cwd if isinstance(cwd, str) else ""))
    return out


def load(hub: Any) -> None:
    """Park the commands that the last gateway handed off in ``hub``, once, for what is left of
    the grace. A missing, unreadable or expired file parks nothing, and a gateway that hands off
    never reads its own file."""
    core = _core()
    path = _path(hub)
    if path is None or handing_off:
        return
    with _lock:
        try:
            text = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return
        except OSError as exc:
            logger.error("device: cannot read %s: %s", path, exc)
            return
        try:
            path.unlink()
        except OSError:
            logger.debug("device: cannot remove %s", path, exc_info=True)
        try:
            raw = json.loads(text)
            entries = _entries(raw)
            written = float(raw.get("written_at"))
        except (ValueError, TypeError) as exc:
            logger.error("device: %s ignored: %s", path, exc)
            return
        left = core.grace_seconds() - (time.time() - written)
        if left <= 0:
            if entries:
                logger.warning("device: %d handed-off commands are older than the grace; dropped", len(entries))
            return
        devices: set[str] = set()
        for device_id, rid, offset, cwd in entries:
            proc = core.DeviceProcess(rid, lambda d=device_id, r=rid: hub._kill(d, r),
                                      lambda d=device_id, r=rid: hub._detach_command(d, r))
            proc.received = offset
            proc.cwd = cwd
            _loaded[rid] = proc
            with hub._lock:
                hub._parked.setdefault(device_id, {})[rid] = proc
            devices.add(device_id)
    with hub._lock:
        for device_id in devices:
            old = hub._park_timers.pop(device_id, None)
            if old is not None:
                old[0].cancel()
            token = object()
            timer = threading.Timer(left, hub._expire, args=(device_id, token))
            timer.daemon = True
            hub._park_timers[device_id] = (timer, token)
            timer.start()
    if entries:
        logger.info("device: %d commands handed off by the last gateway wait %.0fs for their devices",
                    len(entries), left)


def reattach(state: dict) -> Any:
    load(_core().hub())
    rid = state.get("rid") if isinstance(state, dict) else None
    with _lock:
        return _loaded.pop(rid, None) if isinstance(rid, str) else None
