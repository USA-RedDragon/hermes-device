"""Windows devices: the note that tells the agent the OS and shell, and the PowerShell wrapper. Run
inside the Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import base64
import re
import unittest
from unittest import mock

from test_core import load_core

core = load_core()


def conn(os_name: str, shell: str) -> "core.DeviceConn":
    return core.DeviceConn(device_id="d_win", name="laptop", send_text=lambda _: None, loop=mock.Mock(),
                           roots=["/c/Users/alice"], tmp_dir="/c/Users/alice/AppData/Local/Temp",
                           home="/c/Users/alice", hostname="LAPTOP-1", os=os_name, arch="amd64", shell=shell)


class PlatformTest(unittest.TestCase):
    def test_windows_needs_a_known_shell(self):
        self.assertTrue(core.windows_platform_ok("linux", "bash"))
        self.assertTrue(core.windows_platform_ok("darwin", "zsh"))
        for shell in core.WINDOWS_SHELLS:
            self.assertTrue(core.windows_platform_ok("windows", shell))
        for shell in ("", "bash", "cmd", None):
            self.assertFalse(core.windows_platform_ok("windows", shell))

    def test_info_names_the_shell(self):
        self.assertEqual(conn("windows", "wsl").info()["shell"], "wsl")


class NoteTest(unittest.TestCase):
    def test_only_windows_gets_a_note(self):
        self.assertEqual(core.device_system_note(conn("linux", "bash")), "")
        self.assertEqual(core.device_system_note(None), "")

    def test_each_shell(self):
        cases = {
            "powershell": ("PowerShell", "Get-ChildItem", "/c/Users/x"),
            "git-bash": ("Git Bash", "bash commands", "/c/Users/x"),
            "wsl": ("WSL", "Linux", "/mnt/c/Users/x"),
        }
        for shell, words in cases.items():
            note = core.device_system_note(conn("windows", shell))
            self.assertIn("runs Windows", note)
            self.assertIn("laptop (LAPTOP-1)", note)
            self.assertIn("C:\\Users\\x", note)
            for w in words:
                self.assertIn(w, note, shell)

    def test_pre_llm_call(self):
        binding = core.Binding(session_id="s1", device_id="d_win", dir="/c/Users/alice", generation="g1", bound_at=0.0)
        res = core.Resolution(state=core.STATE_BOUND, session_id="s1", binding=binding, via="s1")
        hub = mock.Mock()
        hub.device.return_value = conn("windows", "powershell")
        with mock.patch.object(core, "resolve_session", return_value=res), mock.patch.object(core, "hub", return_value=hub):
            out = core.pre_llm_call(session_id="s1")
            again = core.pre_llm_call(session_id="s1")
        self.assertIn("PowerShell", out["context"])
        self.assertIsNone(again, "the note repeats while the device and shell stay")
        hub.device.return_value = conn("windows", "wsl")
        with mock.patch.object(core, "resolve_session", return_value=res), mock.patch.object(core, "hub", return_value=hub):
            self.assertIn("WSL", core.pre_llm_call(session_id="s1")["context"])
        hub.device.return_value = conn("linux", "bash")
        with mock.patch.object(core, "resolve_session", return_value=res), mock.patch.object(core, "hub", return_value=hub):
            self.assertIsNone(core.pre_llm_call(session_id="s1"))
        never = core.Resolution(state=core.STATE_NEVER_BOUND, session_id="s2")
        with mock.patch.object(core, "resolve_session", return_value=never):
            self.assertIsNone(core.pre_llm_call(session_id="s2"))


class PowerShellTest(unittest.TestCase):
    def test_wrapper(self):
        line = core.powershell_command("Get-ChildItem 'a b' | Select-Object -First 1")
        self.assertIn('"${TALARIA_POWERSHELL:-powershell.exe}" -NoLogo -NoProfile -NonInteractive', line)
        encoded = re.search(r"-EncodedCommand (\S+);", line).group(1)
        script = base64.b64decode(encoded).decode("utf-16-le")
        self.assertIn("Get-ChildItem 'a b' | Select-Object -First 1", script)
        self.assertIn("WriteAllText($env:TALARIA_PS_CWD", script)
        self.assertIn("exit $LASTEXITCODE", script)
        self.assertIn('cd "$(cygpath -u "$__tal_dir")"', line)
        self.assertTrue(line.endswith("(exit $__tal_rc)"))
        # No quote of the command reaches bash.
        self.assertNotIn("'a b'", line)


if __name__ == "__main__":
    unittest.main()
