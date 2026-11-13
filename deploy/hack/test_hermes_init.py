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


class TimeZone(unittest.TestCase):
    """TZ sets the agent's time zone until someone picks another one in Settings, Agent."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.m = load(self.home)

    def tearDown(self):
        self.tmp.cleanup()

    def run_tz(self, config, tz):
        env = {"TZ": tz} if tz else {}
        with mock.patch.dict(os.environ, env, clear=False):
            if not tz:
                os.environ.pop("TZ", None)
            self.m.time_zone(config)
        return config

    def test_tz_sets_it_and_follows_changes(self):
        self.assertEqual(self.run_tz({}, "America/Chicago")["timezone"], "America/Chicago")
        self.assertEqual(self.run_tz({"timezone": "America/Chicago"}, "Europe/Berlin")["timezone"], "Europe/Berlin")

    def test_a_zone_picked_in_settings_wins_for_good(self):
        self.run_tz({}, "America/Chicago")
        picked = self.run_tz({"timezone": "Asia/Tokyo"}, "America/Chicago")
        self.assertEqual(picked["timezone"], "Asia/Tokyo")
        self.assertEqual(self.run_tz({"timezone": "Asia/Tokyo"}, "Europe/Berlin")["timezone"], "Asia/Tokyo")
        # Even when the picked zone later equals TZ, a TZ change does not take it back
        self.assertEqual(self.run_tz({"timezone": "Europe/Berlin"}, "Europe/Berlin")["timezone"], "Europe/Berlin")
        self.assertEqual(self.run_tz({"timezone": "Europe/Berlin"}, "UTC")["timezone"], "Europe/Berlin")
        with open(os.path.join(self.home, "talaria", "init-state.json")) as f:
            self.assertEqual(json.load(f), {"timezone_owner": "settings"})

    def test_existing_zone_before_this_step_is_kept(self):
        self.assertEqual(self.run_tz({"timezone": "Asia/Tokyo"}, "America/Chicago")["timezone"], "Asia/Tokyo")

    def test_without_tz_its_own_zone_goes(self):
        self.run_tz({}, "America/Chicago")
        self.assertNotIn("timezone", self.run_tz({"timezone": "America/Chicago"}, ""))
        self.assertNotIn("timezone", self.run_tz({}, ""))

    def test_zone_from_before_the_record_still_follows_tz(self):
        self.assertEqual(self.run_tz({"timezone": "America/Chicago"}, "America/Chicago")["timezone"], "America/Chicago")
        self.assertEqual(self.run_tz({"timezone": "America/Chicago"}, "Europe/Berlin")["timezone"], "Europe/Berlin")

    def test_live_apply_is_on(self):
        with mock.patch.object(self.m, "password_hash", return_value="h"), \
                mock.patch.object(self.m, "secret", return_value="s"):
            cfg = self.m.talaria_config("/k")
        self.assertIs(cfg["dashboard"]["apply_to_live_chats"], True)
        self.assertNotIn("timezone", cfg)


class Soul(unittest.TestCase):
    """The agent's personality is Talaria's, unless someone wrote their own."""

    STOCK = ("You are Hermes Agent, built by Nous Research. Be direct: match the length of your reply to the weight of "
             "the ask \u2014 a one-line question gets a one-line answer, and finished work gets a short report of what "
             "changed, what's verified, and what's left, never a replay of the process. No filler (\"Great question,\" "
             "\"I'd be happy to\"), no restating the request back, no re-summarizing what you already said, no narrating "
             "tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree because it's "
             "right, not because the user said it. Depth is earned \u2014 give it when the user asks for detail, teaches, or "
             "the stakes demand it, not by default.")

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.m = load(self.home)
        self.path = os.path.join(self.home, "SOUL.md")

    def tearDown(self):
        self.tmp.cleanup()

    def read(self):
        with open(self.path, encoding="utf-8") as f:
            return f.read()

    def write(self, text):
        with open(self.path, "w", encoding="utf-8", newline="") as f:
            f.write(text)

    def test_new_install_gets_talarias(self):
        self.m.soul()
        self.assertEqual(self.read().strip(), self.m.TALARIA_SOUL)

    def test_stock_text_is_replaced_also_with_other_line_endings_and_spacing(self):
        for text in (self.STOCK, self.STOCK + "\n", "\ufeff" + self.STOCK.replace(". ", ".\r\n") .replace(".\r\n", ". ") + "\r\n\r\n", ""):
            self.write(text)
            self.m.soul()
            self.assertEqual(self.read().strip(), self.m.TALARIA_SOUL, repr(text[:20]))

    def test_the_older_scaffold_is_replaced(self):
        self.write("# Hermes Agent Persona\n\n<!--\nThis file defines the agent's personality and tone.\n"
                   "The agent will embody whatever you write here.\nEdit this to customize how Hermes communicates with you.\n\n"
                   "This file is loaded fresh each message -- no restart needed.\n"
                   "Delete the contents (or this file) to use the default personality.\n-->")
        self.m.soul()
        self.assertEqual(self.read().strip(), self.m.TALARIA_SOUL)

    def test_your_own_text_stays(self):
        for text in ("Answer like a pirate.", self.STOCK + " Also: be brief.", self.m.TALARIA_SOUL + "\nMore."):
            self.write(text)
            self.m.soul()
            self.assertEqual(self.read(), text)

    def test_talarias_own_text_is_not_a_stock_text(self):
        import hashlib
        self.assertNotIn(hashlib.sha256(self.m.TALARIA_SOUL.encode()).hexdigest(), self.m.STOCK_SOULS)
        low = self.m.TALARIA_SOUL.lower()
        for word in ("hermes", "nous", "claude"):
            self.assertNotIn(word, low)

    def test_stock_hashes_match_the_stock_text(self):
        import hashlib
        self.assertIn(hashlib.sha256(self.STOCK.encode()).hexdigest(), self.m.STOCK_SOULS)


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


class LongChatDefaults(unittest.TestCase):
    """Recall on its own turn only, collapsed agent messages, the newest skill kept, old tool output pruned."""

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
                    mock.patch.object(m, "talaria_config", return_value={}), \
                    mock.patch.object(m, "setup_honcho"), \
                    mock.patch.object(m, "demo_provider"), \
                    mock.patch.object(m.subprocess, "run"):
                m.main()
            with open(path) as f:
                return json.load(f)

    def test_unset_gets_the_long_chat_settings(self):
        got = self.run_init()
        self.assertEqual(got["memory"], {"recall_history": False, "recall_max_chars": 8000})
        self.assertEqual(got["context"], {"collapse_agent_messages": True})
        self.assertEqual(got["compression"], {"keep_last_skill_chars": 20000, "proactive_prune_tokens": 100000,
                                              "summary_style": "compact", "decision_ledger_chars": 6000})
        self.assertIs(got["approvals"]["smart_provenance"], True)

    def test_explicit_values_win(self):
        got = self.run_init({"memory": {"provider": "honcho", "recall_history": True},
                             "compression": {"threshold": 0.7, "proactive_prune_tokens": 0}})
        self.assertEqual(got["memory"], {"provider": "honcho", "recall_history": True, "recall_max_chars": 8000})
        self.assertEqual(got["compression"], {"threshold": 0.7, "proactive_prune_tokens": 0,
                                              "keep_last_skill_chars": 20000, "summary_style": "compact",
                                              "decision_ledger_chars": 6000})
        got = self.run_init({"compression": {"summary_style": "legacy", "decision_ledger_chars": 0}})
        self.assertEqual(got["compression"]["summary_style"], "legacy")
        self.assertEqual(got["compression"]["decision_ledger_chars"], 0)
        got = self.run_init(extra={"context": {"collapse_agent_messages": False}})
        self.assertIs(got["context"]["collapse_agent_messages"], False)
        got = self.run_init({"approvals": {"mode": "smart", "smart_provenance": False}})
        self.assertEqual(got["approvals"], {"mode": "smart", "smart_provenance": False})


class StockDefaults(unittest.TestCase):
    """The all-in-one image: the agent's own first-start setup writes a full config.yaml with its stock values before
    this step runs. Stock values get Talaria's, on a new install and on an upgrade; values someone changed stay."""

    # The keys of the agent's seeded config.yaml (cli-config.yaml.example) that Talaria has defaults for
    STOCK = {"memory": {"memory_enabled": True, "recall_history": True, "recall_max_chars": 0},
             "compression": {"enabled": True, "keep_last_skill_chars": 0, "proactive_prune_tokens": 0},
             "agent": {"max_turns": 90}, "gateway": None}
    TALARIA = {"memory.recall_history": False, "memory.recall_max_chars": 8000,
               "compression.keep_last_skill_chars": 20000, "compression.proactive_prune_tokens": 100000,
               "compression.summary_style": "compact", "compression.decision_ledger_chars": 6000,
               "gateway.replay_buffer_events": 0, "vision.resize_mode": "fit", "approvals.smart_provenance": True}

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.m = load(self.home)
        # Only the all-in-one image names the seeded file
        self.env = {"TALARIA_STOCK_CONFIG": os.path.join(self.home, "stock.yaml")}
        with open(self.env["TALARIA_STOCK_CONFIG"], "w") as f:
            json.dump(self.STOCK, f)

    def tearDown(self):
        self.tmp.cleanup()

    def run_init(self, existing=None):
        path = os.path.join(self.home, "config.yaml")
        if existing is not None:
            with open(path, "w") as f:
                json.dump(existing, f)
        with mock.patch.dict(os.environ, self.env), \
                mock.patch.object(self.m, "install_plugin"), \
                mock.patch.object(self.m, "write_public_key", return_value="/k"), \
                mock.patch.object(self.m, "talaria_config", return_value={}), \
                mock.patch.object(self.m, "setup_honcho"), \
                mock.patch.object(self.m, "demo_provider"), \
                mock.patch.object(self.m.subprocess, "run"):
            self.m.main()
        with open(path) as f:
            return json.load(f)

    def assertTalaria(self, got, *but):
        for key, want in self.TALARIA.items():
            if key not in but:
                self.assertIs(type(self.m.lookup(got, key)), type(want), key)
                self.assertEqual(self.m.lookup(got, key), want, key)

    def stock(self, **changes):
        cfg = json.loads(json.dumps(self.STOCK))
        for key, value in changes.items():
            section, name = key.split("__")
            cfg.setdefault(section, {})[name] = value
        return cfg

    def test_fresh_install_gets_talarias_over_the_seeded_stock_values(self):
        got = self.run_init(self.stock())
        self.assertTalaria(got)
        self.assertIs(got["memory"]["memory_enabled"], True)
        self.assertEqual(got["agent"]["max_turns"], 90)
        # A second start changes nothing
        self.assertEqual(self.run_init(), got)

    def test_upgrade_with_stock_values_gets_talarias(self):
        # An install from before this record: the stock values on disk, next to the keys the old step filled in
        old = self.stock(compression__summary_style="compact", compression__decision_ledger_chars=6000,
                         approvals__smart_provenance=True)
        old["gateway"] = None
        self.assertFalse(os.path.exists(os.path.join(self.home, "talaria", "defaults.json")))
        self.assertTalaria(self.run_init(old))

    def test_upgrade_keeps_values_someone_changed(self):
        old = self.stock(memory__recall_max_chars=4000, compression__proactive_prune_tokens=50000,
                         compression__summary_style="legacy")
        got = self.run_init(old)
        self.assertEqual(got["memory"]["recall_max_chars"], 4000)
        self.assertEqual(got["compression"]["proactive_prune_tokens"], 50000)
        self.assertEqual(got["compression"]["summary_style"], "legacy")
        self.assertTalaria(got, "memory.recall_max_chars", "compression.proactive_prune_tokens",
                           "compression.summary_style")
        self.assertEqual(self.run_init(), got)

    def test_a_value_changed_after_this_step_is_yours_for_good(self):
        got = self.run_init(self.stock())
        # Set back to the stock value in Settings: a choice, and it stays
        got["memory"]["recall_history"] = True
        got["compression"]["keep_last_skill_chars"] = 0
        got = self.run_init(got)
        self.assertIs(got["memory"]["recall_history"], True)
        self.assertEqual(got["compression"]["keep_last_skill_chars"], 0)
        self.assertEqual(self.run_init(got)["memory"]["recall_history"], True)
        with open(os.path.join(self.home, "talaria", "defaults.json")) as f:
            state = json.load(f)
        self.assertIn("memory.recall_history", state["yours"])
        self.assertNotIn("memory.recall_history", state["applied"])

    def test_a_new_talaria_default_replaces_the_one_this_step_set(self):
        self.run_init(self.stock())
        with mock.patch.dict(self.m.DEFAULTS["memory"], {"recall_max_chars": 6000}):
            self.assertEqual(self.run_init()["memory"]["recall_max_chars"], 6000)

    def test_compose_install_with_talarias_values_already_set(self):
        # Compose ran this step before the agent ever started, so config.yaml holds Talaria's values and no record
        got = self.run_init({"memory": {"recall_history": False, "recall_max_chars": 8000}})
        self.assertTalaria(got)

    def test_without_a_seeded_file_a_stock_value_is_a_choice(self):
        # Compose and the chart: the agent never seeded config.yaml, so someone wrote recall_history: true
        self.env = {}
        os.environ.pop("TALARIA_STOCK_CONFIG", None)
        got = self.run_init({"memory": {"recall_history": True}})
        self.assertIs(got["memory"]["recall_history"], True)
        self.assertTalaria(got, "memory.recall_history")

    def test_false_is_not_zero(self):
        # replay_buffer_events false is no stock value of the integer key
        got = self.run_init({"gateway": {"replay_buffer_events": False}})
        self.assertIs(got["gateway"]["replay_buffer_events"], False)


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
