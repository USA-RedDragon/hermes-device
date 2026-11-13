"""Contract tests.

1. The Python wire constants equal the Go ones in protocol/protocol.go and assertion/assertion.go.
2. Every Hermes internal the plugin calls or wraps still exists with the shape the plugin relies
   on, read from $HERMES_AGENT_SRC (default ~/.hermes/hermes-agent). A Hermes upgrade that moves
   one of them fails here instead of silently letting device sessions read server files.
"""
from __future__ import annotations

import inspect
import os
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import core  # noqa: E402

REPO = Path(os.environ.get("TALARIA_REPO") or os.environ.get("HERMES_DEVICE_REPO") or Path(__file__).resolve().parents[3])
SRC = Path(os.path.expanduser(os.environ.get("HERMES_AGENT_SRC") or "~/.hermes/hermes-agent"))


def require_source() -> None:
    """Skip without a Hermes checkout. When HERMES_AGENT_SRC names one, fail instead, so a
    Hermes release that moves run_agent.py cannot pass by skipping."""
    if (SRC / "run_agent.py").is_file():
        return
    if os.environ.get("HERMES_AGENT_SRC"):
        raise AssertionError(f"HERMES_AGENT_SRC={SRC} has no run_agent.py")
    raise unittest.SkipTest(f"Hermes source not found at {SRC}; set HERMES_AGENT_SRC")

_GO_DURATION = re.compile(r"^(\d+)\s*\*\s*time\.Second$")


def go_consts(path: Path) -> dict[str, object]:
    """Top-level constants of a Go file: ints, strings and N * time.Second (as seconds)."""
    text = path.read_text(encoding="utf-8")
    out: dict[str, object] = {}
    blocks = re.findall(r"^const\s*\((.*?)^\)", text, flags=re.S | re.M)
    singles = re.findall(r"^const\s+(\w+\s*=.*)$", text, flags=re.M)
    for line in [ln for b in blocks for ln in b.splitlines()] + singles:
        line = line.split("//", 1)[0].strip()
        m = re.match(r"^(\w+)\s*=\s*(.+)$", line)
        if not m:
            continue
        name, value = m.group(1), m.group(2).strip()
        if value.startswith('"') and value.endswith('"'):
            out[name] = value[1:-1]
        elif re.fullmatch(r"-?\d+", value):
            out[name] = int(value)
        elif (d := _GO_DURATION.match(value)):
            out[name] = int(d.group(1))
    return out


def py_name(go_name: str) -> str:
    return re.sub(r"(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])", "_", go_name).upper().replace("A_C_P", "ACP")


PROTOCOL_NAMES = {"Version": "PROTOCOL_VERSION", "PingInterval": "PING_INTERVAL_S",
                  "PingTimeout": "PING_TIMEOUT_S", "HelloTimeout": "HELLO_TIMEOUT_S",
                  "ACPHostPath": "ACP_HOST_PATH", "MinVersion": "MIN_PROTOCOL_VERSION",
                  "DefaultGrace": "DEFAULT_GRACE_S", "BrowserDefaultTimeout": "BROWSER_DEFAULT_TIMEOUT_S",
                  "BrowserMaxTimeout": "BROWSER_MAX_TIMEOUT_S", "MCPDefaultTimeout": "MCP_DEFAULT_TIMEOUT_S",
                  "MCPMaxTimeout": "MCP_MAX_TIMEOUT_S"}
ASSERTION_NAMES = {"MaxLifetime": "MAX_LIFETIME_S", "ClockSkew": "CLOCK_SKEW_S", "LegACP": "LEG_ACP"}


class WireConstantTests(unittest.TestCase):
    def check(self, go_file: Path, module, names: dict[str, str]):
        if not go_file.is_file():
            self.skipTest(f"{go_file} not found; set TALARIA_REPO")
        consts = go_consts(go_file)
        self.assertGreater(len(consts), 5)
        for go_name, value in consts.items():
            name = names.get(go_name) or py_name(go_name)
            with self.subTest(go=go_name, py=name):
                self.assertTrue(hasattr(module, name), f"{module.__name__} has no {name} for Go {go_name}")
                self.assertEqual(getattr(module, name), value)

    def test_protocol_constants(self):
        self.check(REPO / "protocol" / "protocol.go", core, PROTOCOL_NAMES)

    def test_assertion_constants(self):
        self.check(REPO / "assertion" / "assertion.go", core.assertion, ASSERTION_NAMES)

    def test_python_only_values(self):
        self.assertEqual(core.CLOSE_UNSUPPORTED_PLATFORM, 4427)
        self.assertEqual(core.CLOSE_PROTOCOL_ERROR, 1002)
        self.assertEqual(core.SILENCE_TIMEOUT_S, 60)
        self.assertEqual(core.PLUGIN_EXEC_PATH, "/api/plugins/device/ws")

    def test_plugin_version_matches_manifest(self):
        manifest = (Path(core.__file__).parent / "plugin.yaml").read_text(encoding="utf-8")
        self.assertIn(f"version: {core.PLUGIN_VERSION}\n", manifest)


class SettingsDocContractTests(unittest.TestCase):
    """Every plugin setting the code reads is in the docs/protocol.md section 8.1 table, with
    the default the code uses."""

    def table(self) -> dict[str, str]:
        doc = REPO / "docs" / "protocol.md"
        if not doc.is_file():
            self.skipTest(f"{doc} not found; set TALARIA_REPO")
        text = doc.read_text(encoding="utf-8")
        section = text.split("### 8.1 Settings", 1)[1].split("### 8.2", 1)[0]
        return {m.group(1): m.group(2) for m in re.finditer(r"^\| `(\w+)` \| ([^|]+) \|", section, flags=re.M)}

    def test_every_setting_is_documented(self):
        code = Path(core.__file__).read_text(encoding="utf-8")
        keys = set(re.findall(r"""(?:settings\(\)|\bs)\.get\(\s*["'](\w+)["']""", code))
        self.assertGreaterEqual(keys, {"fallback", "fallback_device", "fallback_root", "backend_id"})
        table = self.table()
        for key in sorted(keys):
            with self.subTest(key=key):
                self.assertIn(key, table)

    def test_documented_defaults(self):
        table = self.table()
        self.assertEqual(table["fallback"].strip(), "`local`")
        self.assertEqual(table["fallback_root"].strip(), f"`{core.DEFAULT_FALLBACK_ROOT}`")


def original(fn):
    return getattr(fn, "__hermes_device_original__", fn)


def params(fn) -> list[str]:
    return list(inspect.signature(original(fn)).parameters)


class HermesContractTests(unittest.TestCase):
    """The Hermes internals the plugin depends on (docs/protocol.md section 8)."""

    @classmethod
    def setUpClass(cls):
        require_source()

    def source(self, rel: str) -> str:
        return (SRC / rel).read_text(encoding="utf-8")

    def assertSource(self, rel: str, *snippets: str):
        text = self.source(rel)
        for s in snippets:
            self.assertIn(s, text, f"{rel} no longer contains {s!r}")

    def test_runtime_imports_this_source(self):
        import run_agent
        self.assertEqual(Path(run_agent.__file__).resolve().parent, SRC.resolve())

    def test_plugin_context_registrars(self):
        from hermes_cli.plugins import PluginContext
        for name in ("register_terminal_environment_provider", "register_hook", "register_command",
                     "register_context_reference"):
            self.assertTrue(callable(getattr(PluginContext, name, None)), name)
        self.assertEqual(params(PluginContext.register_command)[:5],
                         ["self", "name", "handler", "description", "args_hint"])
        self.assertEqual(params(PluginContext.register_hook)[:3], ["self", "hook_name", "callback"])
        self.assertSource("hermes_cli/plugin_command_dispatch.py", "session_id")

    def test_background_handoff_contract(self):
        """Section 8.4: the plugin hands a device command to Hermes's process registry."""
        from tools import terminal_tool, terminal_tool_background
        from tools.process_registry import ProcessRegistry
        self.assertTrue(callable(getattr(terminal_tool, "yield_to_background_handler", None)))
        self.assertTrue(callable(getattr(terminal_tool_background, "_stamp_routing_if_gateway", None)))
        self.assertEqual(params(terminal_tool_background._stamp_routing_if_gateway),
                         ["process_registry", "session", "session_key"])
        handler = params(terminal_tool.yield_to_background_handler)
        for name in ("command", "env_type", "cwd", "effective_task_id", "task_id", "session_key"):
            self.assertIn(name, handler)
        adopt = params(ProcessRegistry.adopt_local)
        for name in ("proc", "command", "cwd", "task_id", "session_key", "owner_task_id", "output_so_far"):
            self.assertIn(name, adopt)
        self.assertEqual(params(ProcessRegistry._signal_kill)[:4],
                         ["self", "session", "session_id", "consume_output"])
        self.assertSource("tools/terminal_tool.py", "**_yield_kwargs(command", "yield_to_background_handler(")
        self.assertSource("tools/process_registry.py", "elif session.process:",
                          "self._terminate_host_pid(session.process.pid", "_take_handoff_group(session, proc)",
                          "for stream in (proc.stdout, proc.stderr, proc.stdin):")

    def test_terminal_provider_contract(self):
        from agent.terminal_env_provider import TerminalEnvironmentProvider
        self.assertTrue(TerminalEnvironmentProvider.is_container)
        self.assertEqual(TerminalEnvironmentProvider.__abstractmethods__, frozenset(
            {"name", "is_available", "create_environment"}))
        p = inspect.signature(TerminalEnvironmentProvider.create_environment).parameters
        for name in ("cwd", "timeout", "task_id", "image", "container_config"):
            self.assertEqual(p[name].kind, inspect.Parameter.KEYWORD_ONLY, name)
        self.assertSource("tools/terminal_tool_backends.py",
                          "provider.create_environment(cwd=cwd, timeout=timeout, task_id=task_id, image=image,",
                          "env_obj._hermes_backend_name = provider.name.strip().lower()")
        sys.path.insert(0, str(Path(core.__file__).parent.parent))
        try:
            import device as plugin
        finally:
            sys.path.pop(0)
        provider = plugin.make_provider(core)
        self.assertEqual(provider.name, "device")
        self.assertFalse(provider.is_container)
        self.assertFalse(provider.skip_container_guards)
        self.assertTrue(provider.is_available())

    def test_file_tools_class_hints(self):
        from tools import file_tools_paths
        self.assertEqual(file_tools_paths._ENV_CLASS_NAME_HINTS, ("local", "ssh", "docker", "modal", "daytona"))
        name = core.DeviceRouterEnvironment.__name__.lower()
        self.assertFalse([h for h in file_tools_paths._ENV_CLASS_NAME_HINTS if h in name])
        self.assertEqual(params(file_tools_paths._terminal_env_type_for_task), ["task_id"])
        self.assertSource("tools/file_tools_paths.py", 'stamped = getattr(env, "_hermes_backend_name", None)')
        self.assertSource("acp_adapter/edit_approval.py", "return str(_resolve_path_for_task(path))")

    def test_container_cwd_heuristics_justify_is_container_false(self):
        self.assertSource("tools/terminal_tool_config.py", "return _is_host_cwd(cwd) or not os.path.isabs(cwd)")
        self.assertSource("tools/terminal_tool_config.py",
                          'return env_type in _CONTAINER_BACKENDS or _plugin_env_flag(env_type, "is_container")')

    def test_base_environment(self):
        from tools.environments.base import BaseEnvironment, env_uses_host_filesystem
        self.assertEqual(params(BaseEnvironment._run_bash), ["self", "cmd_string", "login", "timeout", "stdin_data"])
        p = params(BaseEnvironment.execute)
        self.assertEqual(p[:3], ["self", "command", "cwd"])
        self.assertIn("stdin_data", p)
        self.assertIn("timeout", p)
        for name in ("init_session", "get_temp_dir", "cleanup", "_wait_for_process", "_wrap_command"):
            self.assertTrue(callable(getattr(BaseEnvironment, name)), name)
        self.assertSource("tools/environments/base.py",
                          'return getattr(env, "uses_host_filesystem", False) is True',
                          "spawned = self._run_bash(wrapped, login=login, timeout=effective_timeout, "
                          "stdin_data=effective_stdin)",
                          "def init_session(self):")
        self.assertSource("agent/deadline.py", "contextvars.copy_context()")
        self.assertFalse(env_uses_host_filesystem(object()))
        from tools.environments.local import LocalEnvironment
        self.assertEqual(params(LocalEnvironment.__init__)[:5], ["self", "cwd", "timeout", "env", "persistent"])
        self.assertIs(LocalEnvironment.uses_host_filesystem, True)
        self.assertSource("tools/terminal_tool_backends.py",
                          'persistent=(local_config or {}).get("persistent", True)',
                          'return {"persistent": config.get("local_persistent", True)}')

    def test_context_references(self):
        from agent import context_references as cr
        self.assertEqual(cr.BUILTIN_PREFIXES, frozenset({"diff", "staged", "file", "folder", "git", "url"}))
        self.assertEqual(cr.ContextReferenceProvider.__abstractmethods__, frozenset({"autocomplete", "expand"}))
        self.assertTrue(inspect.iscoroutinefunction(original(cr._expand_reference)))
        self.assertEqual(params(cr._expand_reference),
                         ["ref", "cwd", "url_fetcher", "allowed_root", "max_inline_tokens"])
        self.assertSource("agent/context_references.py",
                          "_expand_reference(ref, cwd_path, url_fetcher=url_fetcher, allowed_root=allowed_root_path,",
                          "if ref.kind in (\"file\", \"folder\"):",
                          "if ref.kind in _GIT_REFERENCE_ARGS:")
        self.assertEqual(set(cr._GIT_REFERENCE_ARGS), {"diff", "staged", "git"})
        fields = set(cr.ContextReference.__dataclass_fields__)
        self.assertTrue({"raw", "kind", "target", "line_start", "line_end"} <= fields)
        self.assertTrue(cr._SENSITIVE_HOME_FILES)
        self.assertTrue(callable(cr.register_context_reference_provider))

    def test_session_context_refs(self):
        from gateway import session_context_refs as scr
        self.assertEqual(params(scr.inbound_references), ["runner", "source", "session_key", "text"])
        self.assertEqual(params(scr.managed_turn_text), ["authority", "row", "policy"])
        self.assertSource("gateway/session_context_refs.py",
                          "expanded, refusal = await inbound_references(runner, source, session_key, target)")
        self.assertSource("gateway/run_inbound.py", "from gateway.session_context_refs import inbound_references")
        self.assertSource("gateway/session_managed_worker.py",
                          "from gateway.session_context_refs import managed_turn_text",
                          "text, refusal = await managed_turn_text(authority, row, policy)",
                          "if policy.ignore_user_config or policy.kanban_json is not None:")
        self.assertSource("gateway/session_policy.py", "policy = adapter.policies.get(source.chat_id)")
        self.assertSource("hermes_state_runtime.py", "target_session_id")
        self.assertSource("gateway/session.py", "def peek_session_id(self, session_key: str)")
        self.assertSource("tui_gateway/prompt_turn.py",
                          'scopes.session_tokens = _set_session_context(session["session_key"], ui_session_id=sid)',
                          "ctx = preprocess_context_references(")

    def test_edit_approval(self):
        from acp_adapter import edit_approval as ea
        self.assertEqual(params(ea._read_text_if_exists), ["path"])
        self.assertEqual(params(ea._required_path), ["arguments"])
        self.assertSource("acp_adapter/edit_approval.py",
                          "path = _required_path(arguments)",
                          "_read_text_if_exists(path), str(content)",
                          "paths = [_required_path({'path': path}) for path in _extract_v4a_patch_paths(patch_body)]",
                          'return _denied(f"Edit approval denied: could not prepare diff ({exc})")')
        self.assertSource("model_tools.py", "if modified_args is not None:",
                          "from acp_adapter.edit_approval import maybe_require_edit_approval")
        text = self.source("model_tools.py")
        self.assertLess(text.index("_dispatch_pre_tool_call_hooks("), text.index("maybe_require_edit_approval("))

    def test_checkpoints(self):
        from tools.checkpoint_manager import CheckpointManager
        self.assertEqual(params(CheckpointManager.ensure_checkpoint), ["self", "working_dir", "reason"])
        self.assertSource("agent/tool_executor.py", "agent._checkpoint_mgr.ensure_checkpoint(")

    def test_lineage_sources(self):
        from agent import agent_init
        self.assertEqual(params(agent_init._init_session_state)[:4],
                         ["agent", "session_id", "session_db", "parent_session_id"])
        self.assertSource("agent/agent_init.py", "agent._parent_session_id = parent_session_id",
                          "    _init_session_state(\n")
        self.assertSource("run_agent.py", "parent_session_id=self._parent_session_id")
        self.assertSource("tools/delegate_tool.py", "parent_session_id=parent_sid")
        from hermes_cli.goals import _get_session_db
        self.assertTrue(callable(_get_session_db))
        from hermes_state import SessionDB
        self.assertEqual(params(SessionDB.get_session), ["self", "session_id"])
        self.assertSource("hermes_state_sessions.py", "parent_session_id")
        from gateway import session_tui_side, run_background
        self.assertEqual(params(session_tui_side._spawn)[:3], ["authority", "ref", "task_id"])
        self.assertSource("gateway/session_tui_side.py",
                          "return _spawn(authority, ref, task_id, 'background.complete', body, holder=holder)",
                          "session_id=task_id, platform=policy.platform, session_db=authority.db,")
        self.assertEqual(params(run_background.GatewayBackgroundMixin._run_background_task)[:4],
                         ["self", "prompt", "source", "task_id"])
        self.assertSource("gateway/run_background.py", "session_id=task_id,")
        self.assertSource("gateway/slash_commands.py", "self._run_background_task(")
        from gateway.session_authority import SessionAuthority
        self.assertEqual(params(SessionAuthority.physical_target), ["self", "ref"])

    def test_session_variables(self):
        from contextvars import ContextVar
        from gateway import session_context
        self.assertIsInstance(session_context._SESSION_ID, ContextVar)
        self.assertEqual(session_context._SESSION_ID.name, "HERMES_SESSION_ID")
        self.assertIs(session_context._SESSION_ID.get(session_context._UNSET), session_context._UNSET)
        from agent import runtime_cwd
        self.assertTrue(callable(runtime_cwd.scoped_session_cwd))
        self.assertTrue(callable(runtime_cwd.scope_terminal_cwd))
        self.assertSource("gateway/session_context.py", '_runtime_cwd("set_session_cwd", cwd)')
        from tools import terminal_tool
        self.assertEqual(params(terminal_tool.record_session_cwd), ["session_key", "cwd"])
        self.assertEqual(params(terminal_tool.get_session_cwd), ["session_key"])
        self.assertTrue(callable(terminal_tool._get_env_config))
        from tools.approval import get_current_session_key
        self.assertEqual(params(get_current_session_key), ["default"])

    def test_pre_tool_call_contract(self):
        self.assertSource("hermes_cli/plugins.py",
                          'task_id=task_id, session_id=session_id, tool_call_id=tool_call_id, turn_id=turn_id,',
                          'if action == "modify":',
                          'if action == "block":')
        self.assertSource("model_tools.py", 'logger.debug("pre_tool_call hook error: %s", _hook_err)')

    def test_write_denylist(self):
        from agent import file_safety
        for fn in (file_safety.build_write_denied_paths, file_safety.build_write_denied_prefixes,
                   file_safety.build_write_approval_paths):
            self.assertEqual(params(fn), ["home"])
        denied, prefixes, approval = core.device_denylist("/home/alice")
        self.assertIn("/home/alice/.ssh/id_rsa", denied)
        self.assertIn("/etc/shadow", denied)
        self.assertIn("/home/alice/.aws/", prefixes)
        self.assertEqual(approval, {"/home/alice/.ssh/config"})
        self.assertFalse([p for p in denied if not (p.startswith("/home/alice/") or p.startswith("/etc/"))])
        self.assertSource("agent/file_safety.py", 'os.getenv("HERMES_WRITE_SAFE_ROOT", "").split(os.pathsep)')
        self.assertTrue(file_safety.HOME_CREDENTIAL_DIRS)

    def test_tool_names(self):
        for rel, name in (("tools/terminal_tool.py", "terminal"), ("tools/process_registry.py", "process_manage"),
                          ("tools/read_terminal_tool.py", "read_terminal"),
                          ("tools/close_terminal_tool.py", "close_terminal"),
                          ("tools/file_tools.py", "read_file"), ("tools/file_tools.py", "write_file"),
                          ("tools/file_tools.py", "patch"), ("tools/file_tools.py", "search_files"),
                          ("tools/cronjob_tools.py", "cronjob_manage")):
            self.assertRegex(self.source(rel), rf'name\s*=\s*"{name}"', f"{rel}: tool {name}")
        for name in ("kanban_create", "kanban_schedule", "kanban_unblock"):
            self.assertSource("tools/kanban_tools.py", f'@_kanban_handler("{name}")')
        self.assertSource("tools/file_tools.py", '"path": {"type": "string", "description": "Directory or file to search in')
        from tools.patch_parser import parse_v4a_patch
        ops, err = parse_v4a_patch("*** Begin Patch\n*** Move File: a -> b\n*** End Patch")
        self.assertIsNone(err)
        self.assertEqual((ops[0].file_path, ops[0].new_path), ("a", "b"))

    def test_helpers(self):
        from hermes_cli._subprocess_compat import harden_git_argv
        self.assertEqual(harden_git_argv(["diff"]), ["diff", "--no-ext-diff", "--no-textconv"])
        from agent.model_metadata import CHARS_PER_TOKEN, estimate_tokens_rough
        self.assertGreater(CHARS_PER_TOKEN, 0)
        self.assertGreater(estimate_tokens_rough("hello world"), 0)
        from hermes_cli.version_info import get_version_info
        self.assertTrue(get_version_info().base_version)
        from hermes_cli.config import load_config_readonly
        self.assertTrue(callable(load_config_readonly))
        from gateway import run_api
        self.assertTrue(hasattr(run_api, "_ACTIVE_RUNNER"))
        from gateway.session_authorities import all_authorities
        self.assertTrue(callable(all_authorities))
        from gateway.session_policy import LocalSessionPolicy, policy_for_source
        fields = set(LocalSessionPolicy.__dataclass_fields__)
        self.assertTrue({"cwd", "terminal_json", "safe_mode", "ignore_user_config"} <= fields)
        self.assertEqual(params(policy_for_source), ["runner", "source"])
        self.assertSource("gateway/session_policy.py", "terminal.get('TERMINAL_ENV')")

    def test_dashboard_mount(self):
        self.assertSource("hermes_cli/web_server_dashboard.py",
                          'prefix=f"/api/plugins/{plugin[\'name\']}"',
                          "class PluginApiWebSocketGate:")


if __name__ == "__main__":
    unittest.main()
