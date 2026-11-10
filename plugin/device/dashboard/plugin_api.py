"""Device WebSocket, mounted at /api/plugins/device/ws by the dashboard in the gateway process.

The dashboard's plugin gate checks Host, Origin and peer address and that the plugin is
enabled; it does not check credentials. This route authenticates the device by its name and
shared token from the first message.
"""
from __future__ import annotations

import asyncio
import importlib.util
import json
import logging
import sys
from pathlib import Path

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

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


@router.get("/devices")
def devices() -> dict:
    return {"devices": [{"name": c.name, "hostname": c.hostname, "roots": c.roots} for c in _core().hub().devices()]}


@router.websocket("/ws")
async def device_socket(ws: WebSocket) -> None:
    core = _core()
    await ws.accept()
    try:
        hello = json.loads(await asyncio.wait_for(ws.receive_text(), core.HELLO_TIMEOUT_S))
    except (asyncio.TimeoutError, WebSocketDisconnect, ValueError):
        await ws.close(code=1002)
        return
    name, token = str(hello.get("name") or ""), str(hello.get("token") or "")
    if hello.get("type") != "hello" or not core.authenticate(name, token):
        logger.warning("device: refused hello for %r", name)
        await ws.close(code=core.CLOSE_UNAUTHORIZED, reason="unknown device or bad token")
        return
    send_lock = asyncio.Lock()

    async def send_text(text: str) -> None:
        async with send_lock:
            await ws.send_text(text)

    conn = core.DeviceConn(
        name=name, send_text=send_text, loop=asyncio.get_running_loop(),
        roots=[str(r) for r in hello.get("roots") or []], tmp_dir=str(hello.get("tmp_dir") or ""),
        hostname=str(hello.get("hostname") or ""),
        close=lambda: ws.close(code=4409, reason="replaced by a newer connection"))
    hub = core.hub()
    hub.attach(conn)
    try:
        await send_text(json.dumps({"type": "welcome", "name": name}))
        while True:
            text = await ws.receive_text()
            try:
                msg = json.loads(text)
            except ValueError:
                continue
            hub.on_message(conn, msg)
    except WebSocketDisconnect:
        pass
    finally:
        hub.detach(conn)
