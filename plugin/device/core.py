"""Shared state of the device plugin: connected devices, session bindings, the terminal backend.

Loaded once per process under the fixed module name ``hermes_device_core`` so the agent half
(``__init__.py``) and the dashboard route (``dashboard/plugin_api.py``) share one hub.
"""
from __future__ import annotations

import asyncio
import contextvars
import hmac
import json
import logging
import os
import posixpath
import queue
import shlex
import threading
import uuid
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Callable, Optional

logger = logging.getLogger("hermes_device")

PLUGIN_ID = "device"
CLOSE_UNAUTHORIZED = 4401
EXIT_REFUSED = 126
EXIT_KILLED = 130
EXIT_LOST = 255
SEND_TIMEOUT_S = 15.0
HELLO_TIMEOUT_S = 10.0


class DeviceError(Exception):
    """A request the plugin cannot carry out (unknown device, offline, refused dir)."""


# --- settings ---------------------------------------------------------------------------------

def _settings() -> dict[str, Any]:
    """``plugins.entries.device.settings`` from config.yaml, or ``{}``."""
    try:
        from hermes_cli.config import load_config_readonly
        cfg = load_config_readonly() or {}
    except Exception:
        logger.debug("device: config not readable", exc_info=True)
        return {}
    entry = ((cfg.get("plugins") or {}).get("entries") or {}).get(PLUGIN_ID) or {}
    settings = entry.get("settings") if isinstance(entry, dict) else None
    return settings if isinstance(settings, dict) else {}


def configured_devices() -> dict[str, str]:
    """Device name -> shared token. ``token_env`` names an env var holding the token."""
    out: dict[str, str] = {}
    devices = _settings().get("devices") or {}
    if not isinstance(devices, dict):
        return out
    for name, spec in devices.items():
        if not isinstance(spec, dict):
            continue
        token = spec.get("token") or (os.environ.get(spec["token_env"], "") if spec.get("token_env") else "")
        if isinstance(token, str) and len(token) >= 16:
            out[str(name)] = token
        else:
            logger.warning("device: %s has no token of at least 16 characters; ignored", name)
    return out


def placeholder_roots() -> list[str]:
    """Server directories that stand in for the device directory (session cwd, terminal.cwd)."""
    roots = _settings().get("placeholder_roots") or []
    return [os.path.realpath(str(r)) for r in roots if isinstance(r, str) and r]


def _home() -> Path:
    from hermes_constants import get_hermes_home
    return Path(get_hermes_home())


# --- process handle ---------------------------------------------------------------------------

class DeviceProcess:
    """ProcessHandle for one command running on a device. Output arrives from the WebSocket
    thread through :meth:`feed` and is written to a pipe in order by a pump thread, so the base
    class's drain thread reads it as it streams."""

    def __init__(self, request_id: str, kill_fn: Callable[[], bool]):
        self.request_id = request_id
        self._kill_fn = kill_fn
        read_fd, self._write_fd = os.pipe()
        self._stdout = os.fdopen(read_fd, "r", encoding="utf-8", errors="replace")
        self._queue: "queue.SimpleQueue[Any]" = queue.SimpleQueue()
        self._returncode: Optional[int] = None
        self._done = threading.Event()
        self._finished = False
        self.refused = False
        self._lock = threading.Lock()
        threading.Thread(target=self._pump, name=f"device-pump-{request_id[:8]}", daemon=True).start()

    def feed(self, data: str) -> None:
        if data:
            self._queue.put(data)

    def finish(self, code: int, note: str = "") -> None:
        with self._lock:
            if self._finished:
                return
            self._finished = True
        self._queue.put((int(code), note))

    def _write(self, text: str) -> None:
        data = text.encode("utf-8", errors="replace")
        while data:
            try:
                n = os.write(self._write_fd, data)
            except OSError:
                return
            data = data[n:]

    def _pump(self) -> None:
        while True:
            item = self._queue.get()
            if isinstance(item, tuple):
                code, note = item
                if note:
                    self._write(note if note.endswith("\n") else note + "\n")
                try:
                    os.close(self._write_fd)
                except OSError:
                    pass
                self._returncode = code
                self._done.set()
                return
            self._write(item)

    @property
    def stdout(self):
        return self._stdout

    @property
    def returncode(self) -> Optional[int]:
        return self._returncode

    def poll(self) -> Optional[int]:
        return self._returncode if self._done.is_set() else None

    def wait(self, timeout: Optional[float] = None) -> Optional[int]:
        self._done.wait(timeout=timeout)
        return self._returncode

    def kill(self) -> None:
        if not self._kill_fn():
            self.finish(EXIT_KILLED, "[device offline; command abandoned]")


# --- connections and bindings -----------------------------------------------------------------

@dataclass
class DeviceConn:
    """One connected device. ``send`` coroutines run on ``loop`` (the gateway's event loop)."""

    name: str
    send_text: Callable[[str], Any]
    loop: asyncio.AbstractEventLoop
    roots: list[str]
    tmp_dir: str
    hostname: str = ""
    close: Optional[Callable[[], Any]] = None
    conn_id: str = field(default_factory=lambda: uuid.uuid4().hex)
    pending: dict[str, DeviceProcess] = field(default_factory=dict)
    lock: threading.Lock = field(default_factory=threading.Lock)

    def send(self, msg: dict) -> None:
        """Thread-safe send from any thread except the loop's own."""
        try:
            running = asyncio.get_running_loop()
        except RuntimeError:
            running = None
        if running is self.loop:
            raise DeviceError("device send called on the event loop thread")
        fut = asyncio.run_coroutine_threadsafe(self.send_text(json.dumps(msg)), self.loop)
        try:
            fut.result(timeout=SEND_TIMEOUT_S)
        except BaseException:
            # A send that has not started must not go out later with no handle waiting for it.
            fut.cancel()
            raise

    def within_roots(self, path: str) -> bool:
        path = posixpath.normpath(path)
        return any(path == r or path.startswith(r.rstrip("/") + "/") for r in self.roots)


@dataclass
class Binding:
    device: str
    dir: str
    generation: str


class Hub:
    """Process-wide registry of connected devices and session -> (device, dir) bindings."""

    def __init__(self, state_path: Optional[Path] = None):
        self._lock = threading.RLock()
        self._devices: dict[str, DeviceConn] = {}
        self._state_path = state_path
        self._bindings: dict[str, Binding] = self._load()
        self._cwds: dict[str, str] = {}

    # bindings persist so a gateway restart and a later resume keep the device
    def _load(self) -> dict[str, Binding]:
        if not self._state_path or not self._state_path.is_file():
            return {}
        try:
            raw = json.loads(self._state_path.read_text(encoding="utf-8"))
            return {k: Binding(**v) for k, v in (raw.get("bindings") or {}).items()}
        except Exception:
            logger.warning("device: unreadable %s; starting with no bindings", self._state_path)
            return {}

    def _save(self) -> None:
        if not self._state_path:
            return
        self._state_path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self._state_path.with_suffix(".tmp")
        tmp.write_text(json.dumps({"bindings": {k: asdict(v) for k, v in self._bindings.items()}}, indent=2),
                       encoding="utf-8")
        os.replace(tmp, self._state_path)

    # devices
    def attach(self, conn: DeviceConn) -> None:
        with self._lock:
            old = self._devices.get(conn.name)
            self._devices[conn.name] = conn
        if old is not None:
            # Newest connection wins; close the old socket so two machines never share a name.
            logger.warning("device: %s connected again; closing the previous connection", conn.name)
            self._fail_pending(old, "[device reconnected; command lost]")
            if old.close is not None:
                asyncio.run_coroutine_threadsafe(old.close(), old.loop)
        logger.info("device: %s connected (%s) roots=%s", conn.name, conn.hostname, conn.roots)

    def detach(self, conn: DeviceConn) -> None:
        with self._lock:
            if self._devices.get(conn.name) is conn:
                del self._devices[conn.name]
        self._fail_pending(conn, "[device disconnected; command lost]")
        logger.info("device: %s disconnected", conn.name)

    @staticmethod
    def _fail_pending(conn: DeviceConn, note: str) -> None:
        with conn.lock:
            procs = list(conn.pending.values())
            conn.pending.clear()
        for proc in procs:
            proc.finish(EXIT_LOST, note)

    def device(self, name: str) -> Optional[DeviceConn]:
        with self._lock:
            return self._devices.get(name)

    def devices(self) -> list[DeviceConn]:
        with self._lock:
            return list(self._devices.values())

    def on_message(self, conn: DeviceConn, msg: dict) -> None:
        kind, rid = msg.get("type"), msg.get("id", "")
        with conn.lock:
            proc = conn.pending.get(rid) if kind == "output" else conn.pending.pop(rid, None) if kind == "exit" else None
        if kind == "output" and proc is not None:
            proc.feed(msg.get("data") or "")
        elif kind == "exit" and proc is not None:
            error = msg.get("error") or ""
            code = int(msg.get("code", 1))
            proc.refused = bool(error) and code == EXIT_REFUSED
            proc.finish(code, f"[{error}]" if error else "")

    # execution
    def start(self, name: str, *, cmd: str, cwd: str, login: bool, timeout: float,
              stdin: Optional[str]) -> DeviceProcess:
        conn = self.device(name)
        rid = uuid.uuid4().hex
        if conn is None:
            proc = DeviceProcess(rid, lambda: False)
            proc.finish(EXIT_LOST, f"[device {name} is not connected]")
            return proc

        def kill() -> bool:
            if self.device(name) is not conn:
                return False
            try:
                conn.send({"type": "kill", "id": rid})
                return True
            except Exception:
                return False

        proc = DeviceProcess(rid, kill)
        with conn.lock:
            conn.pending[rid] = proc
        msg = {"type": "exec", "id": rid, "cmd": cmd, "cwd": cwd, "login": login, "timeout": timeout}
        if stdin is not None:
            msg["stdin"] = stdin
        try:
            conn.send(msg)
        except Exception as exc:
            with conn.lock:
                conn.pending.pop(rid, None)
            proc.finish(EXIT_LOST, f"[could not reach device {name}: {exc}]")
        return proc

    def run(self, name: str, cmd: str, cwd: str, timeout: float = 20) -> tuple[int, str]:
        """Run a short command synchronously (for /device checks)."""
        proc = self.start(name, cmd=cmd, cwd=cwd, login=False, timeout=timeout, stdin=None)
        if proc.wait(timeout + 5) is None:
            proc.kill()
            return EXIT_KILLED, "timed out"
        with proc.stdout:
            out = proc.stdout.read()
        return proc.returncode or 0, out

    # bindings
    def binding(self, session_id: str) -> Optional[Binding]:
        with self._lock:
            return self._bindings.get(session_id) if session_id else None

    def bind(self, session_id: str, device: str, directory: str) -> Binding:
        if not session_id:
            raise DeviceError("this surface did not give a session id")
        if device not in configured_devices():
            raise DeviceError(f"unknown device {device!r}; configured: {', '.join(sorted(configured_devices())) or 'none'}")
        conn = self.device(device)
        if conn is None:
            raise DeviceError(f"device {device} is not connected")
        if not directory.startswith("/"):
            raise DeviceError("the directory must be an absolute path on the device")
        directory = posixpath.normpath(directory)
        code, out = self.run(device, "pwd -P", directory)
        if code != 0:
            raise DeviceError(f"device {device} refused {directory}: {out.strip()}")
        real = out.strip().splitlines()[-1] if out.strip() else directory
        binding = Binding(device=device, dir=real, generation=uuid.uuid4().hex[:12])
        with self._lock:
            self._bindings[session_id] = binding
            self._save()
        return binding

    def device_cwd(self, session_id: str) -> Optional[str]:
        """The session's current directory on its device: the bound dir until a command cds."""
        with self._lock:
            b = self._bindings.get(session_id) if session_id else None
            return None if b is None else self._cwds.get(f"{session_id}:{b.generation}", b.dir)

    def set_device_cwd(self, session_id: str, cwd: str) -> None:
        with self._lock:
            b = self._bindings.get(session_id)
            if b is not None and cwd:
                self._cwds[f"{session_id}:{b.generation}"] = cwd

    def unbind(self, session_id: str) -> bool:
        with self._lock:
            found = self._bindings.pop(session_id, None) is not None
            if found:
                self._save()
        return found


_HUB: Optional[Hub] = None
_HUB_LOCK = threading.Lock()


def hub() -> Hub:
    global _HUB
    with _HUB_LOCK:
        if _HUB is None:
            _HUB = Hub(_home() / "device" / "bindings.json")
        return _HUB


def authenticate(name: str, token: str) -> bool:
    expected = configured_devices().get(name)
    return bool(expected) and hmac.compare_digest(expected.encode(), (token or "").encode())


def anchor_file_tools(task_id: str, session_id: str) -> None:
    """Point the session's cwd record at the device directory before a tool runs.

    Each turn re-registers the session's server workspace (the placeholder) as its cwd, and the
    file tools resolve relative paths against that record on the server. Rewriting the record to
    the device-side cwd makes ``patch app.py`` mean the device's ``app.py``.
    """
    cwd = hub().device_cwd(session_id or current_session_id())
    if not cwd or not task_id:
        return
    from tools.terminal_tool import get_session_cwd, record_session_cwd
    if get_session_cwd(task_id) != cwd:
        record_session_cwd(task_id, cwd)


def current_session_id() -> str:
    from gateway.session_context import get_session_env
    return get_session_env("HERMES_SESSION_ID", "")


# --- terminal backend -------------------------------------------------------------------------

from tools.environments.base import BaseEnvironment  # noqa: E402  (Hermes runtime import)

_EXEC_CWD: contextvars.ContextVar[str] = contextvars.ContextVar("hermes_device_exec_cwd", default="")


class DeviceEnvironment(BaseEnvironment):
    """Runs each command on the device bound to the current session, in the bound directory.

    The env is created per session by the terminal tool, but the binding is looked up on every
    command from the session id ContextVar, so ``/device`` takes effect on the next command and
    a binding made after the env was created still applies. Server paths (the session's
    placeholder cwd, which must exist on the server) are never sent to the device: ``cwd``
    keeps the device-side directory and ignores placeholder writes.
    """

    def __init__(self, cwd: str, timeout: int, task_id: str = "default"):
        self._device_cwd = ""
        self._placeholders = {os.path.realpath(cwd)} if cwd else set()
        self._bound_generation = ""
        self._device_name = ""
        self._device_tmp = "/tmp"  # no-tmp: ok — device-side, replaced by the device's tmp_dir on bind
        self._last_proc: Optional[DeviceProcess] = None
        self.task_id = task_id
        super().__init__(cwd=cwd, timeout=timeout)

    # cwd: device-side only
    def _is_placeholder(self, path: str) -> bool:
        if not path:
            return True
        real = os.path.realpath(path)
        if real in self._placeholders:
            return True
        return any(real == r or real.startswith(r + os.sep) for r in placeholder_roots())

    @property
    def cwd(self) -> str:
        return self._device_cwd

    @cwd.setter
    def cwd(self, value: str) -> None:
        if self._device_name and value and not self._is_placeholder(value):
            self._device_cwd = value
        elif not self._device_name:
            self._device_cwd = value

    def get_temp_dir(self) -> str:
        return self._device_tmp

    def _rebind(self, binding: Binding) -> None:
        conn = hub().device(binding.device)
        self._device_name = binding.device
        self._device_tmp = (conn.tmp_dir if conn and conn.tmp_dir else "/tmp")  # no-tmp: ok — device-side
        self._device_cwd = binding.dir
        temp_dir = self._device_tmp.rstrip("/") or "/"
        self._snapshot_path = f"{temp_dir}/hermes-snap-{self._session_id}.sh"
        self._cwd_file = f"{temp_dir}/hermes-cwd-{self._session_id}.txt"
        self._snapshot_ready = False
        self._prefer_nonlogin = False
        self._bound_generation = binding.generation
        self.init_session()
        logger.info("device: session %s now runs on %s:%s (snapshot=%s)", current_session_id(),
                    binding.device, binding.dir, self._snapshot_ready)

    def execute(self, command: str, cwd: str = "", **kwargs) -> dict:
        binding = hub().binding(current_session_id())
        if binding is None:
            return {"output": "No device is bound to this session. Ask the user to run "
                              "/device <name> <dir> first.", "returncode": 1}
        if binding.generation != self._bound_generation:
            self._rebind(binding)
        effective = self._device_cwd if self._is_placeholder(cwd) else cwd
        token = _EXEC_CWD.set(effective)
        try:
            result = super().execute(command, effective, **kwargs)
        finally:
            _EXEC_CWD.reset(token)
        last = self._last_proc
        if last is not None and last.refused and effective == self._device_cwd:
            # A tracked cwd outside the device's roots (after `cd /`) must not stick.
            self._device_cwd = binding.dir
        hub().set_device_cwd(current_session_id(), self._device_cwd)
        return result

    def _run_bash(self, cmd_string: str, *, login: bool = False, timeout: int = 120,
                  stdin_data: Optional[str] = None):
        binding = hub().binding(current_session_id())
        name = binding.device if binding else self._device_name
        if not name:
            proc = DeviceProcess(uuid.uuid4().hex, lambda: False)
            proc.finish(1, "[no device bound]")
            return proc
        cwd = _EXEC_CWD.get() or self._device_cwd or (binding.dir if binding else "")
        proc = hub().start(name, cmd=cmd_string, cwd=cwd, login=login, timeout=float(timeout), stdin=stdin_data)
        self._last_proc = proc
        return proc

    def cleanup(self):
        if self._device_name and hub().device(self._device_name):
            quoted = " ".join(shlex.quote(p) for p in (self._snapshot_path, self._cwd_file))
            try:
                hub().start(self._device_name, cmd=f"rm -f {quoted}", cwd=self._device_cwd, login=False,
                            timeout=10, stdin=None)
            except Exception:
                pass
