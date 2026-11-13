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
                return json.load(f)

    def test_missing_key_gets_zero(self):
        self.assertEqual(self.run_init()["gateway"], {"replay_buffer_events": 0, "x": 1})
        self.assertEqual(self.run_init({"gateway": {"other": True}})["gateway"]["replay_buffer_events"], 0)

    def test_value_in_config_yaml_wins(self):
        got = self.run_init({"gateway": {"replay_buffer_events": 2000}})
        self.assertEqual(got["gateway"]["replay_buffer_events"], 2000)

    def test_value_in_own_settings_wins(self):
        got = self.run_init(extra={"gateway": {"replay_buffer_events": 512}})
        self.assertEqual(got["gateway"]["replay_buffer_events"], 512)

    def test_product_help_guidance_off_unless_set(self):
        self.assertIs(self.run_init()["agent"]["product_help_guidance"], False)
        got = self.run_init({"agent": {"max_turns": 9}})["agent"]
        self.assertEqual(got, {"max_turns": 9, "product_help_guidance": False})
        self.assertIs(self.run_init({"agent": {"product_help_guidance": True}})["agent"]["product_help_guidance"], True)
        got = self.run_init(extra={"agent": {"product_help_guidance": True}})
        self.assertIs(got["agent"]["product_help_guidance"], True)


class Hint(unittest.TestCase):
    def test_help_points_to_talaria(self):
        with tempfile.TemporaryDirectory() as home:
            m = load(home)
            with mock.patch.object(m, "password_hash", return_value="h"), \
                    mock.patch.object(m, "secret", return_value="s"):
                cfg = m.talaria_config("/k")
        for platform in ("desktop", "cli"):
            hint = cfg["platform_hints"][platform]["replace"]
            self.assertIn("Talaria's Settings", hint)
            self.assertIn("https://usa-reddragon.github.io/talaria/", hint)


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


class WebSearch(unittest.TestCase):
    """The install's SearXNG is the web search backend unless the user chose another (web_search)."""

    URL = "http://searxng:8080"

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.m = load(self.home)

    def tearDown(self):
        self.tmp.cleanup()

    def run_init(self, config, **env):
        env.setdefault("TALARIA_SEARXNG_URL", self.URL)
        with mock.patch.dict(os.environ, env), mock.patch.object(self.m, "HOME", self.home):
            self.m.web_search(config)
        return config

    def dotenv(self):
        path = os.path.join(self.home, ".env")
        if not os.path.exists(path):
            return []
        with open(path) as f:
            return f.read().splitlines()

    def write_dotenv(self, *lines):
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write("".join(line + "\n" for line in lines))

    def test_on_by_default(self):
        got = self.run_init({"model": {"default": "m"}})
        self.assertEqual(got["web"], {"search_backend": "searxng"})
        self.assertIn("SEARXNG_URL=" + self.URL, self.dotenv())
        # Again: nothing doubles
        self.run_init(got)
        self.assertEqual(self.dotenv().count("SEARXNG_URL=" + self.URL), 1)
        self.assertEqual(got["web"], {"search_backend": "searxng"})

    def test_no_searxng_in_the_install(self):
        got = self.run_init({}, TALARIA_SEARXNG_URL="")
        self.assertNotIn("web", got)
        self.assertEqual(self.dotenv(), [])

    def test_backend_the_user_set_wins(self):
        for web in ({"search_backend": "tavily"}, {"backend": "exa"}, {"backend": "firecrawl", "extract_char_limit": 9}):
            got = self.run_init({"web": dict(web)})
            self.assertEqual(got["web"], web)

    def test_later_choice_takes_the_pin_back(self):
        got = self.run_init({})
        got["web"]["backend"] = "tavily"
        self.run_init(got)
        self.assertEqual(got["web"], {"backend": "tavily"})
        # And it stays theirs
        self.run_init(got)
        self.assertEqual(got["web"], {"backend": "tavily"})
        got = self.run_init({"web": {"search_backend": "exa"}})
        self.assertEqual(got["web"], {"search_backend": "exa"})

    def test_turned_off(self):
        self.write_dotenv("OPENROUTER_API_KEY=k")
        got = self.run_init({})
        for off in ("0", "false", "off"):
            got = self.run_init(got, TALARIA_SEARXNG=off)
            self.assertNotIn("web", got)
            self.assertEqual(self.dotenv(), ["OPENROUTER_API_KEY=k"])
            got = self.run_init(got)
            self.assertEqual(got["web"], {"search_backend": "searxng"})

    def test_own_searxng_url_stays(self):
        self.write_dotenv("SEARXNG_URL=https://search.example.com")
        got = self.run_init({})
        self.assertEqual(self.dotenv(), ["SEARXNG_URL=https://search.example.com"])
        self.assertEqual(got["web"], {"search_backend": "searxng"})
        self.run_init(got, TALARIA_SEARXNG="0")
        self.assertEqual(self.dotenv(), ["SEARXNG_URL=https://search.example.com"])

    def test_address_change_moves_the_line(self):
        self.run_init({})
        self.run_init({}, TALARIA_SEARXNG_URL="http://127.0.0.1:8888")
        self.assertEqual([line for line in self.dotenv() if line.startswith("SEARXNG_URL")],
                         ["SEARXNG_URL=http://127.0.0.1:8888"])


if __name__ == "__main__":
    unittest.main()
