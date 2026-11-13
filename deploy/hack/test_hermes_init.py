#!/usr/bin/env python3
"""Tests of compose/files/hermes-init.py that need no Hermes image: the defaults it fills into config.yaml.

  python3 -I deploy/hack/test_hermes_init.py
"""

import importlib.util
import json
import os
import sys
import tempfile
import types
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "..", "compose", "files", "hermes-init.py")


def load(home):
    # The image's modules, stubbed: hermes_yaml as JSON (a subset of YAML), cryptography unused here.
    stubs = {"hermes_yaml": types.ModuleType("hermes_yaml")}
    stubs["hermes_yaml"].safe_load = lambda f: json.load(f)
    stubs["hermes_yaml"].safe_dump = lambda data, f, sort_keys=False: json.dump(data, f)
    for name in ("cryptography", "cryptography.hazmat", "cryptography.hazmat.primitives",
                 "cryptography.hazmat.primitives.asymmetric", "cryptography.hazmat.primitives.asymmetric.ed25519",
                 "cryptography.hazmat.primitives.serialization"):
        mod = types.ModuleType(name)
        mod.serialization = mod.Ed25519PrivateKey = mod.load_pem_private_key = None
        stubs[name] = mod
    with mock.patch.dict(sys.modules, stubs), mock.patch.dict(os.environ, {"HERMES_HOME": home}):
        spec = importlib.util.spec_from_file_location("hermes_init", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


class Defaults(unittest.TestCase):
    def run_init(self, existing=None, extra=None):
        with tempfile.TemporaryDirectory() as home:
            m = load(home)
            path = os.path.join(home, "config.yaml")
            if existing is not None:
                with open(path, "w") as f:
                    json.dump(existing, f)
            env = {}
            if extra is not None:
                env["TALARIA_EXTRA_CONFIG"] = os.path.join(home, "extra.yaml")
                with open(env["TALARIA_EXTRA_CONFIG"], "w") as f:
                    json.dump(extra, f)
            with mock.patch.dict(os.environ, env), \
                    mock.patch.object(m, "install_plugin"), \
                    mock.patch.object(m, "write_public_key", return_value="/k"), \
                    mock.patch.object(m, "talaria_config", return_value={"gateway": {"x": 1}}), \
                    mock.patch.object(m, "setup_honcho"), \
                    mock.patch.object(m, "demo_provider"), \
                    mock.patch.object(m.subprocess, "run"):
                m.main()
            with open(path) as f:
                return json.load(f)["gateway"]

    def test_missing_key_gets_zero(self):
        self.assertEqual(self.run_init(), {"replay_buffer_events": 0, "x": 1})
        self.assertEqual(self.run_init({"gateway": {"other": True}})["replay_buffer_events"], 0)

    def test_value_in_config_yaml_wins(self):
        self.assertEqual(self.run_init({"gateway": {"replay_buffer_events": 2000}})["replay_buffer_events"], 2000)

    def test_value_in_own_settings_wins(self):
        self.assertEqual(self.run_init(extra={"gateway": {"replay_buffer_events": 512}})["replay_buffer_events"], 512)


class VisionDefaults(unittest.TestCase):
    """Images reach the model at Claude Code's size: 2000 px, 512 KB, scaled once (patched Hermes 0061)."""

    def vision(self, existing=None, extra=None):
        with tempfile.TemporaryDirectory() as home:
            m = load(home)
            path = os.path.join(home, "config.yaml")
            if existing is not None:
                with open(path, "w") as f:
                    json.dump(existing, f)
            env = {}
            if extra is not None:
                env["TALARIA_EXTRA_CONFIG"] = os.path.join(home, "extra.yaml")
                with open(env["TALARIA_EXTRA_CONFIG"], "w") as f:
                    json.dump(extra, f)
            with mock.patch.dict(os.environ, env), \
                    mock.patch.object(m, "install_plugin"), \
                    mock.patch.object(m, "write_public_key", return_value="/k"), \
                    mock.patch.object(m, "talaria_config", return_value={}), \
                    mock.patch.object(m, "setup_honcho"), \
                    mock.patch.object(m, "demo_provider"), \
                    mock.patch.object(m.subprocess, "run"):
                m.main()
            with open(path) as f:
                return json.load(f)["vision"]

    def test_unset_gets_claude_code_sizes(self):
        want = {"embed_max_dimension": 2000, "embed_target_bytes": 524288, "resize_mode": "fit"}
        self.assertEqual(self.vision(), want)
        self.assertEqual(self.vision({"vision": {"max_calls_per_image": 2}}), dict(want, max_calls_per_image=2))

    def test_explicit_values_win(self):
        got = self.vision({"vision": {"embed_max_dimension": 1568, "resize_mode": "halve"}})
        self.assertEqual(got, {"embed_max_dimension": 1568, "resize_mode": "halve", "embed_target_bytes": 524288})
        got = self.vision(extra={"vision": {"embed_target_bytes": 262144}})
        self.assertEqual(got["embed_target_bytes"], 262144)
        self.assertEqual(got["resize_mode"], "fit")


if __name__ == "__main__":
    unittest.main()
