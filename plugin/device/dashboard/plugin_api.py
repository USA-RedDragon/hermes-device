"""Device plugin routes, mounted at /api/plugins/device/ by the dashboard in the gateway process
(docs/protocol.md sections 4 and 9).

The HTTP routes sit behind dashboard sign-in. The dashboard's WebSocket gate checks Host, Origin,
peer and that the plugin is enabled, but no credential, so the exec route verifies the hub
assertion itself before it reads any message.
"""
from __future__ import annotations

import asyncio
import base64
import importlib.util
import json
import logging
import posixpath
import sys
from pathlib import Path
from typing import Any, Optional
from urllib.parse import quote

from fastapi import APIRouter, Request, WebSocket
from fastapi.responses import JSONResponse, Response

router = APIRouter()
logger = logging.getLogger("hermes_device")
_CORE = "hermes_device_core"


def _core():
    module = sys.modules.get(_CORE)
    if module is None:
        spec = importlib.util.spec_from_file_location(_CORE, Path(__file__).parent.parent / "core.py")
        module = importlib.util.module_from_spec(spec)
        sys.modules[_CORE] = module
        spec.loader.exec_module(module)
    return module


def _error(status: int, code: str, message: str, **extra: Any) -> JSONResponse:
    return JSONResponse({"error": code, "message": message, **extra}, status_code=status)


# --- HTTP routes (section 9) ------------------------------------------------------------------

@router.get("/info")
def info() -> dict:
    core = _core()
    return {"plugin_version": core.PLUGIN_VERSION, "hermes_version": core.hermes_version(),
            "protocol_version": core.PROTOCOL_VERSION, "backend_id": core.backend_id(),
            "placeholder": core.placeholder()}


@router.get("/devices")
def devices() -> dict:
    core = _core()
    conns = sorted(core.hub().devices(), key=lambda c: (c.name, c.device_id))
    return {"devices": [c.info() for c in conns]}


@router.get("/devices/{device_id}/dirs")
def device_dirs(device_id: str, path: str = ""):
    core = _core()
    try:
        resolved, names = core.list_device_dirs(device_id, path)
    except core.BindError as exc:
        return _error(exc.status, exc.code, exc.message)
    return {"path": resolved, "entries": [{"name": n} for n in names]}


@router.get("/bindings")
def bindings() -> dict:
    core = _core()
    return {"bindings": [b.to_json() for b in core.hub().bindings()]}


@router.get("/sessions/{session_id}/binding")
def get_binding(session_id: str):
    core = _core()
    res = core.hub().resolve(session_id)
    if res.state == core.STATE_BOUND:
        return {**res.binding.to_json(), "resolved": res.to_json()}
    resolved = res.to_json()
    if res.state == core.STATE_NEVER_BOUND and core.fallback() == "device":
        sandbox = core.sandbox_resolution(res)
        resolved["sandbox"] = ({"device_id": sandbox.binding.device_id, "dir": sandbox.binding.dir}
                               if sandbox.state == core.STATE_BOUND else None)
    return _error(404, "not_bound", f"session {session_id} is not bound to a device", resolved=resolved)


@router.put("/sessions/{session_id}/binding")
async def put_binding(session_id: str, request: Request):
    core = _core()
    try:
        body = await request.json()
    except Exception:
        body = None
    if not isinstance(body, dict) or not isinstance(body.get("device_id"), str) or not body.get("device_id") \
            or not isinstance(body.get("dir"), str):
        return _error(400, "bad_request", 'the body must be {"device_id": <string>, "dir": <string>}')
    try:
        binding = await asyncio.to_thread(core.hub().bind, session_id, body["device_id"], body["dir"])
    except core.BindError as exc:
        return _error(exc.status, exc.code, exc.message)
    return binding.to_json()


@router.delete("/sessions/{session_id}/binding")
def delete_binding(session_id: str):
    core = _core()
    try:
        removed = core.hub().unbind(session_id)
    except core.StateUnavailable as exc:
        return _error(503, "state_unavailable", str(exc))
    if not removed:
        return _error(404, "not_bound", f"session {session_id} is not bound to a device")
    return Response(status_code=204)


def _agents():
    return _core()._load_sibling("hermes_device_agents", "agents.py")


@router.get("/agents/requests")
def agent_requests(wait: float = 0.0) -> dict:
    """The hub's long poll for agent message requests (section 12)."""
    agents = _agents()
    agents.INBOX.sweep()
    return {"requests": agents.BUS.take(wait)}


@router.post("/agents/requests/{request_id}")
async def agent_answer(request_id: str, request: Request):
    try:
        body = await request.json()
    except Exception:
        body = None
    if not isinstance(body, dict) or ("result" in body) == ("error" in body):
        return _error(400, "bad_request", 'the body must be {"result": ...} or {"error": {"code", "message"}}')
    if not _agents().BUS.answer(request_id, body):
        return _error(404, "unknown_request", "no tool call waits for this request")
    return Response(status_code=204)


@router.post("/agents/deliver")
async def agent_deliver(request: Request):
    try:
        body = await request.json()
    except Exception:
        body = None
    if not isinstance(body, dict):
        return _error(400, "bad_request", "the body must be a JSON object")
    try:
        return await asyncio.to_thread(_agents().deliver, body)
    except ValueError as exc:
        return _error(400, "bad_request", str(exc))


@router.post("/sessions/{session_id}/files")
async def post_file(session_id: str, request: Request):
    core = _core()
    if not core.UPLOADS.acquire(blocking=False):
        return _error(429, "busy", f"{core.MAX_UPLOADS} uploads are running; try again in a moment")
    try:
        return await _post_file(core, session_id, request)
    finally:
        core.UPLOADS.release()


async def _post_file(core, session_id: str, request: Request):
    limit = 4 * ((core.MAX_UPLOAD_BYTES + 2) // 3) + 64 * 1024
    too_large = f"the file is larger than {core.MAX_UPLOAD_BYTES // (1024 * 1024)} MB"
    length = request.headers.get("content-length", "")
    if length.isdigit() and int(length) > limit:
        return _error(413, "too_large", too_large)
    raw = bytearray()
    async for chunk in request.stream():
        raw += chunk
        if len(raw) > limit:
            return _error(413, "too_large", too_large)
    try:
        body = json.loads(bytes(raw))
    except ValueError:
        body = None
    del raw
    if not isinstance(body, dict) or not isinstance(body.get("name"), str) \
            or not isinstance(body.get("content_base64"), str):
        return _error(400, "bad_request", 'the body must be {"name": <string>, "content_base64": <string>}')
    try:
        data = base64.b64decode(body.pop("content_base64"), validate=True)
    except ValueError:
        return _error(400, "bad_request", "content_base64 is not valid base64")
    try:
        return await asyncio.to_thread(core.put_session_file, session_id, body["name"], data)
    except core.BindError as exc:
        return _error(exc.status, exc.code, exc.message)


class _ReleasingResponse(Response):
    """A response that frees a read slot once it is sent or fails, so the file's bytes count
    against the slot until they leave."""

    def __init__(self, *args: Any, release, **kwargs: Any):
        super().__init__(*args, **kwargs)
        self._release = release

    async def __call__(self, scope, receive, send) -> None:
        try:
            await super().__call__(scope, receive, send)
        finally:
            self._release()


@router.get("/sessions/{session_id}/files")
async def get_file(session_id: str, path: str = ""):
    core = _core()
    if not core.FILE_READS.acquire(blocking=False):
        return _error(429, "busy", f"{core.MAX_FILE_READS} file reads are running; try again in a moment")
    slot = core.FILE_READS
    fut = asyncio.get_running_loop().run_in_executor(None, core.get_session_file, session_id, path)
    # Shielded, so a dropped request lets the read finish in its thread and then frees the slot.
    try:
        name, data = await asyncio.shield(fut)
    except asyncio.CancelledError:
        fut.add_done_callback(lambda _f: slot.release())
        raise
    except BaseException as exc:
        slot.release()
        if isinstance(exc, core.BindError):
            return _error(exc.status, exc.code, exc.message)
        raise
    return _ReleasingResponse(content=data, media_type="application/octet-stream", release=slot.release, headers={
        "Content-Disposition": "attachment; filename*=UTF-8''" + quote(name, safe=""),
        "Content-Security-Policy": "default-src 'none'; sandbox",
        "X-Content-Type-Options": "nosniff",
        "Cache-Control": "no-store",
    })



async def _browse(fn, *args):
    core = _core()
    if not core.BROWSES.acquire(blocking=False):
        return _error(429, "busy", f"{core.MAX_BROWSES} folder reads are running; try again in a moment")
    slot = core.BROWSES
    fut = asyncio.get_running_loop().run_in_executor(None, fn, *args)
    try:
        out = await asyncio.shield(fut)
    except asyncio.CancelledError:
        fut.add_done_callback(lambda _f: slot.release())
        raise
    except BaseException as exc:
        slot.release()
        if isinstance(exc, core.BindError):
            return _error(exc.status, exc.code, exc.message)
        raise
    slot.release()
    return JSONResponse(out, headers={"Cache-Control": "no-store"})


@router.get("/sessions/{session_id}/tree")
async def get_tree(session_id: str, path: str = ""):
    return await _browse(_core().list_session_dir, session_id, path)


@router.get("/sessions/{session_id}/diff")
async def get_diff(session_id: str):
    return await _browse(_core().session_diff, session_id)

# --- built-in memory (section 9.1) -------------------------------------------------------------

def _memory():
    return _core()._load_sibling("hermes_device_memory", "memory.py")


async def _memory_call(fn, *args):
    mem = _memory()
    try:
        return await asyncio.to_thread(fn, *args)
    except mem.MemoryRefused as exc:
        return _error(exc.status, exc.code, exc.message)


async def _json_body(request: Request) -> Optional[dict]:
    try:
        body = await request.json()
    except Exception:
        return None
    return body if isinstance(body, dict) else None


@router.get("/memory")
async def memory_get():
    return await _memory_call(_memory().snapshot)


@router.post("/memory/pending/{pending_id}/approve")
async def memory_approve(pending_id: str):
    return await _memory_call(_memory().approve, pending_id)


@router.post("/memory/pending/{pending_id}/reject")
async def memory_reject(pending_id: str):
    return await _memory_call(_memory().reject, pending_id)


@router.post("/memory/{target}/{op}")
async def memory_change(target: str, op: str, request: Request):
    mem = _memory()
    body = await _json_body(request)
    try:
        if op == "add":
            args = (mem.add, target, mem.body_text(body, "content"))
        elif op == "edit":
            args = (mem.edit, target, mem.body_text(body, "entry"), mem.body_text(body, "content"))
        elif op == "remove":
            args = (mem.remove, target, mem.body_text(body, "entry"))
        elif op == "replace":
            args = (mem.replace_all, target, mem.body_text(body, "text"), mem.body_list(body, "base"))
        else:
            return _error(404, "not_found", "unknown memory operation")
    except mem.MemoryRefused as exc:
        return _error(exc.status, exc.code, exc.message)
    return await _memory_call(*args)



# --- session store views (section 9.2) ---------------------------------------------------------

def _insights():
    return _core()._load_sibling("hermes_device_insights", "insights.py")


async def _insights_call(fn, *args):
    mod = _insights()
    if not mod.READS.acquire(blocking=False):
        return _error(429, "busy", f"{mod.MAX_READS} searches are running; try again in a moment")
    slot = mod.READS
    fut = asyncio.get_running_loop().run_in_executor(None, fn, *args)
    try:
        out = await asyncio.shield(fut)
    except asyncio.CancelledError:
        fut.add_done_callback(lambda _f: slot.release())
        raise
    except BaseException as exc:
        slot.release()
        if isinstance(exc, mod.Refused):
            return _error(exc.status, exc.code, exc.message)
        raise
    slot.release()
    return JSONResponse(out, headers={"Cache-Control": "no-store"})


@router.get("/search")
async def search(q: str = "", limit: int = 20):
    return await _insights_call(_insights().search, q, limit)


@router.get("/usage")
async def usage(days: int = 30):
    return await _insights_call(_insights().usage, days)


@router.get("/sessions/{session_id}/changes")
async def changes(session_id: str):
    return await _insights_call(_insights().changes, session_id)

# --- exec leg (section 4) ---------------------------------------------------------------------

def _header(ws: WebSocket, name: str) -> Optional[bytes]:
    want = name.lower().encode("latin-1")
    for key, value in ws.scope.get("headers") or []:
        if key.lower() == want:
            return value
    return None


def _request_path(ws: WebSocket) -> str:
    raw = ws.scope.get("raw_path")
    if isinstance(raw, (bytes, bytearray)) and raw:
        return bytes(raw).split(b"?", 1)[0].decode("latin-1")
    return ws.scope.get("path") or ws.url.path


class _Gone(Exception):
    """The peer or the server closed the socket; ``code`` is the close code."""

    def __init__(self, code: Any):
        super().__init__(code)
        self.code = code


async def _receive_text(ws: WebSocket, timeout: float) -> str:
    """The next text message, or "" for a binary one. Raises _Gone when the socket closed."""
    msg = await asyncio.wait_for(ws.receive(), timeout)
    if msg.get("type") == "websocket.disconnect":
        raise _Gone(msg.get("code"))
    text = msg.get("text")
    return text if isinstance(text, str) else ""


def _version_range(hello: dict) -> Optional[tuple[int, int]]:
    """The device's ``(oldest, newest)`` protocol versions from hello (section 4.1)."""
    low, high = hello.get("version"), hello.get("max_version", hello.get("version"))
    if any(isinstance(v, bool) or not isinstance(v, int) for v in (low, high)) or high < low:
        return None
    return low, high


def negotiate(hello: dict, dev_mode: bool) -> tuple[Optional[int], str]:
    """The newest version both sides speak, or None and the close reason."""
    core = _core()
    lowest = 1 if dev_mode else core.MIN_PROTOCOL_VERSION
    span = _version_range(hello)
    if span is not None:
        chosen = min(span[1], core.PROTOCOL_VERSION)
        if chosen >= max(span[0], lowest):
            return chosen, ""
        theirs = f"{span[0]}" if span[0] == span[1] else f"{span[0]} to {span[1]}"
    else:
        theirs = repr(hello.get("version"))
    client = hello.get("client_version") if isinstance(hello.get("client_version"), str) else "?"
    return None, (f"hermes-device {client[:16]} speaks protocol {theirs[:12]}; plugin {core.PLUGIN_VERSION} speaks "
                  f"{lowest} to {core.PROTOCOL_VERSION}; upgrade them together")


async def _close(ws: WebSocket, code: int, reason: str) -> None:
    try:
        await ws.close(code=code, reason=reason)
    except Exception:
        logger.debug("device: close %d failed", code, exc_info=True)


@router.websocket("/ws")
async def device_socket(ws: WebSocket) -> None:
    core = _core()
    header = _header(ws, core.assertion.HEADER)
    path = _request_path(ws)
    await ws.accept()
    claims = None
    if header is not None or not core.dev_static_tokens():
        if header is None:
            logger.warning("device: exec upgrade without an assertion refused")
            await _close(ws, core.CLOSE_ASSERTION_REFUSED, "assertion missing")
            return
        want = core.assertion.Expect(audience=core.backend_id(), leg=core.assertion.LEG_EXEC, method="GET",
                                     path=path)
        try:
            claims = core.verifier().verify(header, want)
        except core.assertion.Refused as exc:
            logger.warning("device: assertion refused on %s: %s", path, exc)
            await _close(ws, core.CLOSE_ASSERTION_REFUSED, "assertion refused")
            return
    try:
        text = await _receive_text(ws, core.HELLO_TIMEOUT_S)
        hello = json.loads(text) if text else None
    except _Gone:
        return
    except (asyncio.TimeoutError, ValueError):
        hello = None
    if not isinstance(hello, dict) or hello.get("type") != core.TYPE_HELLO:
        await _close(ws, core.CLOSE_PROTOCOL_ERROR, "expected hello")
        return
    version, why = negotiate(hello, claims is None)
    if version is None:
        logger.warning("device: refused hello: %s", why)
        await _close(ws, core.CLOSE_UNSUPPORTED_VERSION, why)
        return
    if str(hello.get("os") or "").lower() == "windows":
        await _close(ws, core.CLOSE_UNSUPPORTED_PLATFORM, "windows devices are not supported")
        return
    if claims is None:
        name, token = str(hello.get("name") or ""), str(hello.get("token") or "")
        if not core.DEVICE_ID_RE.fullmatch(name) or not core.authenticate_static(name, token):
            logger.warning("device: refused dev hello for %r", name)
            await _close(ws, core.CLOSE_UNAUTHORIZED, "unknown device or bad token")
            return
        device_id = display = name
        user = ""
    else:
        device_id, display, user = claims.device_id, claims.device_name, claims.sub

    def text_field(key: str) -> str:
        value = hello.get(key)
        return value if isinstance(value, str) else ""

    roots = [posixpath.normpath(r) for r in (hello.get("roots") or [])
             if isinstance(r, str) and posixpath.isabs(r)] if isinstance(hello.get("roots"), list) else []
    send_lock = asyncio.Lock()

    async def send_text(data: str) -> None:
        async with send_lock:
            await ws.send_text(data)

    offered = hello.get("capabilities")
    capabilities = frozenset(c for c in offered if isinstance(c, str) and c in core.CAPABILITIES) \
        if isinstance(offered, list) else frozenset()
    conn = core.DeviceConn(
        device_id=device_id, name=display, send_text=send_text, loop=asyncio.get_running_loop(), roots=roots,
        tmp_dir=text_field("tmp_dir"), home=text_field("home"), hostname=text_field("hostname"),
        os=text_field("os"), arch=text_field("arch"), shell=text_field("shell"),
        client_version=text_field("client_version"), version=version, capabilities=capabilities, user=user,
        close=lambda code, reason: _close(ws, code, reason))
    if core.CAPABILITY_MCP in capabilities:
        names = hello.get("mcp_servers")
        servers = tuple(dict.fromkeys(n for n in names if isinstance(n, str) and core.MCP_SERVER_RE.fullmatch(n))) \
            if isinstance(names, list) else ()
        if servers:
            conn.mcp_servers = servers[:64]
        else:
            capabilities = capabilities - {core.CAPABILITY_MCP}
            conn.capabilities = capabilities
    hub = core.hub()
    welcome = {"type": core.TYPE_WELCOME, "version": version, "device_id": device_id, "name": display,
               "ping_interval": core.PING_INTERVAL_S, "plugin_version": core.PLUGIN_VERSION,
               "hermes_version": core.hermes_version()}
    if version >= core.RESUME_VERSION:
        welcome["grace_seconds"] = core.grace_seconds()
    if capabilities:
        welcome["capabilities"] = sorted(capabilities)
    image = core.hermes_image()
    if image:
        welcome["hermes_image"] = image
    pinger: Optional[asyncio.Task] = None
    resumable = True
    attached = False
    try:
        async with send_lock:
            # attach runs with the send lock held, so no exec reaches the device before the
            # welcome and the resume.
            resume = hub.attach(conn)
            attached = True
            await ws.send_text(json.dumps(welcome))
            if version >= core.RESUME_VERSION:
                await ws.send_text(json.dumps({"type": core.TYPE_RESUME, "commands": resume}))

        async def ping_loop() -> None:
            seq = 0
            while True:
                await asyncio.sleep(core.PING_INTERVAL_S)
                seq += 1
                await send_text(json.dumps({"type": core.TYPE_PING, "seq": seq}))

        pinger = asyncio.create_task(ping_loop())
        loop = asyncio.get_running_loop()
        last = loop.time()
        while True:
            remaining = core.SILENCE_TIMEOUT_S - (loop.time() - last)
            try:
                if remaining <= 0:
                    raise asyncio.TimeoutError
                text = await _receive_text(ws, remaining)
            except asyncio.TimeoutError:
                logger.warning("device: %s silent for %ss; closing", device_id, core.SILENCE_TIMEOUT_S)
                await _close(ws, core.CLOSE_PING_TIMEOUT, "no message for 60 seconds")
                break
            except _Gone as gone:
                if gone.code == core.CLOSE_SERVICE_RESTART:
                    # uvicorn ends every socket with 1012 when the gateway stops, and the device
                    # kills its commands on 1012, so nothing would resume them.
                    resumable = False
                break
            last = loop.time()
            try:
                msg = json.loads(text) if text else None
            except ValueError:
                continue
            if not isinstance(msg, dict):
                continue
            kind = msg.get("type")
            if kind == core.TYPE_PING:
                await send_text(json.dumps({"type": core.TYPE_PONG, "seq": msg.get("seq")}))
            elif kind in (core.TYPE_OUTPUT, core.TYPE_EXIT, core.TYPE_BROWSER_RESULT, core.TYPE_BROWSER_STATUS,
                          core.TYPE_MCP_RESULT):
                hub.on_message(conn, msg)
    except asyncio.CancelledError:
        resumable = False
        raise
    except Exception:
        logger.debug("device: exec leg of %s ended", device_id, exc_info=True)
    finally:
        if pinger is not None:
            pinger.cancel()
        if attached:
            hub.detach(conn, resumable=resumable)
