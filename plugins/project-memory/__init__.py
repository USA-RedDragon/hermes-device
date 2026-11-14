"""project-memory: one curated memory per project for chats bound to a device folder.

Registers the ``project_memory`` tool (toolset ``project-memory``), the ``project-memory`` system prompt section
(the project's notes next to the global memory block, frozen with the prompt like that block), a
``subagent_start`` hook that marks delegate children, which may not write project memory while they may not
write memory, an ``agent_turn_start`` hook that resets the per-turn cap on failed writes, a wrapper on Hermes's memory
approval replay that applies staged project writes to their project, and one paragraph in the built-in memory tool's description that sends project facts here.
core.py has the details; dashboard/plugin_api.py has the routes for hermes-hub.
"""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_CORE = "hermes_project_memory_core"


def _core():
    module = sys.modules.get(_CORE)
    if module is None:
        spec = importlib.util.spec_from_file_location(_CORE, Path(__file__).with_name("core.py"))
        module = importlib.util.module_from_spec(spec)
        sys.modules[_CORE] = module
        spec.loader.exec_module(module)
    return module


def register(ctx) -> None:
    core = _core()
    ctx.register_tool(core.TOOL, core.TOOLSET, core.SCHEMA, core.handle, check_fn=core.offered,
                      description=core.SCHEMA["description"])
    ctx.register_system_prompt_section(core.SECTION_ID, core.section, max_chars=core.request_section_chars())
    ctx.register_hook("subagent_start", core.note_child)
    try:
        from hermes_cli.plugins import VALID_HOOKS
    except ImportError:
        VALID_HOOKS = set()
    if "agent_turn_start" in VALID_HOOKS:
        ctx.register_hook("agent_turn_start", core.on_turn_start)
    core.extend_memory_schema()
    core.install_pending_replay()
