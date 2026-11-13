"""Hermes device plugin: a terminal backend that runs a session's commands and file operations
on the user's own device, which dials in through the Talaria hub.

Registers the ``device`` terminal environment provider, the ``pre_tool_call`` guard, the
``/device`` command, the ``@device:`` context reference and the ``device-browser``,
``device-mcp`` and ``device-agents`` toolsets, hides the built-in ``browser_vault`` tool, and wraps the Hermes internals
that read server files directly. The exec leg and the HTTP routes live in
``dashboard/plugin_api.py``; both halves share ``core.py`` through one module object.
docs/protocol.md section 8.
"""
from __future__ import annotations

import importlib.util
import os
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


def device_command(raw_args: str = "", session_id: str = "", **kwargs) -> str:
    return _core().device_command(raw_args, session_id=session_id, **kwargs)


def make_provider(core):
    from agent.terminal_env_provider import TerminalEnvironmentProvider

    class DeviceTerminalProvider(TerminalEnvironmentProvider):
        name = "device"
        display_name = "Device"
        is_remote = True
        # Container path handling would discard device paths such as /home/alice/src as host paths.
        is_container = False

        @property
        def skip_container_guards(self) -> bool:
            return False

        @property
        def env_description(self) -> str:
            return ("the user's own device (Linux or macOS bash, or on Windows Git Bash, PowerShell or WSL), "
                    "reached through the Talaria hub")

        def is_available(self) -> bool:
            return True

        def create_environment(self, *, cwd: str, timeout: int, task_id: str = "default", **_kwargs):
            return core.DeviceRouterEnvironment(cwd=cwd, timeout=timeout, task_id=task_id)

    return DeviceTerminalProvider()


def make_reference_provider(core):
    import asyncio

    from agent.context_references import ContextCompletionItem, ContextReferenceProvider

    class DeviceReferenceProvider(ContextReferenceProvider):
        """``@device:<path>``: a file or folder of the session's device."""

        prefix = "device"
        description = "file or folder on this session's device"

        def _bound(self):
            res = core.resolve_current()
            if res.state != core.STATE_BOUND:
                raise ValueError("this session is not bound to a device; bind it with /device <device> <dir>"
                                 if res.state == core.STATE_NEVER_BOUND else core.fail_closed_message(res))
            return res

        async def expand(self, target: str):
            def run():
                warning, block = core.expand_device_path(self._bound(), f"@device:{target}", "any", target)
                if warning:
                    raise ValueError(warning)
                return block
            return await asyncio.to_thread(run)

        async def autocomplete(self, query: str, *, limit: int = 10):
            def run():
                try:
                    res = self._bound()
                    base = query.rsplit("/", 1)[0] + "/" if "/" in query else ""
                    directory = core.device_abs_path(res, base or ".")
                    _, names = core.list_device_dirs(res.binding.device_id, directory)
                except Exception:
                    return []
                stem = query.rsplit("/", 1)[-1]
                return [ContextCompletionItem(text=f"{base}{n}/", meta="device folder")
                        for n in names if n.startswith(stem)][:limit]
            return await asyncio.to_thread(run)

    return DeviceReferenceProvider()


# Built-in tools the plugin takes out of the tool list (docs/browser.md section 3.1).
HIDDEN_TOOLS = ("browser_vault",)


def _hidden_check() -> bool:
    return False


def _hidden_handler(args, **_kwargs) -> str:
    return '{"error": "this tool is turned off on this server"}'


def _hide_one(registry, name: str) -> bool:
    """Remove the built-in tool name for this plugin's profile; False if it is already gone.

    A plugin loaded for a profile cannot deregister a process-global tool (Hermes v0.23 says
    "register a scoped override instead"), so on that refusal the plugin registers an override
    in its own profile whose check_fn is never true: the tool is then left out of every tool
    list there. Both ways need the same opt-in (tools.override)."""
    entry = registry.get_entry(name)
    if entry is None or entry.check_fn is _hidden_check:
        return False
    try:
        registry.deregister(name)
        return True
    except PermissionError as exc:
        if "scoped override" not in str(exc):
            raise
    registry.register(name, entry.toolset, entry.schema, _hidden_handler, check_fn=_hidden_check,
                      description=entry.description, override=True)
    return True


def hide_builtin_tools(**_kwargs) -> list[str]:
    """Hide HIDDEN_TOOLS from Hermes's tool list and return the ones hidden.

    Hermes allows a plugin to remove a built-in tool only with the operator's opt-in
    (plugins.entries.device.granted_capabilities: [tools.override], or the older
    allow_tool_override: true, which Hermes still accepts as an alias). This runs from this
    module so that the registry sees the plugin as the caller and applies that check. Without
    the opt-in it logs once and keeps the tool. It runs at load and again at each session
    start, because the built-in tools can be imported after the plugin."""
    try:
        from tools.registry import registry
    except ImportError:
        return []
    removed = []
    for name in HIDDEN_TOOLS:
        try:
            hidden = _hide_one(registry, name)
        except PermissionError:
            if name not in _WARNED:
                _WARNED.add(name)
                _core().logger.warning(
                    "device: cannot hide the built-in tool %s: set plugins.entries.device.granted_capabilities "
                    "to include tools.override in config.yaml", name)
            continue
        if hidden:
            removed.append(name)
    return removed


_WARNED: set[str] = set()


def register(ctx) -> None:
    core = _core()
    hide_builtin_tools()
    ctx.register_hook("on_session_start", hide_builtin_tools)
    ctx.register_terminal_environment_provider(make_provider(core))
    ctx.register_hook("pre_tool_call", core.pre_tool_call)
    if "pre_llm_call" in core.valid_hooks():
        ctx.register_hook("pre_llm_call", core.pre_llm_call)
    if "filter_session_list" in core.valid_hooks():
        ctx.register_hook("filter_session_list", core.filter_session_list)
    ctx.register_command("device", device_command, description="Bind this session to a device directory",
                         args_hint="<device> <dir> | off | list")
    ctx.register_context_reference(make_reference_provider(core))
    core._load_sibling("hermes_device_browser", "browser.py").register(ctx, core)
    core._load_sibling("hermes_device_mcp", "mcptools.py").register(ctx, core)
    core._load_sibling("hermes_device_agents", "agents.py").register(ctx, core)
    core._load_sibling("hermes_device_handoff", "handoff.py").register(ctx, core)
    missing = core.install_hooks()
    if missing:
        core.logger.error("device: Hermes internals not wrapped: %s", ", ".join(missing))
    core.apply_safe_root_policy()
    ph = core.placeholder()
    if not ph:
        core.logger.error("device: the placeholder setting is required (plugins.entries.device.settings)")
    elif not os.path.isdir(ph):
        core.logger.error("device: placeholder %s does not exist", ph)
    if not core.backend_id():
        core.logger.error("device: the backend_id setting is required; the exec route refuses every assertion")
