"""How the plugin reports a device command's exit (docs/protocol.md section 4.4). Run inside the
Hermes runtime with scripts/test-plugin.sh."""
from __future__ import annotations

import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_core import Store, core  # noqa: E402
from test_resume import Recorder, output  # noqa: E402


class ExitTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.tmp.cleanup)
        self.hub = core.Hub(Path(self.tmp.name) / "bindings.json", parent_lookup=Store().lookup)
        for p in (mock.patch.object(core, "settings", lambda: {"backend_id": "alice"}),
                  mock.patch.object(core, "hub", lambda: self.hub)):
            p.start()
            self.addCleanup(p.stop)
        self.dev = Recorder(3)
        self.addCleanup(self.dev.stop)
        self.hub.attach(self.dev.conn)

    def run_exit(self, exit_msg: dict) -> tuple[int, str]:
        proc = self.hub.start("d_pc", cmd="rg x", cwd="/home/alice/src", login=False, timeout=60, stdin=None)
        rid = self.dev.execs()[-1]["id"]
        self.hub.on_message(self.dev.conn, {"type": "output", "id": rid, "data": "partial\n", "offset": 0})
        self.hub.on_message(self.dev.conn, {"type": "exit", "id": rid, **exit_msg})
        code = proc.wait(5)
        return code, output(proc)

    def test_oom_kill_tells_the_model(self):
        code, out = self.run_exit({"code": 137, "signal": "SIGKILL", "reason": "oom", "memory_max": 1536 << 20})
        self.assertEqual(code, 137)
        self.assertTrue(out.startswith("partial\n[killed: out of memory (limit 1.5 GiB). "), out)

    def test_oom_kill_without_limit(self):
        _, out = self.run_exit({"code": 143, "signal": "SIGTERM", "reason": "oom", "memory_max": "big"})
        self.assertIn("[killed: out of memory. The device ran out of memory and stopped the command", out)

    def test_signal_alone_adds_no_note(self):
        code, out = self.run_exit({"code": 137, "signal": "SIGKILL"})
        self.assertEqual((code, out), (137, "partial\n"))

    def test_unknown_reason_is_ignored(self):
        code, out = self.run_exit({"code": 1, "reason": "future"})
        self.assertEqual((code, out), (1, "partial\n"))

    def test_format_bytes(self):
        for n, want in ((4 << 30, "4 GiB"), (1536 << 20, "1.5 GiB"), (512 << 20, "512 MiB"),
                        (6442450944, "6 GiB"), (100, "100 bytes")):
            self.assertEqual(core.format_bytes(n), want)


if __name__ == "__main__":
    unittest.main()
