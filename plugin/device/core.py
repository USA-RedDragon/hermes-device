"""Shared state of the device plugin: connected devices, session bindings, lineage resolution,
the terminal backend and the server-side reader wrappers.

Loaded once per process under the fixed module name ``hermes_device_core`` so the agent half
(``__init__.py``) and the dashboard routes (``dashboard/plugin_api.py``) share one hub.
docs/protocol.md sections 3, 4, 8 and 9 are the normative description.
"""
from __future__ import annotations

import asyncio
import contextvars
import hashlib
import hmac
import importlib.util
import json
import logging
import os
import posixpath
import queue
import re
import shlex
import sys
import tempfile
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable, Optional

logger = logging.getLogger("hermes_device")

PLUGIN_ID = "device"
PLUGIN_VERSION = "0.6.0"

PROTOCOL_VERSION = 3
MIN_PROTOCOL_VERSION = 2
RESUME_VERSION = 3
TYPE_HELLO = "hello"
TYPE_WELCOME = "welcome"
TYPE_EXEC = "exec"
TYPE_KILL = "kill"
TYPE_OUTPUT = "output"
TYPE_EXIT = "exit"
TYPE_PING = "ping"
TYPE_PONG = "pong"
TYPE_RESUME = "resume"
TYPE_BROWSER = "browser"
TYPE_BROWSER_RESULT = "browser_result"
TYPE_BROWSER_STATUS = "browser_status"

# Capabilities the plugin accepts from a device's hello (section 4.1).
CAPABILITY_BROWSER = "browser"
CAPABILITIES = frozenset({CAPABILITY_BROWSER})

BROWSER_NOT_CONNECTED = "not_connected"
BROWSER_REFUSED = "refused"
BROWSER_TIMEOUT = "timeout"
BROWSER_CANCELLED = "cancelled"
BROWSER_FAILED = "failed"
BROWSER_DEFAULT_TIMEOUT_S = 60
BROWSER_MAX_TIMEOUT_S = 300
MAX_BROWSER_RESULT = 16_777_216
# How long past its timeout the plugin waits for a browser_result before it gives up and kills it.
BROWSER_SLACK_S = 10.0

CLOSE_PROTOCOL_ERROR = 1002
CLOSE_SERVICE_RESTART = 1012
CLOSE_UNAUTHORIZED = 4401
CLOSE_ASSERTION_REFUSED = 4403
CLOSE_PING_TIMEOUT = 4408
CLOSE_REPLACED = 4409
CLOSE_UNSUPPORTED_VERSION = 4426
CLOSE_UNSUPPORTED_PLATFORM = 4427
CLOSE_BACKEND_UNAVAILABLE = 4502

EXIT_REFUSED = 126
EXIT_KILLED = 130
EXIT_LOST = 255

PING_INTERVAL_S = 25
PING_TIMEOUT_S = 15
HELLO_TIMEOUT_S = 10
SILENCE_TIMEOUT_S = 60
DEFAULT_GRACE_S = 120
MAX_GRACE_S = 3600

PLUGIN_EXEC_PATH = "/api/plugins/device/ws"
ACP_HOST_PATH = "/acp"

SEND_TIMEOUT_S = 15.0
MAX_LINEAGE_HOPS = 100
BINDINGS_VERSION = 2
MAX_DIR_ENTRIES = 500
MAX_READ_BYTES = 16 * 1024 * 1024
DEFAULT_FALLBACK_ROOT = "/work"

STATE_BOUND = "bound"
STATE_NEVER_BOUND = "never_bound"
STATE_FAIL_CLOSED = "fail_closed"

DEVICE_ID_RE = re.compile(r"[A-Za-z0-9_-]{1,64}")
FILE_AND_COMMAND_TOOLS = frozenset({
    "terminal", "process_manage", "read_terminal", "close_terminal", "read_file", "write_file", "patch",
    "search_files", "execute_code"})
FILE_PATH_TOOLS = ("read_file", "write_file", "patch", "search_files")
POD_ONLY_TOOLS = frozenset({"cronjob_manage", "kanban_create", "kanban_schedule", "kanban_unblock"})
_BOM = "\ufeff"


class DeviceError(Exception):
    """A request the plugin cannot carry out (unknown device, offline, refused dir)."""


class BrowserError(DeviceError):
    """A browser request that did not succeed. ``code`` is a browser_result code."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


class LineageError(Exception):
    """The session lineage could not be read completely."""


def _load_sibling(module_name: str, file_name: str):
    module = sys.modules.get(module_name)
    if module is None:
        spec = importlib.util.spec_from_file_location(module_name, Path(__file__).with_name(file_name))
        module = importlib.util.module_from_spec(spec)
        sys.modules[module_name] = module
        spec.loader.exec_module(module)
    return module


assertion = _load_sibling("hermes_device_assertion", "assertion.py")


# --- settings ---------------------------------------------------------------------------------

def _config() -> dict[str, Any]:
    try:
        from hermes_cli.config import load_config_readonly
        return load_config_readonly() or {}
    except Exception:
        logger.debug("device: config not readable", exc_info=True)
        return {}


def settings() -> dict[str, Any]:
    """``plugins.entries.device.settings`` from config.yaml, or ``{}``."""
    entry = ((_config().get("plugins") or {}).get("entries") or {}).get(PLUGIN_ID) or {}
    value = entry.get("settings") if isinstance(entry, dict) else None
    return value if isinstance(value, dict) else {}


def backend_id() -> str:
    value = settings().get("backend_id")
    return value if isinstance(value, str) else ""


def placeholder() -> str:
    """The configured placeholder directory, normalized, or "" when unset."""
    value = settings().get("placeholder")
    if not isinstance(value, str) or not value.strip():
        return ""
    return os.path.realpath(os.path.expanduser(value.strip()))


def fallback() -> str:
    """``local``, ``refuse`` or ``device`` (section 8.1). A bad value refuses never-bound sessions."""
    value = settings().get("fallback", "local")
    if value in ("local", "refuse"):
        return value
    if value == "device":
        if fallback_device():
            return value
        logger.error("device: fallback device needs the fallback_device setting; refusing never-bound sessions")
        return "refuse"
    logger.error("device: fallback %r is not local, refuse or device; refusing never-bound sessions", value)
    return "refuse"


def fallback_device() -> str:
    """The sandbox device (id or name) that never-bound sessions run on with ``fallback: device``."""
    value = settings().get("fallback_device")
    return value.strip() if isinstance(value, str) else ""


def fallback_root() -> str:
    """The sandbox directory that holds one working directory per session lineage, or "" when
    the setting is not an absolute path."""
    value = settings().get("fallback_root", DEFAULT_FALLBACK_ROOT)
    if not isinstance(value, str) or not posixpath.isabs(value.strip()):
        logger.error("device: fallback_root %r is not an absolute path; never-bound sessions fail closed", value)
        return ""
    return posixpath.normpath(value.strip())


def grace_seconds() -> float:
    """How long a dropped version 3 leg keeps its pending commands for a resume (section 4.7).
    0 turns resume off."""
    value = settings().get("grace_seconds", DEFAULT_GRACE_S)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        logger.error("device: grace_seconds %r is not a number of seconds; using %d", value, DEFAULT_GRACE_S)
        return float(DEFAULT_GRACE_S)
    return float(min(value, MAX_GRACE_S))


def dev_static_tokens() -> bool:
    return settings().get("dev_static_tokens") is True


def device_write_roots() -> list[str]:
    roots = settings().get("device_write_roots") or []
    return [r for r in roots if isinstance(r, str) and r] if isinstance(roots, list) else []


def terminal_cwd() -> str:
    term = _config().get("terminal") or {}
    value = term.get("cwd") if isinstance(term, dict) else None
    return value if isinstance(value, str) else ""


def configured_devices() -> dict[str, str]:
    """Dev mode: device name -> static token. ``token_env`` names an env var holding it."""
    out: dict[str, str] = {}
    devices = settings().get("devices") or {}
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


def authenticate_static(name: str, token: str) -> bool:
    expected = configured_devices().get(name)
    return bool(expected) and hmac.compare_digest(expected.encode(), (token or "").encode())


def hermes_home() -> Path:
    from hermes_constants import get_hermes_home
    return Path(get_hermes_home())


def hermes_version() -> str:
    try:
        from hermes_cli.version_info import get_version_info
        base = get_version_info().base_version
    except Exception:
        return "unknown"
    return base if base.startswith("v") or base == "unknown" else f"v{base}"


# --- hub public keys --------------------------------------------------------------------------

_VERIFIER = assertion.Verifier()
_KEY_CACHE: dict[str, Any] = {"text": None, "keys": []}
_KEY_LOCK = threading.Lock()


def hub_key_text() -> Optional[bytes]:
    s = settings()
    path = s.get("hub_public_key_file")
    if isinstance(path, str) and path:
        try:
            return Path(os.path.expanduser(path)).read_bytes()
        except OSError as exc:
            logger.error("device: cannot read hub_public_key_file %s: %s", path, exc)
            return None
    env_name = s.get("hub_public_key_env") or "HERMES_DEVICE_HUB_PUBLIC_KEY"
    value = os.environ.get(str(env_name))
    return value.encode("utf-8", errors="surrogatepass") if value else None


def verifier() -> Any:
    """The process-wide verifier with the currently configured keys (re-read on every call, so a
    rotated key file takes effect without a restart; the nonce cache is kept)."""
    text = hub_key_text()
    with _KEY_LOCK:
        if text != _KEY_CACHE["text"]:
            keys: list = []
            if text is not None:
                try:
                    keys = assertion.parse_public_keys(text)
                except assertion.Refused as exc:
                    logger.error("device: hub public keys refused: %s", exc)
            _KEY_CACHE.update(text=text, keys=keys)
        _VERIFIER.keys = list(_KEY_CACHE["keys"])
    return _VERIFIER


# --- process handle ---------------------------------------------------------------------------

class DeviceProcess:
    """ProcessHandle for one command running on a device. Output arrives from the WebSocket
    thread through :meth:`feed` and is written to a pipe in order by a pump thread, so the base
    class's drain thread reads it as it streams."""

    def __init__(self, request_id: str, kill_fn: Callable[[], bool]):
        self.request_id = request_id
        self._kill_fn = kill_fn
        # Output bytes (UTF-8) received so far; a resume asks the device for the rest.
        self.received = 0
        read_fd, self._write_fd = os.pipe()
        self._stdout = os.fdopen(read_fd, "r", encoding="utf-8", errors="replace")
        self._queue: "queue.SimpleQueue[Any]" = queue.SimpleQueue()
        self._returncode: Optional[int] = None
        self._done = threading.Event()
        self._finished = False
        self.refused = False
        self.error = ""
        self.cwd = ""
        self._lock = threading.Lock()
        threading.Thread(target=self._pump, name=f"device-pump-{request_id[:8]}", daemon=True).start()

    def feed(self, data: str) -> None:
        if data:
            self._queue.put(data)

    def feed_at(self, data: str, offset: Any) -> None:
        """Feed a version 3 output chunk that starts at byte ``offset`` of the command's output:
        skip bytes already received, and note bytes the device no longer had."""
        raw = data.encode("utf-8", errors="surrogatepass")
        if isinstance(offset, bool) or not isinstance(offset, int) or offset < 0:
            offset = self.received
        if offset > self.received:
            self.feed(f"\n[hermes-device: {offset - self.received} bytes of output were lost while the device "
                      f"was disconnected]\n")
        elif offset < self.received:
            raw = raw[self.received - offset:]
            offset = self.received
        if raw:
            self.feed(raw.decode("utf-8", errors="replace"))
        self.received = max(self.received, offset + len(raw))

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


def finished_process(code: int, note: str) -> DeviceProcess:
    proc = DeviceProcess(uuid.uuid4().hex, lambda: False)
    proc.finish(code, note)
    return proc


# --- connections --------------------------------------------------------------------------------

@dataclass
class DeviceConn:
    """One connected device. ``send_text`` and ``close`` coroutines run on ``loop``."""

    device_id: str
    name: str
    send_text: Callable[[str], Any]
    loop: asyncio.AbstractEventLoop
    roots: list[str]
    tmp_dir: str
    home: str = ""
    hostname: str = ""
    os: str = ""
    arch: str = ""
    shell: str = ""
    client_version: str = ""
    close: Optional[Callable[[int, str], Any]] = None
    # The negotiated exec leg protocol version (section 4.1).
    version: int = MIN_PROTOCOL_VERSION
    # The capabilities both sides accepted (section 4.1).
    capabilities: frozenset = frozenset()
    # The device's last browser_status, without its type.
    browser: dict = field(default_factory=dict)
    # Browser requests waiting for their browser_result. They never wait for a resume.
    browser_calls: dict[str, "BrowserCall"] = field(default_factory=dict)
    connected_at: float = field(default_factory=time.time)
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
            fut.cancel()
            raise

    def within_roots(self, path: str) -> bool:
        return any(is_under(path, r) for r in self.roots)

    def info(self) -> dict:
        out = {"device_id": self.device_id, "name": self.name, "hostname": self.hostname, "os": self.os,
               "arch": self.arch, "home": self.home, "roots": list(self.roots),
               "client_version": self.client_version, "connected_at": int(self.connected_at),
               "capabilities": sorted(self.capabilities)}
        if CAPABILITY_BROWSER in self.capabilities:
            out["browser"] = dict(self.browser) or {"connected": False}
        return out


class BrowserCall:
    """One browser request waiting for its browser_result."""

    def __init__(self, request_id: str):
        self.request_id = request_id
        self._done = threading.Event()
        self.ok = False
        self.result: Any = None
        self.code = ""
        self.error = ""

    def finish(self, ok: bool, result: Any = None, code: str = "", error: str = "") -> None:
        if self._done.is_set():
            return
        self.ok, self.result, self.code, self.error = ok, result, code, error
        self._done.set()

    def wait(self, timeout: float) -> bool:
        return self._done.wait(timeout)


def is_under(path: str, root: str) -> bool:
    path, root = posixpath.normpath(path), posixpath.normpath(root)
    return path == root or path.startswith(root.rstrip("/") + "/")


# --- bindings ---------------------------------------------------------------------------------

@dataclass(frozen=True)
class Binding:
    session_id: str
    device_id: str
    dir: str
    generation: str
    bound_at: float

    def to_json(self) -> dict:
        return {"session_id": self.session_id, "device_id": self.device_id, "dir": self.dir,
                "generation": self.generation, "bound_at": self.bound_at}


@dataclass(frozen=True)
class Resolution:
    state: str
    session_id: str
    via: Optional[str] = None
    binding: Optional[Binding] = None
    reason: str = ""
    # The lineage root of a never-bound session; it names the session's sandbox directory.
    root: Optional[str] = None
    # True when the session is served (or would be) by the fallback sandbox device.
    sandbox: bool = False

    def to_json(self) -> dict:
        return {"state": self.state, "via": self.via}


class StateUnavailable(DeviceError):
    """bindings.json exists but cannot be read; every non-bound session fails closed."""


ParentLookup = Callable[[str], tuple[bool, Optional[str]]]


class BindError(DeviceError):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status, self.code, self.message = status, code, message


class Hub:
    """Process-wide registry of connected devices and the persisted bindings (section 8.2)."""

    def __init__(self, state_path: Optional[Path] = None, parent_lookup: Optional[ParentLookup] = None,
                 session_check: Optional[Callable[[str], Optional[str]]] = None):
        self._lock = threading.RLock()
        self._session_check = session_check
        self._devices: dict[str, DeviceConn] = {}
        self._seen: set[str] = set()
        self._state_path = state_path
        self._parent_lookup = parent_lookup
        self._parents: dict[str, str] = {}
        self._orphans: dict[str, str] = {}
        self._cwds: dict[str, str] = {}
        self._state_error = ""
        self._bindings: dict[str, Binding] = {}
        self._ever_bound: set[str] = set()
        self._sandbox_dirs: dict[tuple[str, str], str] = {}
        self._sandbox_ready: set[tuple[str, str]] = set()
        self._parked: dict[str, dict[str, DeviceProcess]] = {}
        self._park_timers: dict[str, tuple[threading.Timer, object]] = {}
        # Commands the plugin killed. A resume never lists them, so the device kills them too.
        self._killed: set[str] = set()
        self._load()

    # persistence
    def _load(self) -> None:
        path = self._state_path
        if not path or not path.exists():
            return
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(raw, dict):
                raise ValueError("not a JSON object")
            if raw.get("version") == BINDINGS_VERSION:
                self._load_v2(raw)
            elif "version" not in raw:
                self._migrate_v1(raw)
                self._save()
            else:
                raise ValueError(f"unknown version {raw.get('version')!r}")
        except Exception as exc:
            self._bindings, self._ever_bound = {}, set()
            self._state_error = f"{path} is unreadable ({exc})"
            logger.error("device: %s; every session that is not bound fails closed until it is fixed",
                         self._state_error)

    def _load_v2(self, raw: dict) -> None:
        bindings: dict[str, Binding] = {}
        for sid, b in (raw.get("bindings") or {}).items():
            bindings[sid] = Binding(session_id=sid, device_id=str(b["device_id"]), dir=str(b["dir"]),
                                    generation=str(b["generation"]), bound_at=float(b.get("bound_at") or 0))
        ever = raw.get("ever_bound") or []
        if not isinstance(ever, list):
            raise ValueError("ever_bound is not a list")
        self._bindings = bindings
        self._ever_bound = {str(s) for s in ever} | set(bindings)
        self._seen.update(b.device_id for b in bindings.values())

    def _migrate_v1(self, raw: dict) -> None:
        """v1 (proof of concept) keyed bindings by device name. In dev mode the device id is the
        configured name, so a name that is a valid id carries over; others are dropped but stay in
        ever_bound, so their sessions fail closed instead of running on the server."""
        stamp = self._state_path.stat().st_mtime if self._state_path else time.time()
        bindings: dict[str, Binding] = {}
        ever: set[str] = set()
        for sid, b in (raw.get("bindings") or {}).items():
            ever.add(str(sid))
            if not isinstance(b, dict):
                continue
            name, directory = str(b.get("device") or ""), b.get("dir")
            if DEVICE_ID_RE.fullmatch(name) and isinstance(directory, str) and directory.startswith("/"):
                bindings[sid] = Binding(session_id=sid, device_id=name, dir=directory,
                                        generation=str(b.get("generation") or uuid.uuid4().hex[:12]), bound_at=stamp)
            else:
                logger.warning("device: v1 binding of session %s (device %r) dropped; the session fails closed",
                               sid, name)
        self._bindings, self._ever_bound = bindings, ever
        self._seen.update(b.device_id for b in bindings.values())
        logger.info("device: migrated %d v1 bindings to version %d", len(bindings), BINDINGS_VERSION)

    def _save(self) -> None:
        if not self._state_path:
            return
        path = self._state_path
        path.parent.mkdir(parents=True, exist_ok=True)
        doc = {"version": BINDINGS_VERSION,
               "bindings": {sid: {"device_id": b.device_id, "dir": b.dir, "generation": b.generation,
                                  "bound_at": b.bound_at} for sid, b in sorted(self._bindings.items())},
               "ever_bound": sorted(self._ever_bound)}
        fd, tmp = tempfile.mkstemp(prefix=".bindings-", suffix=".tmp", dir=str(path.parent))
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(doc, fh, indent=2)
                fh.write("\n")
                fh.flush()
                os.fsync(fh.fileno())
            os.chmod(tmp, 0o644)
            os.replace(tmp, path)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise

    def _require_state(self) -> None:
        if self._state_error:
            raise StateUnavailable(self._state_error)

    # devices
    def attach(self, conn: DeviceConn) -> list[dict]:
        """Register ``conn`` as the device's connection. On a version 3 connection the commands
        that a dropped or replaced connection of the device left pending move to it; the result
        is the ``resume`` list for them (section 4.7). Otherwise they fail."""
        with self._lock:
            old = self._devices.get(conn.device_id)
            self._devices[conn.device_id] = conn
            self._seen.add(conn.device_id)
            timer = self._park_timers.pop(conn.device_id, None)
            carried = self._parked.pop(conn.device_id, {})
            if old is not None and old is not conn:
                with old.lock:
                    if old.version >= RESUME_VERSION:
                        carried.update(old.pending)
                        old.pending.clear()
            killed = {rid: carried.pop(rid) for rid in list(carried) if rid in self._killed}
            self._killed.difference_update(killed)
            lost = {}
            if conn.version < RESUME_VERSION:
                lost, carried = carried, {}
            with conn.lock:
                conn.pending.update(carried)
        if timer is not None:
            timer[0].cancel()
        for proc in killed.values():
            proc.finish(EXIT_KILLED, "[command killed while the device was disconnected]")
        for proc in lost.values():
            proc.finish(EXIT_LOST, "[device reconnected with protocol version 2, which cannot resume; command lost]")
        resume = [{"id": rid, "offset": proc.received} for rid, proc in carried.items()]
        if old is not None and old is not conn:
            logger.warning("device: %s connected again; closing the previous connection", conn.device_id)
            self._fail_pending(old, "[device reconnected; command lost]")
            self._fail_browser(old, f"device {conn.device_id} reconnected during the browser operation")
            if old.close is not None:
                try:
                    asyncio.run_coroutine_threadsafe(old.close(CLOSE_REPLACED, "replaced by a newer connection"),
                                                     old.loop)
                except RuntimeError:
                    logger.debug("device: old connection loop is closed", exc_info=True)
        logger.info("device: %s (%s) connected from %s with protocol %d, roots=%s, resuming %d commands",
                    conn.device_id, conn.name, conn.hostname, conn.version, conn.roots, len(resume))
        return resume

    def detach(self, conn: DeviceConn, resumable: bool = False) -> None:
        """Forget ``conn``. With ``resumable`` (a version 3 leg that dropped while the gateway
        keeps running), its pending commands wait ``grace_seconds`` for the device to reconnect
        and resume them; otherwise they fail at once with exit 255."""
        grace = grace_seconds() if resumable and conn.version >= RESUME_VERSION else 0.0
        self._fail_browser(conn, f"device {conn.device_id} disconnected during the browser operation")
        with self._lock:
            current = self._devices.get(conn.device_id) is conn
            if current:
                del self._devices[conn.device_id]
            park = current and grace > 0
            if park:
                with conn.lock:
                    procs = dict(conn.pending)
                    conn.pending.clear()
                if procs:
                    self._parked.setdefault(conn.device_id, {}).update(procs)
                    old = self._park_timers.pop(conn.device_id, None)
                    if old is not None:
                        old[0].cancel()
                    token = object()
                    timer = threading.Timer(grace, self._expire, args=(conn.device_id, token))
                    timer.daemon = True
                    self._park_timers[conn.device_id] = (timer, token)
                    timer.start()
                    logger.info("device: %s disconnected; keeping %d commands for %ss for a resume",
                                conn.device_id, len(procs), grace)
                    return
        self._fail_pending(conn, "[device disconnected; command lost]")
        logger.info("device: %s disconnected", conn.device_id)

    def _expire(self, device_id: str, token: object) -> None:
        with self._lock:
            current = self._park_timers.get(device_id)
            if current is None or current[1] is not token:
                return
            del self._park_timers[device_id]
            procs = self._parked.pop(device_id, {})
            self._killed.difference_update(procs)
        for proc in procs.values():
            proc.finish(EXIT_LOST, f"[device {device_id} did not reconnect within {grace_seconds():g}s; "
                                   "command lost]")
        if procs:
            logger.warning("device: %s did not reconnect in time; %d commands lost", device_id, len(procs))

    @staticmethod
    def _fail_pending(conn: DeviceConn, note: str) -> None:
        with conn.lock:
            procs = list(conn.pending.values())
            conn.pending.clear()
        for proc in procs:
            proc.finish(EXIT_LOST, note)

    @staticmethod
    def _fail_browser(conn: DeviceConn, why: str) -> None:
        with conn.lock:
            calls = list(conn.browser_calls.values())
            conn.browser_calls.clear()
        for call in calls:
            call.finish(False, code=BROWSER_NOT_CONNECTED, error=why)

    def device(self, device_id: str) -> Optional[DeviceConn]:
        with self._lock:
            return self._devices.get(device_id)

    def devices(self) -> list[DeviceConn]:
        with self._lock:
            return list(self._devices.values())

    def seen(self, device_id: str) -> bool:
        with self._lock:
            return device_id in self._seen

    def find_device(self, name_or_id: str) -> DeviceConn:
        """A connected device by id, else by unique name."""
        conn = self.device(name_or_id)
        if conn is not None:
            return conn
        matches = [c for c in self.devices() if c.name == name_or_id]
        if len(matches) == 1:
            return matches[0]
        if len(matches) > 1:
            raise DeviceError(f"several connected devices are named {name_or_id!r}; use the device id: "
                              + ", ".join(sorted(c.device_id for c in matches)))
        raise DeviceError(f"no connected device is named {name_or_id!r}")

    def on_message(self, conn: DeviceConn, msg: dict) -> None:
        kind, rid = msg.get("type"), msg.get("id", "")
        if kind in (TYPE_BROWSER_RESULT, TYPE_BROWSER_STATUS):
            if CAPABILITY_BROWSER in conn.capabilities:
                self._on_browser(conn, msg)
            return
        if kind not in (TYPE_OUTPUT, TYPE_EXIT) or not isinstance(rid, str):
            return
        with conn.lock:
            proc = conn.pending.get(rid) if kind == TYPE_OUTPUT else conn.pending.pop(rid, None)
        if kind == TYPE_EXIT:
            with self._lock:
                self._killed.discard(rid)
        if proc is None:
            return
        if kind == TYPE_OUTPUT:
            data = msg.get("data")
            data = data if isinstance(data, str) else ""
            if conn.version >= RESUME_VERSION:
                proc.feed_at(data, msg.get("offset"))
            else:
                proc.feed(data)
            return
        error = msg.get("error") if isinstance(msg.get("error"), str) else ""
        try:
            code = int(msg.get("code", 1))
        except (TypeError, ValueError):
            code = 1
        proc.refused = bool(error) and code == EXIT_REFUSED
        proc.error = error
        if proc.refused and proc.cwd:
            self._forget_sandbox_dir(conn, proc.cwd)
        proc.finish(code, f"[{error}]" if error else "")

    def _on_browser(self, conn: DeviceConn, msg: dict) -> None:
        if msg.get("type") == TYPE_BROWSER_STATUS:
            status = {"connected": msg.get("connected") is True}
            for key in ("browser", "extension_version"):
                if isinstance(msg.get(key), str) and msg[key]:
                    status[key] = msg[key][:64]
            conn.browser = status
            logger.info("device: %s browser %s", conn.device_id, "connected" if status["connected"] else "not connected")
            return
        rid = msg.get("id")
        if not isinstance(rid, str):
            return
        with conn.lock:
            call = conn.browser_calls.pop(rid, None)
        if call is None:
            return
        if msg.get("ok") is True:
            call.finish(True, result=msg.get("result"))
            return
        code = msg.get("code") if isinstance(msg.get("code"), str) else BROWSER_FAILED
        error = msg.get("error") if isinstance(msg.get("error"), str) else "browser operation failed"
        call.finish(False, code=code, error=error)

    def browser_call(self, device_id: str, op: str, args: dict, timeout: float = BROWSER_DEFAULT_TIMEOUT_S) -> Any:
        """Run one browser operation on the device (docs/browser.md section 4) and return its
        result. Raises BrowserError. Call from a worker thread, never the event loop."""
        conn = self.device(device_id)
        if conn is None:
            raise BrowserError(BROWSER_NOT_CONNECTED, f"device {device_id} is not connected")
        if CAPABILITY_BROWSER not in conn.capabilities:
            raise BrowserError(BROWSER_NOT_CONNECTED,
                               f"device {conn.name} [{device_id}] does not offer browser control: set browser.enabled "
                               f"to true in its hermes-device config, run hermes-device browser install, and restart "
                               f"hermes-device run")
        timeout = max(1.0, min(float(timeout), BROWSER_MAX_TIMEOUT_S))
        rid = uuid.uuid4().hex
        call = BrowserCall(rid)
        with conn.lock:
            conn.browser_calls[rid] = call
        try:
            conn.send({"type": TYPE_BROWSER, "id": rid, "op": op, "args": args, "timeout": timeout})
        except Exception as exc:
            with conn.lock:
                conn.browser_calls.pop(rid, None)
            raise BrowserError(BROWSER_NOT_CONNECTED, f"could not reach device {device_id}: {exc}") from exc
        if not call.wait(timeout + BROWSER_SLACK_S):
            with conn.lock:
                conn.browser_calls.pop(rid, None)
            try:
                conn.send({"type": TYPE_KILL, "id": rid})
            except Exception:
                logger.debug("device: kill of browser request %s not sent", rid, exc_info=True)
            raise BrowserError(BROWSER_TIMEOUT, f"device {device_id} did not answer the {op} request in {timeout:g}s")
        if not call.ok:
            raise BrowserError(call.code, call.error)
        return call.result

    # sandbox directories (section 8.3, fallback device)
    def note_sandbox_dir(self, device_id: str, directory: str, mkdir_cwd: str) -> None:
        """Record a sandbox session directory, created with mkdir -p (run in ``mkdir_cwd``, a
        device root) before the first command that runs in it or below it on each connection of
        the device, and again after the device refused a command there."""
        with self._lock:
            self._sandbox_dirs[(device_id, directory)] = mkdir_cwd

    def _sandbox_dir_of(self, device_id: str, cwd: str) -> Optional[tuple[str, str]]:
        """``(sandbox dir, mkdir cwd)`` of the sandbox directory that holds ``cwd``. Call with the lock."""
        probe = posixpath.normpath(cwd)
        while True:
            mkdir_cwd = self._sandbox_dirs.get((device_id, probe))
            if mkdir_cwd is not None:
                return probe, mkdir_cwd
            parent = posixpath.dirname(probe)
            if parent == probe:
                return None
            probe = parent

    def _forget_sandbox_dir(self, conn: DeviceConn, cwd: str) -> None:
        """The device refused a command in ``cwd`` (for example, the agent deleted its sandbox
        directory): make the next command create the directory again."""
        with self._lock:
            found = self._sandbox_dir_of(conn.device_id, cwd)
            if found is not None:
                self._sandbox_ready.discard((conn.conn_id, found[0]))

    def _ensure_sandbox_dir(self, conn: DeviceConn, device_id: str, cwd: str) -> Optional[DeviceProcess]:
        """None when ``cwd`` is in no sandbox directory or the directory exists; else a finished
        process that reports why it could not be created."""
        with self._lock:
            found = self._sandbox_dir_of(device_id, cwd)
            if found is None or (conn.conn_id, found[0]) in self._sandbox_ready:
                return None
        directory, root = found
        code, out, proc = self.run(device_id, "mkdir -p -- " + shlex.quote(directory), root)
        if code != 0:
            why = proc.error or out.strip() or f"exit {code}"
            return finished_process(EXIT_LOST if code == EXIT_LOST else EXIT_REFUSED,
                                    f"[hermes-device: could not create the sandbox directory {directory} on device "
                                    f"{device_id}: {why}]")
        with self._lock:
            self._sandbox_ready.add((conn.conn_id, directory))
        return None

    # execution
    def start(self, device_id: str, *, cmd: str, cwd: str, login: bool, timeout: float,
              stdin: Optional[str]) -> DeviceProcess:
        conn = self.device(device_id)
        if conn is None:
            return finished_process(EXIT_LOST, f"[device {device_id} is not connected; the command did not run]")
        if not posixpath.isabs(cwd or ""):
            return finished_process(EXIT_REFUSED, f"[hermes-device: no device directory for this command ({cwd!r})]")
        failed = self._ensure_sandbox_dir(conn, device_id, cwd)
        if failed is not None:
            return failed
        rid = uuid.uuid4().hex
        proc = DeviceProcess(rid, lambda: self._kill(device_id, rid))
        proc.cwd = cwd
        with conn.lock:
            conn.pending[rid] = proc
        msg = {"type": TYPE_EXEC, "id": rid, "cmd": cmd, "cwd": cwd, "login": bool(login), "timeout": timeout}
        if stdin is not None:
            msg["stdin"] = stdin
        try:
            conn.send(msg)
        except Exception as exc:
            with conn.lock:
                conn.pending.pop(rid, None)
            proc.finish(EXIT_LOST, f"[could not reach device {device_id}: {exc}]")
        return proc

    def _kill(self, device_id: str, rid: str) -> bool:
        """Send ``kill`` on the connection that holds the command. The command is also marked
        killed, so that a resume never lists it, even when this kill went into a socket that was
        already dead; the device then kills it as a command the plugin no longer waits for. A
        command parked for a resume is dropped and reported killed at once."""
        with self._lock:
            self._killed.add(rid)
            self._parked.get(device_id, {}).pop(rid, None)
            conn = self._devices.get(device_id)
            held = False
            if conn is not None:
                with conn.lock:
                    held = rid in conn.pending
        if not held:
            with self._lock:
                self._killed.discard(rid)
            return False
        try:
            conn.send({"type": TYPE_KILL, "id": rid})
            return True
        except Exception:
            return False

    def run(self, device_id: str, cmd: str, cwd: str, timeout: float = 20,
            stdin: Optional[str] = None) -> tuple[int, str, DeviceProcess]:
        """Run a short command synchronously and return ``(code, output, process)``."""
        proc = self.start(device_id, cmd=cmd, cwd=cwd, login=False, timeout=timeout, stdin=stdin)
        if proc.wait(timeout + 5) is None:
            proc.kill()
            proc.wait(5)
            with proc.stdout:
                proc.stdout.read()
            return EXIT_KILLED, "timed out", proc
        with proc.stdout:
            out = proc.stdout.read()
        return proc.returncode if proc.returncode is not None else 1, out, proc

    # bindings
    def binding(self, session_id: str) -> Optional[Binding]:
        with self._lock:
            return self._bindings.get(session_id) if session_id else None

    def bindings(self) -> list[Binding]:
        with self._lock:
            return sorted(self._bindings.values(), key=lambda b: b.session_id)

    def ever_bound(self, session_id: str) -> bool:
        with self._lock:
            return session_id in self._ever_bound

    def _check_claim(self, session_id: str, device_id: str, operator: bool) -> None:
        """Refuse a bind that would take a session from another device (section 9 PUT rules).

        A device (``operator`` false: the PUT route, so the splice, the ACP bridge and the hub)
        may bind a session whose lineage resolves to that device, or one that is unbound and was
        never bound anywhere in its lineage. The operator's ``/device`` may move a session whose
        own binding names another device, and may bind any session that is not bound now; it may
        not carve a session out of a lineage bound to another device through an ancestor."""
        res = self.resolve(session_id)
        if res.state == STATE_BOUND and res.binding.device_id != device_id:
            if res.via == session_id:
                if operator:
                    return
                raise BindError(409, "bound_to_other_device",
                                f"session {session_id} is bound to device {res.binding.device_id}; only that "
                                f"device, or /device in the session, can move it")
            raise BindError(409, "bound_to_other_device",
                            f"session {session_id} is bound to device {res.binding.device_id} through session "
                            f"{res.via}; run /device off in the session first to move it")
        if res.state == STATE_FAIL_CLOSED and not operator:
            raise BindError(409, "not_bindable",
                            f"session {session_id} {res.reason}, so only /device in the session can bind it")

    def bind(self, session_id: str, device_id: str, directory: str, *, operator: bool = False) -> Binding:
        """Bind ``session_id`` to ``directory`` on a connected device (section 9 PUT rules).
        ``operator`` is true for the ``/device`` command. Raises :class:`BindError` with the HTTP
        status and error code."""
        if not session_id:
            raise BindError(400, "bad_request", "no session id")
        if self._state_error:
            raise BindError(503, "state_unavailable", self._state_error)
        conn = self.device(device_id)
        if conn is None:
            if self.seen(device_id):
                raise BindError(409, "device_offline", f"device {device_id} is not connected")
            raise BindError(404, "unknown_device", f"device {device_id} is not connected and was never seen")
        if not isinstance(directory, str) or not posixpath.isabs(directory):
            raise BindError(400, "bad_dir", "dir must be an absolute device path")
        directory = posixpath.normpath(directory)
        why = self._session_check(session_id) if self._session_check else None
        if why:
            raise BindError(409, "not_device_session", why)
        self._check_claim(session_id, device_id, operator)
        real = device_pwd(self, device_id, directory)
        with self._lock:
            # Again under the lock: a bind of this session or an ancestor may have landed meanwhile.
            self._check_claim(session_id, device_id, operator)
            existing = self._bindings.get(session_id)
            if existing is not None and existing.device_id == device_id and existing.dir == real:
                return existing
            binding = Binding(session_id=session_id, device_id=device_id, dir=real,
                              generation=uuid.uuid4().hex[:12], bound_at=round(time.time(), 3))
            previous = (dict(self._bindings), set(self._ever_bound))
            self._bindings[session_id] = binding
            self._ever_bound.add(session_id)
            try:
                self._save()
            except Exception as exc:
                self._bindings, self._ever_bound = previous
                raise BindError(500, "state_write_failed", f"could not write bindings: {exc}") from exc
        logger.info("device: session %s bound to %s:%s", session_id, device_id, real)
        return binding

    def unbind(self, session_id: str) -> bool:
        with self._lock:
            self._require_state()
            if session_id not in self._bindings:
                return False
            removed = self._bindings.pop(session_id)
            try:
                self._save()
            except Exception:
                self._bindings[session_id] = removed
                raise
        logger.info("device: session %s unbound", session_id)
        return True

    def device_cwd(self, binding: Binding) -> str:
        with self._lock:
            return self._cwds.get(f"{binding.session_id}:{binding.generation}", binding.dir)

    def set_device_cwd(self, binding: Binding, cwd: str) -> None:
        if cwd and posixpath.isabs(cwd):
            with self._lock:
                self._cwds[f"{binding.session_id}:{binding.generation}"] = cwd

    # lineage (section 8.3)
    def note_parent(self, session_id: str, parent_id: Optional[str]) -> None:
        """Record a parent link learned in process (a delegate child before its row exists)."""
        if session_id and parent_id and session_id != parent_id:
            with self._lock:
                self._parents.setdefault(session_id, parent_id)

    def note_orphan(self, session_id: str, reason: str) -> None:
        """Mark a side agent whose starting session could not be found: it fails closed."""
        if session_id:
            with self._lock:
                self._orphans[session_id] = reason

    def _parent(self, session_id: str) -> Optional[str]:
        with self._lock:
            cached = self._parents.get(session_id)
        if cached is not None:
            return cached
        lookup = self._parent_lookup or db_parent_lookup
        try:
            found, parent = lookup(session_id)
        except LineageError:
            raise
        except Exception as exc:
            raise LineageError(f"the session store failed ({exc})") from exc
        if not found:
            raise LineageError(f"session {session_id} has no row in the session store yet")
        if parent:
            with self._lock:
                self._parents[session_id] = parent
        return parent or None

    def resolve(self, session_id: str) -> Resolution:
        with self._lock:
            state_error = self._state_error
        if not session_id:
            return Resolution(STATE_FAIL_CLOSED, "", reason="has no session id in this context")
        seen: set[str] = set()
        saw_ever = False
        cur = session_id
        for _ in range(MAX_LINEAGE_HOPS + 1):
            if cur in seen:
                return Resolution(STATE_FAIL_CLOSED, session_id, reason="has a cycle in its lineage")
            seen.add(cur)
            binding = self.binding(cur)
            if binding is not None:
                return Resolution(STATE_BOUND, session_id, via=cur, binding=binding)
            with self._lock:
                orphan = self._orphans.get(cur)
            if orphan:
                return Resolution(STATE_FAIL_CLOSED, session_id, reason=orphan)
            saw_ever = saw_ever or self.ever_bound(cur)
            try:
                parent = self._parent(cur)
            except LineageError as exc:
                return Resolution(STATE_FAIL_CLOSED, session_id, reason=f"has an unreadable lineage: {exc}")
            if parent is None:
                if saw_ever:
                    return Resolution(STATE_FAIL_CLOSED, session_id, reason="was bound to a device and is not now")
                if state_error:
                    return Resolution(STATE_FAIL_CLOSED, session_id, reason=f"cannot be checked: {state_error}")
                return Resolution(STATE_NEVER_BOUND, session_id, root=cur)
            cur = parent
        return Resolution(STATE_FAIL_CLOSED, session_id,
                          reason=f"has a lineage longer than {MAX_LINEAGE_HOPS} hops")


def fail_closed_message(res: Resolution) -> str:
    sid = res.session_id or "(none)"
    if res.sandbox:
        return (f"hermes-device: session {sid} {res.reason or 'runs in the sandbox device'}, so this did not "
                f"run. Sessions that are not bound to a device run only in the sandbox, never on the server. Try "
                f"again when the sandbox is back, or bind the session with /device <device> <dir>.")
    return (f"hermes-device: session {sid} {res.reason or 'cannot be served'}, so this does not run on the "
            f"server. Rebind it with /device <device> <dir>.")


def refusal_message(res: Resolution) -> str:
    if res.state == STATE_NEVER_BOUND and fallback() == "device":
        return fail_closed_message(sandbox_resolution(res))
    if res.state == STATE_NEVER_BOUND:
        return (f"hermes-device: session {res.session_id or '(none)'} is not bound to a device and this server "
                f"refuses unbound sessions (fallback: refuse). Bind it with /device <device> <dir>.")
    return fail_closed_message(res)


def db_parent_lookup(session_id: str) -> tuple[bool, Optional[str]]:
    """``(row found, parent id)`` from the Hermes session store."""
    from hermes_cli.goals import _get_session_db
    db = _get_session_db()
    if db is None:
        raise LineageError("the session store is unavailable")
    row = db.get_session(session_id)
    if row is None:
        return False, None
    parent = row.get("parent_session_id")
    return True, (str(parent) if parent else None)


def _live_policy(session_id: str):
    """The frozen local-session policy of a live session in this gateway, or None when this
    process cannot tell (no gateway runner, a messaging session, a session not loaded)."""
    from gateway import run_api
    runner = getattr(run_api, "_ACTIVE_RUNNER", None)
    if runner is None:
        return None
    from gateway.session_authorities import all_authorities
    from gateway.session_policy import policy_for_source
    for authority in all_authorities(runner):
        live = (getattr(authority, "sessions", None) or {}).get(session_id)
        if live is not None:
            return policy_for_source(authority.runner, live.source)
    return None


def not_device_session_reason(session_id: str) -> Optional[str]:
    """Why a session can never serve a device (a bypass worker, or a frozen terminal backend
    that is not ``device``), or None when it can or this process cannot tell."""
    try:
        policy = _live_policy(session_id)
    except Exception:
        logger.debug("device: policy of %s not readable", session_id, exc_info=True)
        return None
    if policy is None:
        return None
    if getattr(policy, "safe_mode", False) or getattr(policy, "ignore_user_config", False):
        return (f"session {session_id} runs in a safe-mode or config-bypass worker, which loads no plugins, so it "
                f"cannot run on a device")
    try:
        terminal = json.loads(policy.terminal_json)
    except Exception:
        return None
    backend = str(terminal.get("TERMINAL_ENV") or "local").strip().lower() if isinstance(terminal, dict) else ""
    if backend != PLUGIN_ID:
        return f"session {session_id} has terminal backend {backend!r}, not {PLUGIN_ID!r}, so it cannot run on a device"
    return None


_HUB: Optional[Hub] = None
_HUB_LOCK = threading.Lock()


def hub() -> Hub:
    global _HUB
    with _HUB_LOCK:
        if _HUB is None:
            _HUB = Hub(hermes_home() / "device" / "bindings.json", session_check=not_device_session_reason)
        return _HUB


# --- session id -------------------------------------------------------------------------------

SESSION_OVERRIDE: contextvars.ContextVar[Optional[str]] = contextvars.ContextVar(
    "hermes_device_session_id", default=None)


def current_session_id() -> str:
    """The session the running turn serves: the plugin's own override (set around reference
    expansion, which runs before the turn binds the session), else the HERMES_SESSION_ID context
    variable of gateway.session_context. Never the process environment, which agent
    construction overwrites. "" when unset."""
    override = SESSION_OVERRIDE.get()
    if override is not None:
        return override
    try:
        from gateway.session_context import _SESSION_ID, _UNSET
    except Exception:
        return ""
    value = _SESSION_ID.get()
    return "" if value is _UNSET or not isinstance(value, str) else value


def session_cwd() -> str:
    """The session cwd context variable Hermes sets from the frozen policy, or ""."""
    try:
        from agent.runtime_cwd import scoped_session_cwd
        return scoped_session_cwd()
    except Exception:
        return ""


def is_placeholder_cwd(cwd: str) -> bool:
    ph = placeholder()
    if not ph or not cwd:
        return False
    try:
        return posixpath.normpath(cwd) == ph or os.path.realpath(cwd) == ph
    except (OSError, ValueError):
        return False


def resolve_session(session_id: str, cwd: Optional[str] = None) -> Resolution:
    """Section 8.3 for a running turn: lineage resolution, plus the placeholder rule (a session
    whose cwd is the placeholder and that has no binding fails closed), plus the implicit
    sandbox binding of ``fallback: device``."""
    res = hub().resolve(session_id)
    if res.state == STATE_NEVER_BOUND and any(is_placeholder_cwd(c) for c in (session_cwd(), cwd or "")):
        return Resolution(STATE_FAIL_CLOSED, session_id,
                          reason="was created for a device (its cwd is the placeholder) but is not bound")
    if res.state == STATE_NEVER_BOUND and fallback() == "device":
        return sandbox_resolution(res)
    return res


_SANDBOX_NAME = re.compile(r"[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}")


def sandbox_dir_name(session_id: str) -> str:
    """The directory name of a lineage root in the sandbox: the session id when it is a safe
    file name, else a cleaned prefix, "~" and a hash of the id. Safe names never hold "~", so
    distinct ids never share a directory."""
    if _SANDBOX_NAME.fullmatch(session_id):
        return session_id
    clean = re.sub(r"[^A-Za-z0-9._-]", "_", session_id)[:64].lstrip(".") or "session"
    return f"{clean}~{hashlib.sha256(session_id.encode('utf-8', 'surrogatepass')).hexdigest()[:12]}"


def sandbox_resolution(res: Resolution) -> Resolution:
    """The implicit binding of a never-bound session to its directory on the sandbox device
    (``fallback: device``), or a sandbox fail-closed resolution when the sandbox cannot serve it.
    The binding is computed, not stored: it never enters ``bindings`` or ``ever_bound``, so an
    explicit bind still works and wins."""
    sid, target, root = res.session_id, fallback_device(), fallback_root()
    if not target or not root:
        return Resolution(STATE_FAIL_CLOSED, sid, sandbox=True,
                          reason="has no sandbox: fallback_device or fallback_root is not set correctly")
    try:
        conn = hub().device(target)
        if conn is None and hub().seen(target):
            raise DeviceError(f"device {target} is not connected")
        if conn is None:
            conn = hub().find_device(target)
    except DeviceError as exc:
        why = f"is ambiguous ({exc})" if "several" in str(exc) else "is not connected"
        return Resolution(STATE_FAIL_CLOSED, sid, sandbox=True, reason=f"runs in the sandbox device {target}, which {why}")
    device_root = next((r for r in conn.roots if is_under(root, r)), None)
    if device_root is None:
        return Resolution(STATE_FAIL_CLOSED, sid, sandbox=True,
                          reason=f"runs in the sandbox device {conn.name} [{conn.device_id}], whose roots "
                                 f"({', '.join(conn.roots) or 'none'}) do not hold fallback_root {root}")
    lineage_root = res.root or sid
    directory = posixpath.join(root, sandbox_dir_name(lineage_root))
    generation = "sb" + hashlib.sha256(f"{conn.device_id}\0{directory}".encode()).hexdigest()[:10]
    hub().note_sandbox_dir(conn.device_id, directory, device_root)
    binding = Binding(session_id=lineage_root, device_id=conn.device_id, dir=directory, generation=generation,
                      bound_at=0.0)
    return Resolution(STATE_BOUND, sid, via=lineage_root, binding=binding, root=lineage_root, sandbox=True)


def where(res: Resolution) -> str:
    """How a bound resolution reads in messages."""
    b = res.binding
    if res.sandbox:
        return f"runs in the sandbox device {b.device_id if b else fallback_device()}"
    return f"is bound to device {b.device_id if b else '?'}"


def resolve_current() -> Resolution:
    return resolve_session(current_session_id())


def runs_on_pod(res: Resolution) -> bool:
    return res.state == STATE_NEVER_BOUND and fallback() == "local"


# --- device helpers -----------------------------------------------------------------------------

_MARK = "__HERMES_DEVICE_{}__"


def _sh(script: str, *args: str) -> str:
    """A command line that runs ``script`` with /bin/sh whatever the device user's shell is."""
    return "exec /bin/sh -c " + " ".join(shlex.quote(a) for a in (script, "sh", *args))


def run_script(device_id: str, script: str, args: Iterable[str], cwd: str, timeout: float = 30,
               stdin: Optional[str] = None) -> tuple[int, str, str]:
    """Run a /bin/sh script on the device. Returns ``(code, output after the marker, error)``.
    The marker keeps shell startup noise out of the result."""
    mark = _MARK.format(uuid.uuid4().hex[:12])
    full = f"printf '%s\\n' {shlex.quote(mark)}\n{script}"
    code, out, proc = hub().run(device_id, _sh(full, *args), cwd, timeout, stdin=stdin)
    idx = out.find(mark + "\n")
    body = out[idx + len(mark) + 1:] if idx >= 0 else ""
    err = proc.error or ("" if idx >= 0 else out.strip())
    return code, body, err


def device_pwd(h: Hub, device_id: str, directory: str) -> str:
    """The device's ``pwd -P`` of ``directory``. Raises :class:`BindError` 422 when refused."""
    code, out, proc = h.run(device_id, "pwd -P", directory)
    if code != 0:
        message = proc.error or out.strip() or f"exit {code}"
        if code == EXIT_LOST:
            raise BindError(409, "device_offline", message)
        raise BindError(422, "dir_refused", message)
    lines = [ln for ln in out.strip().splitlines() if ln.startswith("/")]
    if not lines:
        raise BindError(422, "dir_refused", f"device did not report a directory for {directory}")
    return posixpath.normpath(lines[-1])


_LIST_DIRS = r"""
cd -P -- "$1" 2>/dev/null || { echo "cannot enter $1" >&2; exit 2; }
pwd -P
for f in * .*; do
  case $f in .|..) continue ;; esac
  [ -d "$f" ] && printf '%s\0' "$f"
done
exit 0
"""


def list_device_dirs(device_id: str, path: str) -> tuple[str, list[str]]:
    """``(resolved path, subdirectory names)`` on the device; raises :class:`BindError`."""
    conn = hub().device(device_id)
    if conn is None:
        raise BindError(409, "device_offline", f"device {device_id} is not connected")
    path = path or (conn.roots[0] if conn.roots else "")
    if not posixpath.isabs(path):
        raise BindError(400, "bad_dir", "path must be an absolute device path")
    path = posixpath.normpath(path)
    code, body, err = run_script(device_id, _LIST_DIRS, [path], cwd=path)
    if code == EXIT_LOST:
        raise BindError(409, "device_offline", err or f"device {device_id} is not connected")
    if code != 0:
        raise BindError(422, "dir_refused", err or body.strip() or f"exit {code}")
    head, _, rest = body.partition("\n")
    names = sorted({n for n in rest.split("\0") if n and "/" not in n})
    return posixpath.normpath(head.strip() or path), names[:MAX_DIR_ENTRIES]


_RESOLVE = r"""
for p in "$@"; do
  n=0
  while [ -L "$p" ] && [ $n -lt 40 ]; do
    t=$(readlink -- "$p") || break
    case $t in /*) p=$t ;; *) p=$(dirname -- "$p")/$t ;; esac
    n=$((n+1))
  done
  d=$(dirname -- "$p"); b=$(basename -- "$p"); rest=""
  while [ ! -d "$d" ]; do rest=$(basename -- "$d")/$rest; d=$(dirname -- "$d"); done
  d=$(cd -P -- "$d" 2>/dev/null && pwd -P) || d=/
  case $b in /) printf '%s\0' "$d" ;; *) printf '%s\0' "${d%/}/$rest$b" ;; esac
done
"""


def device_realpaths(res: Resolution, paths: list[str]) -> list[str]:
    """Each absolute device path with symlinks resolved on the device (missing tails kept)."""
    if not paths:
        return []
    b = res.binding
    code, body, err = run_script(b.device_id, _RESOLVE, paths, cwd=b.dir)
    out = [posixpath.normpath(p) for p in body.split("\0")[:len(paths)] if p]
    if code != 0 or len(out) != len(paths):
        raise DeviceError(err or f"device {b.device_id} could not resolve {', '.join(paths)} (exit {code})")
    return out


_READ = r"""
p=$1; max=$2
if [ -d "$p" ]; then echo D; exit 0; fi
if [ ! -e "$p" ]; then echo N; exit 0; fi
if [ ! -f "$p" ]; then echo X; exit 0; fi
n=$(wc -c < "$p" 2>/dev/null | tr -d ' ') || exit 3
echo "F $n"
head -c "$max" < "$p" 2>/dev/null || exit 3
"""


@dataclass(frozen=True)
class DeviceFile:
    kind: str  # "file", "dir", "missing", "other"
    size: int = 0
    text: str = ""


def read_device_file(res: Resolution, path: str, max_bytes: int = MAX_READ_BYTES) -> DeviceFile:
    b = res.binding
    code, body, err = run_script(b.device_id, _READ, [path, str(max_bytes)], cwd=b.dir)
    head, _, rest = body.partition("\n")
    if code != 0 or not head:
        raise DeviceError(err or f"device {b.device_id} could not read {path} (exit {code})")
    if head == "D":
        return DeviceFile("dir")
    if head == "N":
        return DeviceFile("missing")
    if head == "X":
        return DeviceFile("other")
    if head.startswith("F "):
        try:
            size = int(head[2:].strip())
        except ValueError:
            size = len(rest)
        return DeviceFile("file", size, rest)
    raise DeviceError(f"device {b.device_id} sent an unreadable answer for {path}")


def active_conn(res: Resolution) -> DeviceConn:
    conn = hub().device(res.binding.device_id) if res.binding else None
    if conn is None:
        device = res.binding.device_id if res.binding else "?"
        raise DeviceError(f"device {device} is not connected; session {res.session_id} cannot reach its files")
    return conn


def device_abs_path(res: Resolution, path: str, *, cwd: Optional[str] = None) -> str:
    """``path`` as an absolute device path: ``~`` against the device home, relative paths against
    the session's device cwd."""
    path = (path or "").strip()
    if not path:
        raise DeviceError("empty path")
    if path == "~" or path.startswith("~/"):
        home = active_conn(res).home
        if not posixpath.isabs(home or ""):
            raise DeviceError("the device did not report its home directory")
        path = home + path[1:]
    elif path.startswith("~"):
        raise DeviceError(f"{path}: ~user paths are not supported on devices")
    if not posixpath.isabs(path):
        path = posixpath.join(cwd or hub().device_cwd(res.binding), path)
    return posixpath.normpath(path)


# --- device write denylist (section 8.6) --------------------------------------------------------

_PROBE_HOME = "/__hermes_device_probe_home__"


def device_denylist(home: str) -> tuple[set[str], list[str], set[str]]:
    """``(denied paths, denied prefixes, approval paths)`` of Hermes's write denylist rebuilt for
    a device home. Hermes's builders realpath on the server and add server HERMES_HOME files, so
    they are called with a probe home; entries under it are moved to the device home, /etc
    entries are kept, and the server's HERMES_HOME entries are dropped."""
    from agent.file_safety import build_write_approval_paths, build_write_denied_paths, build_write_denied_prefixes

    home = posixpath.normpath(home)

    def rebase(p: str) -> Optional[str]:
        if p == _PROBE_HOME or p.startswith(_PROBE_HOME + "/"):
            return home.rstrip("/") + p[len(_PROBE_HOME):]
        if p.startswith("/etc/"):
            return p
        return None

    denied = {q for p in build_write_denied_paths(_PROBE_HOME) if (q := rebase(p))}
    prefixes = [q for p in build_write_denied_prefixes(_PROBE_HOME) if (q := rebase(p))]
    approval = {q for p in build_write_approval_paths(_PROBE_HOME) if (q := rebase(p))}
    return denied, prefixes, approval


def device_write_denial(conn: DeviceConn, path: str) -> Optional[str]:
    """Why a write to the absolute device ``path`` is refused, or None."""
    path = posixpath.normpath(path)
    if not conn.within_roots(path):
        return f"{path} is outside the device roots ({', '.join(conn.roots) or 'none'})"
    if not posixpath.isabs(conn.home or ""):
        return "the device did not report its home directory, so its credential paths cannot be checked"
    denied, prefixes, approval = device_denylist(conn.home)
    if path in approval:
        return f"{path} needs approval on the device and file tools cannot ask for it here"
    if path in denied or any(path.startswith(p) or path + "/" == p for p in prefixes):
        return f"{path} is a protected credential or system path"
    return None


# --- attachment uploads (section 9) ------------------------------------------------------------

MAX_UPLOAD_BYTES = 32 * 1024 * 1024
# Each upload holds its file several times over in the gateway's memory, so only a few run at once.
MAX_UPLOADS = 2
UPLOADS = threading.BoundedSemaphore(MAX_UPLOADS)
MAX_UPLOAD_NAME = 120
UPLOAD_DIR = (".hermes", "attachments")
UPLOAD_TIMEOUT_S = 300

_UPLOAD_DIR_SCRIPT = r"""
umask 077
pwd -P
c=.
for part in "$@"; do
  c=$c/$part
  if [ -L "$c" ]; then echo "${c#./} is a symbolic link" >&2; exit 4; fi
  if [ -e "$c" ] && [ ! -d "$c" ]; then echo "${c#./} is not a folder" >&2; exit 4; fi
  [ -d "$c" ] || mkdir -- "$c" || exit 3
done
cd -P -- "$c" || exit 3
pwd -P
"""

_UPLOAD_WRITE_SCRIPT = r"""
want=$1; name=$2; stem=$3; ext=$4
[ "$(pwd -P)" = "$want" ] || { echo "the attachments folder moved" >&2; exit 4; }
if [ "$(printf 'YQ==' | base64 -d 2>/dev/null)" = a ]; then dec() { base64 -d; }
elif [ "$(printf 'YQ==' | base64 -D 2>/dev/null)" = a ]; then dec() { base64 -D; }
elif [ "$(printf 'YQ==' | openssl base64 -d -A 2>/dev/null)" = a ]; then dec() { openssl base64 -d -A; }
else echo "the device has no base64 decoder" >&2; exit 5; fi
umask 077
n=$name; i=2
while [ -e "$n" ] || [ -L "$n" ]; do
  [ "$i" -le 999 ] || { echo "too many files named $name" >&2; exit 6; }
  n=$stem-$i$ext; i=$((i+1))
done
set -C
exec 3> "$n" || { echo "could not create $n" >&2; exit 6; }
set +C
dec >&3 || { rm -f -- "$n"; echo "could not write $n" >&2; exit 5; }
exec 3>&-
printf '%s\n' "$n"
"""

_UPLOAD_NAME_BAD = re.compile(r"[\x00-\x1f\x7f/\\]+")


def upload_name(name: str) -> str:
    """A file name that is safe on any device: the last path part, without control characters,
    slashes or leading dots, at most MAX_UPLOAD_NAME bytes of UTF-8, else ``attachment``."""
    base = str(name or "").replace("\\", "/").rsplit("/", 1)[-1]
    clean = _UPLOAD_NAME_BAD.sub("_", base).strip().lstrip(".").strip()
    encoded = clean.encode("utf-8", "ignore")
    if len(encoded) > MAX_UPLOAD_NAME:
        stem, dot, ext = clean.rpartition(".")
        ext = dot + ext if dot and len(ext.encode()) <= 16 else ""
        keep = MAX_UPLOAD_NAME - len(ext.encode())
        clean = (stem if ext else clean).encode("utf-8", "ignore")[:keep].decode("utf-8", "ignore").rstrip() + ext
    return clean or "attachment"


def _outside_safe_root(path: str) -> Optional[str]:
    """Why HERMES_WRITE_SAFE_ROOT refuses a write to the device ``path``, or None (section 8.7)."""
    raw = os.environ.get("HERMES_WRITE_SAFE_ROOT", "")
    roots = [posixpath.normpath(os.path.expanduser(r)) for r in raw.split(os.pathsep) if r]
    if not roots or any(is_under(path, r) for r in roots):
        return None
    return f"{path} is outside HERMES_WRITE_SAFE_ROOT; add the device folder to device_write_roots"


def upload_resolution(session_id: str) -> Resolution:
    """The binding that serves a session's uploads: its own or an ancestor's, or the sandbox for a
    never-bound session with ``fallback: device``. Raises :class:`BindError` when no device serves it."""
    res = hub().resolve(session_id)
    if res.state == STATE_NEVER_BOUND:
        if fallback() == "local":
            raise BindError(409, "runs_on_pod", f"session {session_id} runs on the Hermes server, not on a device")
        if fallback() != "device":
            raise BindError(409, "fail_closed", f"session {session_id} is not bound to a device and runs nowhere")
        res = sandbox_resolution(res)
    if res.state != STATE_BOUND or res.binding is None:
        raise BindError(409, "fail_closed", fail_closed_message(res))
    return res


def put_session_file(session_id: str, name: str, data: bytes) -> dict:
    """Write an attached file into ``.hermes/attachments`` of the session's device directory and
    return ``{"path", "name", "device_id", "bytes"}``. The folder and every part of it must be a
    real folder inside the binding directory, and the file must pass the write rules of file tools
    (device roots, credential denylist, HERMES_WRITE_SAFE_ROOT). An existing file is never
    overwritten. Raises :class:`BindError`."""
    import base64

    if len(data) > MAX_UPLOAD_BYTES:
        raise BindError(413, "too_large", f"the file is larger than {MAX_UPLOAD_BYTES // (1024 * 1024)} MB")
    res = upload_resolution(session_id)
    b = res.binding
    conn = hub().device(b.device_id)
    if conn is None:
        raise BindError(409, "device_offline", f"device {b.device_id} is not connected")
    clean = upload_name(name)
    code, body, err = run_script(b.device_id, _UPLOAD_DIR_SCRIPT, UPLOAD_DIR, cwd=b.dir)
    if code == EXIT_LOST:
        raise BindError(409, "device_offline", err or f"device {b.device_id} is not connected")
    lines = [ln for ln in body.splitlines() if ln.startswith("/")]
    if code != 0 or len(lines) != 2:
        raise BindError(422, "write_refused", err or body.strip() or f"exit {code}")
    root, folder = (posixpath.normpath(ln) for ln in lines)
    if folder != posixpath.join(root, *UPLOAD_DIR):
        raise BindError(422, "write_refused", f"{folder} is not inside the session folder {b.dir}")
    target = posixpath.join(folder, clean)
    why = device_write_denial(conn, target) or _outside_safe_root(target)
    if why:
        raise BindError(422, "write_refused", why)
    stem, dot, ext = clean.rpartition(".")
    stem, ext = (stem, dot + ext) if dot and stem else (clean, "")
    code, body, err = run_script(b.device_id, _UPLOAD_WRITE_SCRIPT, [folder, clean, stem, ext], cwd=folder,
                                 timeout=UPLOAD_TIMEOUT_S, stdin=base64.b64encode(data).decode("ascii"))
    if code == EXIT_LOST:
        raise BindError(409, "device_offline", err or f"device {b.device_id} is not connected")
    written = body.strip().splitlines()[-1] if body.strip() else ""
    if code != 0 or not written or "/" in written:
        raise BindError(422, "write_refused", err or body.strip() or f"exit {code}")
    return {"path": posixpath.join(folder, written), "name": written, "device_id": b.device_id, "bytes": len(data)}


# --- terminal backend (section 8.4) -----------------------------------------------------------

_EXEC_STATE: contextvars.ContextVar[Optional[dict]] = contextvars.ContextVar("hermes_device_exec", default=None)

from tools.environments.base import BaseEnvironment  # noqa: E402  (Hermes runtime import)


def _scope_terminal_cwd() -> str:
    try:
        from agent.runtime_cwd import scope_terminal_cwd
        return scope_terminal_cwd()
    except Exception:
        return ""


def _pod_paths(extra: Iterable[str] = ()) -> list[str]:
    out = []
    for p in (placeholder(), terminal_cwd(), _scope_terminal_cwd(), *extra):
        if isinstance(p, str) and p and posixpath.isabs(os.path.expanduser(p)):
            out.append(posixpath.normpath(os.path.expanduser(p)))
    return out


def is_pod_path(path: str, extra: Iterable[str] = ()) -> bool:
    """True for paths that name server directories (the placeholder, terminal.cwd, TERMINAL_CWD,
    the env's creation cwd), which must never be sent to a device."""
    if not path or not posixpath.isabs(path):
        return True
    candidates = {posixpath.normpath(path)}
    try:
        candidates.add(os.path.realpath(path))
    except (OSError, ValueError):
        pass
    return any(is_under(c, p) for c in candidates for p in _pod_paths(extra))


class DeviceShell(BaseEnvironment):
    """Runs commands for one binding on its device, with Hermes's snapshot and cwd tracking."""

    def __init__(self, binding: Binding, conn: DeviceConn, timeout: int, pod_cwd: str = ""):
        self._binding = binding
        self._device_tmp = conn.tmp_dir if posixpath.isabs(conn.tmp_dir or "") else "/tmp"  # no-tmp: ok — device-side
        self._conn_id = conn.conn_id
        self._pod_cwd = pod_cwd
        super().__init__(cwd=hub().device_cwd(binding), timeout=timeout)
        self.init_session()

    @property
    def binding(self) -> Binding:
        return self._binding

    def get_temp_dir(self) -> str:
        return self._device_tmp

    def refresh(self, conn: DeviceConn) -> None:
        """A reconnected device may have lost the snapshot file: take a new one."""
        if conn.conn_id != self._conn_id:
            self._conn_id = conn.conn_id
            if posixpath.isabs(conn.tmp_dir or ""):
                self._device_tmp = conn.tmp_dir
                temp_dir = self._device_tmp.rstrip("/") or "/"
                self._snapshot_path = f"{temp_dir}/hermes-snap-{self._session_id}.sh"
                self._cwd_file = f"{temp_dir}/hermes-cwd-{self._session_id}.txt"
            self._snapshot_ready = False
            self._prefer_nonlogin = False
            self.init_session()

    def _safe_cwd(self) -> str:
        cwd = self.cwd or ""
        return self._binding.dir if is_pod_path(cwd, [self._pod_cwd]) else cwd

    def map_cwd(self, cwd: str) -> str:
        if not cwd or is_pod_path(cwd, [self._pod_cwd]):
            return self._safe_cwd()
        return posixpath.normpath(cwd)

    def execute(self, command: str, cwd: str = "", **kwargs) -> dict:
        effective = self.map_cwd(cwd)
        state = {"cwd": effective, "proc": None}
        token = _EXEC_STATE.set(state)
        try:
            result = super().execute(command, effective, **kwargs)
        finally:
            _EXEC_STATE.reset(token)
        proc = state["proc"]
        if proc is not None and proc.refused:
            self.cwd = self._binding.dir
        self.cwd = self._safe_cwd()
        hub().set_device_cwd(self._binding, self.cwd)
        return result

    def _run_bash(self, cmd_string: str, *, login: bool = False, timeout: int = 120,
                  stdin_data: Optional[str] = None):
        state = _EXEC_STATE.get()
        cwd = state["cwd"] if state else self._safe_cwd()
        proc = hub().start(self._binding.device_id, cmd=cmd_string, cwd=cwd, login=login,
                           timeout=float(timeout), stdin=stdin_data)
        if state is not None:
            state["proc"] = proc
        return proc

    def cleanup(self):
        if hub().device(self._binding.device_id) is None:
            return
        quoted = " ".join(shlex.quote(p) for p in (self._snapshot_path, self._cwd_file))
        try:
            hub().start(self._binding.device_id, cmd=f"rm -f {quoted}", cwd=self._binding.dir, login=False,
                        timeout=10, stdin=None)
        except Exception:
            logger.debug("device: snapshot cleanup failed", exc_info=True)


def _default_local_factory(cwd: str, timeout: int):
    from tools.environments.local import LocalEnvironment
    try:
        from tools.terminal_tool import _get_env_config
        persistent = _get_env_config().get("local_persistent", True)
    except Exception:
        persistent = True
    return LocalEnvironment(cwd=cwd, timeout=timeout, persistent=persistent)


class DeviceRouterEnvironment(BaseEnvironment):
    """The ``device`` terminal backend (section 8.4). Each command goes to the child that the
    session's binding resolution picks: a device shell for a bound session, a lazily created
    Hermes LocalEnvironment for a never-bound one, or a refusal.

    The class name avoids the backend hints of tools/file_tools_paths.py, so Hermes identifies
    it by its stamped backend name.
    """

    def __init__(self, cwd: str, timeout: int, task_id: str = "default",
                 local_factory: Optional[Callable[[str, int], Any]] = None):
        self._ready = False
        self._pod_cwd = cwd
        self._local = None
        self._local_lock = threading.Lock()
        self._shells: dict[tuple[str, str], DeviceShell] = {}
        self._shells_lock = threading.Lock()
        self._local_factory = local_factory or _default_local_factory
        self.task_id = task_id
        super().__init__(cwd=cwd, timeout=timeout)
        self._ready = True

    def _resolve(self) -> Resolution:
        return resolve_current()

    def _local_env(self):
        with self._local_lock:
            if self._local is None:
                self._local = self._local_factory(self._pod_cwd, self.timeout)
            return self._local

    def _shell(self, res: Resolution) -> DeviceShell | dict:
        b = res.binding
        conn = hub().device(b.device_id)
        if conn is None:
            if res.sandbox:
                return {"output": f"[the sandbox device {b.device_id} is not connected; session {res.session_id} "
                                  f"runs there ({b.dir}), so the command did not run. It never runs on the "
                                  f"server.]", "returncode": EXIT_LOST}
            return {"output": f"[device {b.device_id} is not connected; session {res.session_id} is bound to it "
                              f"({b.dir}), so the command did not run]", "returncode": EXIT_LOST}
        key = (b.session_id, b.generation)
        with self._shells_lock:
            shell = self._shells.get(key)
            if shell is None:
                for k in [k for k in self._shells if k[0] == b.session_id]:
                    self._shells.pop(k).cleanup()
                shell = DeviceShell(b, conn, self.timeout, pod_cwd=self._pod_cwd)
                self._shells[key] = shell
            else:
                shell.refresh(conn)
        return shell

    @property
    def cwd(self) -> str:
        if not getattr(self, "_ready", False):
            return getattr(self, "_pod_cwd", "")
        res = self._resolve()
        if res.state == STATE_BOUND:
            return hub().device_cwd(res.binding)
        if runs_on_pod(res) and self._local is not None:
            return self._local.cwd
        return self._pod_cwd

    @cwd.setter
    def cwd(self, value: str) -> None:
        if not getattr(self, "_ready", False):
            self._pod_cwd = value
            return
        res = self._resolve()
        if res.state == STATE_BOUND:
            if isinstance(value, str) and posixpath.isabs(value) and not is_pod_path(value, [self._pod_cwd]):
                hub().set_device_cwd(res.binding, posixpath.normpath(value))
        elif runs_on_pod(res) and self._local is not None:
            self._local.cwd = value
        else:
            self._pod_cwd = value

    @property
    def uses_host_filesystem(self) -> bool:
        if not getattr(self, "_ready", False):
            return False
        from tools.environments.local import LocalEnvironment
        return runs_on_pod(self._resolve()) and LocalEnvironment.uses_host_filesystem is True

    @property
    def is_local(self) -> bool:
        return self.uses_host_filesystem

    def get_temp_dir(self) -> str:
        if not getattr(self, "_ready", False):
            return tempfile.gettempdir()
        res = self._resolve()
        if res.state == STATE_BOUND:
            conn = hub().device(res.binding.device_id)
            if conn is not None and posixpath.isabs(conn.tmp_dir or ""):
                return conn.tmp_dir
            return "/tmp"  # no-tmp: ok — device-side
        if runs_on_pod(res) and self._local is not None:
            return self._local.get_temp_dir()
        return tempfile.gettempdir()

    def init_session(self):
        return None

    def execute(self, command: str, cwd: str = "", **kwargs) -> dict:
        res = self._resolve()
        if res.state == STATE_BOUND:
            child = self._shell(res)
            return child if isinstance(child, dict) else child.execute(command, cwd, **kwargs)
        if runs_on_pod(res):
            return self._local_env().execute(command, cwd, **kwargs)
        return {"output": refusal_message(res), "returncode": 1}

    def _run_bash(self, cmd_string: str, *, login: bool = False, timeout: int = 120,
                  stdin_data: Optional[str] = None):
        res = self._resolve()
        if res.state == STATE_BOUND:
            child = self._shell(res)
            if isinstance(child, dict):
                return finished_process(child["returncode"], child["output"])
            return child._run_bash(cmd_string, login=login, timeout=timeout, stdin_data=stdin_data)
        if runs_on_pod(res):
            return self._local_env()._run_bash(cmd_string, login=login, timeout=timeout, stdin_data=stdin_data)
        return finished_process(1, refusal_message(res))

    def cleanup(self):
        with self._shells_lock:
            shells = list(self._shells.values())
            self._shells.clear()
        for shell in shells:
            shell.cleanup()
        if self._local is not None:
            self._local.cleanup()


# --- server-side readers (section 8.5) ----------------------------------------------------------

def read_text_for_edit(res: Resolution, path: str) -> Optional[str]:
    """``_read_text_if_exists`` for a bound session: None when missing, raises OSError when the
    path is not a file or the device cannot be read."""
    try:
        active_conn(res)
        f = read_device_file(res, device_abs_path(res, path))
    except DeviceError as exc:
        raise OSError(f"hermes-device: cannot read {path} on the device: {exc}") from exc
    if f.kind == "missing":
        return None
    if f.kind != "file":
        raise OSError(f"Cannot edit non-file path: {path}")
    if f.size > MAX_READ_BYTES:
        raise OSError(f"hermes-device: {path} is larger than {MAX_READ_BYTES} bytes")
    return f.text[1:] if f.text.startswith(_BOM) else f.text


def _sensitive_device_path(conn: DeviceConn, path: str) -> bool:
    from agent.context_references import _SENSITIVE_HOME_FILES
    from agent.file_safety import HOME_CREDENTIAL_DIRS
    home = posixpath.normpath(conn.home or "/")
    exact = {posixpath.join(home, str(p)) for p in _SENSITIVE_HOME_FILES}
    dirs = [posixpath.join(home, d) for d in HOME_CREDENTIAL_DIRS]
    denied, prefixes, _approval = device_denylist(home)
    return path in exact or path in denied or any(is_under(path, d) for d in dirs) \
        or any(path.startswith(p) for p in prefixes)


_FENCE = {".py": "python", ".js": "javascript", ".ts": "typescript", ".tsx": "tsx", ".jsx": "jsx",
          ".json": "json", ".md": "markdown", ".sh": "bash", ".yml": "yaml", ".yaml": "yaml", ".toml": "toml"}


def _tokens(text: str) -> int:
    from agent.model_metadata import estimate_tokens_rough
    return estimate_tokens_rough(text)


def _chars_per_token() -> int:
    from agent.model_metadata import CHARS_PER_TOKEN
    return CHARS_PER_TOKEN


_LISTING = r"""
find "$1" -mindepth 1 -maxdepth 3 -name .git -prune -o -print 2>/dev/null | head -n "$2" | while IFS= read -r f; do
  if [ -d "$f" ]; then printf 'd %s\n' "$f"; else printf 'f %s\n' "$f"; fi
done
"""


def device_folder_listing(res: Resolution, path: str, limit: int = 200) -> str:
    code, body, err = run_script(res.binding.device_id, _LISTING, [path, str(limit)], cwd=res.binding.dir)
    if code != 0:
        raise DeviceError(err or f"device listing of {path} failed (exit {code})")
    lines = [f"{path}/"]
    entries = sorted(ln for ln in body.splitlines() if ln[:2] in ("d ", "f "))
    for ln in entries:
        kind, full = ln[0], ln[2:]
        depth = posixpath.relpath(full, path).count("/")
        name = posixpath.basename(full)
        lines.append(f"{'  ' * depth}- {name}/" if kind == "d" else f"{'  ' * depth}- {name}")
    if len(entries) >= limit:
        lines.append("- ...")
    return "\n".join(lines)


def expand_device_path(res: Resolution, raw: str, kind: str, target: str, line_start: Optional[int] = None,
                       line_end: Optional[int] = None,
                       max_inline_tokens: Optional[int] = None) -> tuple[Optional[str], Optional[str]]:
    """``(warning, block)`` for a file or folder reference read from the device. ``kind`` is
    file, folder or any (``@device:``)."""
    conn = active_conn(res)
    root = res.binding.dir
    path = device_abs_path(res, target)
    real = device_realpaths(res, [path])[0]
    if not is_under(real, root):
        return f"{raw}: {path} is outside the session's device directory {root}", None
    if _sensitive_device_path(conn, real):
        return f"{raw}: path is a sensitive credential path and cannot be attached", None
    cap = MAX_READ_BYTES if max_inline_tokens is None else max_inline_tokens * _chars_per_token() + 1
    f = read_device_file(res, real, max_bytes=cap)
    if f.kind == "missing":
        return f"{raw}: {'file' if kind == 'any' else kind} not found on the device", None
    if kind == "folder" or (kind == "any" and f.kind == "dir"):
        if f.kind != "dir":
            return f"{raw}: path is not a folder", None
        listing = device_folder_listing(res, real)
        return None, f"📁 {raw} ({_tokens(listing)} tokens)\n{listing}"
    if f.kind != "file":
        return f"{raw}: path is not a file", None
    binary = "\x00" in f.text[:8192]
    if f.size > cap or binary:
        why = "binary" if binary else f"too large to inline ({f.size} bytes)"
        return None, (f"📄 {raw}: {why}; it is on the device at {real}, where the agent's tools run. "
                      f"Read it with the file tools instead.")
    text = f.text[1:] if f.text.startswith(_BOM) else f.text
    if line_start is not None:
        lines = text.splitlines(keepends=True)
        text = "".join(lines[max(line_start - 1, 0):(line_end or line_start)])
    tokens = _tokens(text)
    if max_inline_tokens is not None and tokens > max_inline_tokens:
        return None, (f"📄 {raw}: {tokens} tokens, too large to inline; it is on the device at {real}. "
                      f"Read it with the file tools instead.")
    lang = _FENCE.get(posixpath.splitext(real)[1].lower(), "")
    return None, f"📄 {raw} ({tokens} tokens)\n```{lang}\n{text}\n```"


_GIT = r"""
{ env GIT_TERMINAL_PROMPT=0 GIT_PAGER=cat PAGER=cat GIT_CONFIG_NOSYSTEM=1 GCM_INTERACTIVE=Never \
    git -c core.fsmonitor=false -c core.hooksPath=/dev/null --no-pager "$@" 2>&1; echo "__RC__$?"; } | head -c 4194400
"""


def _expand_device_git(res: Resolution, raw: str, args: list[str], label: str) -> tuple[Optional[str], Optional[str]]:
    from hermes_cli._subprocess_compat import harden_git_argv
    cwd = hub().device_cwd(res.binding)
    code, body, err = run_script(res.binding.device_id, _GIT, harden_git_argv(args), cwd=cwd, timeout=30)
    if code != 0 and "__RC__" not in body:
        return f"{raw}: {err or 'git command failed on the device'}", None
    out, sep, rc = body.rpartition("__RC__")
    if not sep:
        return f"{raw}: git output on the device is larger than 4 MiB", None
    if rc.strip() != "0":
        return f"{raw}: {out.strip() or 'git command failed'}", None
    content = out.strip() or "(no output)"
    return None, f"🧾 {label} ({_tokens(content)} tokens)\n```diff\n{content}\n```"


def expand_reference_on_device(res: Resolution, ref: Any, max_inline_tokens: Optional[int]):
    """A built-in ``@file``/``@folder``/``@diff``/``@staged``/``@git`` reference served from the
    device of a bound session."""
    try:
        if ref.kind in ("file", "folder"):
            return expand_device_path(res, ref.raw, ref.kind, ref.target, ref.line_start, ref.line_end,
                                      max_inline_tokens)
        from agent.context_references import _GIT_REFERENCE_ARGS
        args = _GIT_REFERENCE_ARGS[ref.kind](ref)
        return _expand_device_git(res, ref.raw, args, "git " + " ".join(args))
    except Exception as exc:
        return f"{ref.raw}: {exc}", None


# --- Hermes hook installation -------------------------------------------------------------------

_WRAPPED_MARK = "__hermes_device_original__"
DEVICE_REFERENCE_KINDS = ("file", "folder", "diff", "staged", "git")


def _wrap(owner: Any, name: str, make: Callable[[Callable], Callable]) -> bool:
    original = getattr(owner, name)
    if getattr(original, _WRAPPED_MARK, None) is not None:
        return False
    wrapped = make(original)
    setattr(wrapped, _WRAPPED_MARK, original)
    wrapped.__name__ = getattr(original, "__name__", name)
    wrapped.__qualname__ = getattr(original, "__qualname__", name)
    wrapped.__doc__ = getattr(original, "__doc__", None)
    setattr(owner, name, wrapped)
    return True


def _make_expand_reference(original):
    async def _expand_reference(ref, cwd, *args, **kwargs):
        if getattr(ref, "kind", None) not in DEVICE_REFERENCE_KINDS:
            return await original(ref, cwd, *args, **kwargs)
        res = await asyncio.to_thread(resolve_session, current_session_id(), str(cwd))
        if runs_on_pod(res):
            return await original(ref, cwd, *args, **kwargs)
        if res.state != STATE_BOUND:
            return f"{ref.raw}: {refusal_message(res)}", None
        return await asyncio.to_thread(expand_reference_on_device, res, ref, kwargs.get("max_inline_tokens"))
    return _expand_reference


def _inbound_session_id(runner, source, session_key) -> str:
    try:
        from gateway.config import Platform
        if source is not None and source.platform == Platform.LOCAL:
            return str(source.chat_id or "")
    except Exception:
        logger.debug("device: LOCAL check failed", exc_info=True)
    try:
        return str(runner.session_store.peek_session_id(session_key) or "")
    except Exception:
        logger.debug("device: session id for %s not found", session_key, exc_info=True)
        return ""


def _make_inbound_references(original):
    async def inbound_references(runner, source, session_key, text):
        token = SESSION_OVERRIDE.set(_inbound_session_id(runner, source, session_key))
        try:
            return await original(runner, source, session_key, text)
        finally:
            SESSION_OVERRIDE.reset(token)
    return inbound_references


def _make_managed_turn_text(original):
    async def managed_turn_text(authority, row, policy):
        sid = str((row or {}).get("target_session_id") or "")
        token = SESSION_OVERRIDE.set(sid)
        try:
            if getattr(policy, "ignore_user_config", False):
                res = await asyncio.to_thread(resolve_session, sid, str(getattr(policy, "cwd", "") or ""))
                if not runs_on_pod(res):
                    head = (f"hermes-device: session {sid} {where(res)}, so this does not run on the server."
                            if res.state == STATE_BOUND else refusal_message(res))
                    return None, (f"{head} Safe-mode and config-bypass sessions do not load the "
                                  f"device plugin, so they cannot serve device sessions.")
            return await original(authority, row, policy)
        finally:
            SESSION_OVERRIDE.reset(token)
    return managed_turn_text


def _make_required_path(original):
    def _required_path(arguments):
        res = resolve_current()
        if runs_on_pod(res):
            return original(arguments)
        path = str((arguments or {}).get("path") or "")
        if not path:
            raise ValueError("path required")
        if res.state != STATE_BOUND:
            raise ValueError(refusal_message(res))
        try:
            return device_abs_path(res, path)
        except DeviceError as exc:
            raise ValueError(str(exc)) from exc
    return _required_path


def _make_read_text_if_exists(original):
    def _read_text_if_exists(path):
        res = resolve_current()
        if runs_on_pod(res):
            return original(path)
        if res.state != STATE_BOUND:
            raise OSError(refusal_message(res))
        return read_text_for_edit(res, str(path))
    return _read_text_if_exists


def _make_ensure_checkpoint(original):
    def ensure_checkpoint(self, working_dir, reason="auto"):
        if not runs_on_pod(resolve_current()):
            return False
        return original(self, working_dir, reason)
    return ensure_checkpoint


def _make_init_session_state(original):
    def _init_session_state(agent, session_id, session_db, parent_session_id, *args, **kwargs):
        result = original(agent, session_id, session_db, parent_session_id, *args, **kwargs)
        try:
            hub().note_parent(str(getattr(agent, "session_id", "") or ""),
                              str(parent_session_id) if parent_session_id else None)
        except Exception:
            logger.debug("device: could not record a parent link", exc_info=True)
        return result
    return _init_session_state


def _make_side_spawn(original):
    def _spawn(authority, ref, task_id, *args, **kwargs):
        _link_side_agent(task_id, lambda: _physical_session(authority, ref))
        return original(authority, ref, task_id, *args, **kwargs)
    return _spawn


def _physical_session(authority, ref) -> str:
    try:
        return str(authority.physical_target(ref) or ref.session_id)
    except Exception:
        return str(getattr(ref, "session_id", "") or "")


def _link_side_agent(task_id: str, parent: Callable[[], str]) -> None:
    """Record that side agent ``task_id`` belongs to the session that started it; when that
    session cannot be found, the side agent fails closed."""
    try:
        parent_id = parent()
    except Exception:
        logger.debug("device: starting session of %s not found", task_id, exc_info=True)
        parent_id = ""
    if parent_id:
        hub().note_parent(str(task_id), parent_id)
    else:
        hub().note_orphan(str(task_id), "is a background task whose starting session could not be found")


def _make_run_background_task(original):
    async def _run_background_task(self, prompt, source, task_id, *args, **kwargs):
        _link_side_agent(task_id, lambda: _inbound_session_id(self, source, self._session_key_for_source(source)))
        return await original(self, prompt, source, task_id, *args, **kwargs)
    return _run_background_task


HOOK_POINTS: tuple[tuple[str, str, str, Callable], ...] = (
    ("agent.context_references", "", "_expand_reference", _make_expand_reference),
    ("gateway.session_context_refs", "", "inbound_references", _make_inbound_references),
    ("gateway.session_context_refs", "", "managed_turn_text", _make_managed_turn_text),
    ("acp_adapter.edit_approval", "", "_required_path", _make_required_path),
    ("acp_adapter.edit_approval", "", "_read_text_if_exists", _make_read_text_if_exists),
    ("tools.checkpoint_manager", "CheckpointManager", "ensure_checkpoint", _make_ensure_checkpoint),
    ("agent.agent_init", "", "_init_session_state", _make_init_session_state),
    ("gateway.session_tui_side", "", "_spawn", _make_side_spawn),
    ("gateway.run_background", "GatewayBackgroundMixin", "_run_background_task", _make_run_background_task),
)


def install_hooks() -> list[str]:
    """Wrap the Hermes internals in :data:`HOOK_POINTS`. Returns the names it could not wrap."""
    import importlib
    missing = []
    for module_name, cls, name, make in HOOK_POINTS:
        label = f"{module_name}.{cls + '.' if cls else ''}{name}"
        try:
            owner = importlib.import_module(module_name)
            if cls:
                owner = getattr(owner, cls)
            _wrap(owner, name, make)
        except Exception:
            logger.error("device: cannot wrap %s; device sessions may read server files there", label,
                         exc_info=True)
            missing.append(label)
    return missing


# --- pre_tool_call guard (section 8.6) ----------------------------------------------------------

_V4A_HEADER = re.compile(r"^(\*\*\*\s*(?:Update|Add|Delete)\s+File:\s*)(.+?)(\s*)$")
_V4A_MOVE = re.compile(r"^(\*\*\*\s*Move\s+File:\s*)(.+?)(\s*->\s*)(.+?)(\s*)$")


def _v4a_paths(body: str) -> Optional[list[str]]:
    """The paths a V4A patch touches, as Hermes's own parser reads them; None on a parse error."""
    from tools.patch_parser import parse_v4a_patch
    ops, error = parse_v4a_patch(body)
    if error:
        return None
    paths = []
    for op in ops:
        paths.append(op.file_path)
        if op.new_path:
            paths.append(op.new_path)
    return paths


def _rewrite_v4a(body: str, mapping: dict[str, str]) -> str:
    """``body`` with each file header's path replaced through ``mapping``."""
    out = []
    for line in body.split("\n"):
        m = _V4A_MOVE.match(line)
        if m and m.group(2).strip() in mapping and m.group(4).strip() in mapping:
            line = f"{m.group(1)}{mapping[m.group(2).strip()]}{m.group(3)}{mapping[m.group(4).strip()]}{m.group(5)}"
        else:
            m = _V4A_HEADER.match(line)
            if m and m.group(2).strip() in mapping:
                line = f"{m.group(1)}{mapping[m.group(2).strip()]}{m.group(3)}"
        out.append(line)
    return "\n".join(out)


def _record_cwd(task_id: str, cwd: str) -> None:
    from tools.terminal_tool import get_session_cwd, record_session_cwd
    keys = {task_id or "default"}
    try:
        from tools.approval import get_current_session_key
        key = get_current_session_key(default="")
        if key:
            keys.add(key)
    except Exception:
        logger.debug("device: no session key", exc_info=True)
    for key in keys:
        if get_session_cwd(key) != cwd:
            record_session_cwd(key, cwd)


def _block(message: str) -> dict:
    return {"action": "block", "message": message}


def pre_tool_call(tool_name: str = "", args: Optional[dict] = None, task_id: str = "", session_id: str = "",
                  **_kwargs) -> Optional[dict]:
    """The section 8.6 guard. Any exception blocks the call: Hermes runs a tool whose hook raised."""
    try:
        return _guard(tool_name, args if isinstance(args, dict) else {}, task_id)
    except Exception as exc:
        logger.warning("device: pre_tool_call guard failed for %s", tool_name, exc_info=True)
        return _block(f"hermes-device: {tool_name} refused because the device check failed: {exc}")


def _guard(tool_name: str, args: dict, task_id: str) -> Optional[dict]:
    res = resolve_current()
    if runs_on_pod(res):
        return None
    if res.state != STATE_BOUND:
        if tool_name in FILE_AND_COMMAND_TOOLS or tool_name in POD_ONLY_TOOLS:
            return _block(refusal_message(res))
        return None
    b = res.binding
    if tool_name in POD_ONLY_TOOLS:
        return _block(f"hermes-device: session {res.session_id} {where(res)}; {tool_name} "
                      f"creates work that runs on the server, so it is refused for device sessions.")
    from tools.file_tools_paths import _terminal_env_type_for_task
    env_type = _terminal_env_type_for_task(task_id or "default")
    if env_type != PLUGIN_ID:
        return _block(f"hermes-device: session {res.session_id} {where(res)}, but its terminal "
                      f"backend is {env_type!r}, not {PLUGIN_ID!r}, so tools would run on the server. Start a new "
                      f"session with terminal.backend: {PLUGIN_ID}.")
    cwd = hub().device_cwd(b)
    _record_cwd(task_id, cwd)
    if tool_name not in FILE_PATH_TOOLS:
        return None
    v4a = tool_name == "patch" and args.get("mode", "replace") == "patch"
    if v4a:
        body = args.get("patch")
        paths = _v4a_paths(body) if isinstance(body, str) else None
        if paths is None:
            return None
    elif tool_name == "search_files" and not str(args.get("path") or "").strip():
        paths = ["."]
    else:
        paths = [str(args.get("path") or "")]
    paths = [p.strip() for p in paths if isinstance(p, str) and p.strip()]
    if not paths:
        return None
    conn = active_conn(res)
    try:
        absolute = {p: device_abs_path(res, p, cwd=cwd) for p in paths}
    except DeviceError as exc:
        return _block(f"hermes-device: {tool_name} refused: {exc}")
    if tool_name in ("write_file", "patch"):
        targets = list(dict.fromkeys(absolute.values()))
        resolved = device_realpaths(res, targets)
        for a, r in zip(targets, resolved):
            for candidate in dict.fromkeys((a, r)):
                reason = device_write_denial(conn, candidate)
                if reason:
                    return _block(f"hermes-device: {tool_name} to {a} refused on device {conn.name}: {reason}")
    if v4a:
        rewritten = _rewrite_v4a(args["patch"], absolute)
        if _v4a_paths(rewritten) != [absolute[p] for p in paths]:
            return _block("hermes-device: could not rewrite the V4A patch paths to device paths; use absolute "
                          "device paths in the patch.")
        return None if rewritten == args["patch"] else {"action": "modify", "args": {"patch": rewritten}}
    new_path = absolute[paths[0]]
    return None if new_path == args.get("path") else {"action": "modify", "args": {"path": new_path}}


# --- HERMES_WRITE_SAFE_ROOT (section 8.7) -------------------------------------------------------

def pod_writable_paths() -> list[str]:
    paths = ["/opt/data", str(hermes_home()), tempfile.gettempdir(),
             "/tmp", "/var/tmp", "/dev/shm",  # no-tmp: ok — server paths compared, not written
             placeholder(), terminal_cwd()]
    out: list[str] = []
    for p in paths:
        if not p or not posixpath.isabs(os.path.expanduser(p)):
            continue
        for q in (posixpath.normpath(os.path.expanduser(p)), os.path.realpath(os.path.expanduser(p))):
            if q not in out:
                out.append(q)
    return out


def apply_safe_root_policy(environ=None) -> list[str]:
    """Remove from HERMES_WRITE_SAFE_ROOT every device_write_roots entry that equals, contains or
    lies inside a pod-writable path. Returns the removed entries."""
    environ = os.environ if environ is None else environ
    raw = environ.get("HERMES_WRITE_SAFE_ROOT", "")
    if not raw:
        return []
    pod = pod_writable_paths()
    bad = set()
    roots = device_write_roots()
    if fallback() == "device" and fallback_root():
        roots.append(fallback_root())
    for root in roots:
        norm = posixpath.normpath(os.path.expanduser(root))
        hit = next((p for p in pod if is_under(norm, p) or is_under(p, norm)), None)
        if hit:
            logger.error("device: device_write_roots entry %s overlaps the server path %s; removed from "
                         "HERMES_WRITE_SAFE_ROOT so server sessions cannot write there", root, hit)
            bad.add(norm)
    if not bad:
        return []
    kept, removed = [], []
    for entry in raw.split(os.pathsep):
        if entry and posixpath.normpath(os.path.expanduser(entry)) in bad:
            removed.append(entry)
        else:
            kept.append(entry)
    environ["HERMES_WRITE_SAFE_ROOT"] = os.pathsep.join(kept)
    return removed


# --- /device command (section 8.8) --------------------------------------------------------------

USAGE = ("Usage: /device shows this session's binding; /device <device name or id> <dir> binds it; "
         "/device off unbinds it; /device list lists connected devices.")


def device_command(raw_args: str = "", session_id: str = "", **_kwargs) -> str:
    h = hub()
    sid = session_id or current_session_id()
    args = (raw_args or "").strip().split(maxsplit=1)
    if not args:
        res = h.resolve(sid)
        if res.state == STATE_NEVER_BOUND and fallback() == "device":
            res = resolve_session(sid)
            if res.state == STATE_BOUND and res.sandbox:
                conn = h.device(res.binding.device_id)
                return (f"Session {sid} is not bound to a device, so it runs in the sandbox device "
                        f"{conn.name if conn else '?'} [{res.binding.device_id}] in {res.binding.dir}. Bind it to "
                        f"one of your devices with /device <device> <dir>.")
        if res.state == STATE_BOUND:
            b = res.binding
            conn = h.device(b.device_id)
            state = f"connected as {conn.name}" if conn else "offline"
            via = "" if res.via == sid else f" (inherited from session {res.via})"
            return f"Session {sid} runs on device {b.device_id} ({state}) in {b.dir}{via}."
        if res.state == STATE_FAIL_CLOSED:
            return fail_closed_message(res)
        return f"Session {sid or '(none)'} is not bound to a device. {USAGE}"
    if args[0] == "list" and len(args) == 1:
        conns = h.devices()
        if not conns:
            return "No devices are connected."
        return "\n".join(f"{c.name} [{c.device_id}] on {c.hostname or '?'} ({c.os}/{c.arch}), roots: "
                         f"{', '.join(c.roots) or 'none'}" for c in sorted(conns, key=lambda c: c.name))
    if args[0] == "off" and len(args) == 1:
        res = h.resolve(sid)
        target = res.via if res.state == STATE_BOUND else sid
        try:
            removed = h.unbind(target) if target else False
        except DeviceError as exc:
            return f"Not unbound: {exc}"
        if not removed:
            if res.state == STATE_NEVER_BOUND and fallback() == "device":
                return "No device was bound. This session runs in the sandbox device until you bind it."
            return "No device was bound."
        return (f"Device binding of session {target} removed. This session now fails closed until you bind it "
                f"again with /device <device> <dir>.")
    if len(args) != 2:
        return USAGE
    if not sid:
        return "Not bound: this surface did not give a session id."
    try:
        conn = h.find_device(args[0])
        b = h.bind(sid, conn.device_id, args[1].strip(), operator=True)
    except DeviceError as exc:
        return f"Not bound: {exc}"
    return f"Session {sid} now runs commands on {conn.name} [{b.device_id}] in {b.dir}."
