"""Hermes device plugin: a terminal backend that runs commands on a device that dials in.

Registers the ``device`` terminal environment provider and the ``/device`` command. The
WebSocket the device dials lives in ``dashboard/plugin_api.py``; both halves share
``core.py`` through one module object.
"""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_CORE = "hermes_device_core"


def _core():
    module = sys.modules.get(_CORE)
    if module is None:
        spec = importlib.util.spec_from_file_location(_CORE, Path(__file__).with_name("core.py"))
        module = importlib.util.module_from_spec(spec)
        sys.modules[_CORE] = module
        spec.loader.exec_module(module)
    return module


USAGE = ("Usage: /device <name> <dir> binds this session to a device directory; "
         "/device shows the binding; /device off removes it; /device list shows connected devices.")


def device_command(raw_args: str = "", session_id: str = "", **_kwargs) -> str:
    core = _core()
    hub = core.hub()
    args = (raw_args or "").split(maxsplit=1)
    if not args:
        b = hub.binding(session_id)
        if b is None:
            return f"No device bound to session {session_id or '(none)'}. {USAGE}"
        state = "connected" if hub.device(b.device) else "offline"
        return f"Session {session_id} runs on {b.device} ({state}) in {b.dir}"
    if args[0] == "list" and len(args) == 1:
        conns = hub.devices()
        if not conns:
            return "No devices connected."
        return "\n".join(f"{c.name} ({c.hostname}) roots: {', '.join(c.roots)}" for c in conns)
    if args[0] == "off" and len(args) == 1:
        return "Device binding removed." if hub.unbind(session_id) else "No device was bound."
    if len(args) != 2:
        return USAGE
    try:
        b = hub.bind(session_id, args[0], args[1].strip())
    except core.DeviceError as exc:
        return f"Not bound: {exc}"
    return f"Session {session_id} now runs commands on {b.device} in {b.dir}"


def register(ctx) -> None:
    core = _core()
    from agent.terminal_env_provider import TerminalEnvironmentProvider

    class DeviceTerminalProvider(TerminalEnvironmentProvider):
        name = "device"
        display_name = "Device"
        is_remote = True
        # Device paths look like host paths (/home/...); a container backend would discard them.
        is_container = False

        @property
        def skip_container_guards(self) -> bool:
            return False

        @property
        def env_description(self) -> str:
            return "the user's own device, reached through the device plugin (Linux or macOS shell)"

        def is_available(self) -> bool:
            return bool(core.configured_devices())

        def create_environment(self, *, cwd: str, timeout: int, task_id: str = "default", **_kwargs):
            return core.DeviceEnvironment(cwd=cwd, timeout=timeout, task_id=task_id)

    ctx.register_terminal_environment_provider(DeviceTerminalProvider())
    def pre_tool_call(*_args, task_id: str = "", session_id: str = "", **_kwargs):
        try:
            core.anchor_file_tools(task_id, session_id)
        except Exception:
            core.logger.debug("device: could not anchor file tools", exc_info=True)
        return None

    ctx.register_hook("pre_tool_call", pre_tool_call)
    ctx.register_command("device", device_command, description="Bind this session to a device directory",
                         args_hint="<name> <dir> | off | list")
