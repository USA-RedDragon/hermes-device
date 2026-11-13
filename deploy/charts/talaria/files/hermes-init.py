"""Talaria setup of the Hermes home, run before the Hermes gateway starts.

Runs in the Hermes image as its hermes user, with HERMES_HOME on the data volume. Each start it:

- copies the talaria device plugin (written by the plugin step into /plugin-src) into the home,
- writes the hub's public key, derived from the hub's signing key, for the device plugin,
- merges the Talaria settings into config.yaml (other settings you change stay),
- gives the agent Talaria's personality (SOUL.md) when it has none or still has the stock text,
- enables the device plugin.

Settings come from environment variables (see .env.example). Secrets come from /secrets (or
TALARIA_SECRETS_DIR). The same script runs in the Helm chart, which also sets TALARIA_ROLLING (the settings
for zero-downtime rolling updates) and TALARIA_SKILLS_SYNC.
"""

import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys

sys.path.insert(0, "/opt/hermes")

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import load_pem_private_key
from hermes_yaml import safe_dump, safe_load

HOME = os.environ.get("HERMES_HOME", "/opt/data")
SECRETS = os.environ.get("TALARIA_SECRETS_DIR", "/secrets")
HERMES = "/opt/hermes/.venv/bin/hermes"


def secret(name):
    with open(os.path.join(SECRETS, name)) as f:
        return f.read().strip()


def env(name, default=""):
    return os.environ.get(name, "").strip() or default


def install_plugin():
    src = os.path.join(env("TALARIA_PLUGIN_SRC", "/plugin-src"), "device")
    dest = os.path.join(HOME, "plugins", "device")
    if not os.path.isdir(src):
        sys.exit("hermes-init: no device plugin in /plugin-src; the plugin step did not run")
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    if os.path.isdir(dest) and tree_hash(src) == tree_hash(dest):
        print("hermes-init: device plugin unchanged")
        return
    # Two renames: during a rolling update the old pod's gateway may still load it
    tmp, old = dest + ".new", dest + ".old"
    shutil.rmtree(tmp, ignore_errors=True)
    shutil.rmtree(old, ignore_errors=True)
    shutil.copytree(src, tmp)
    if os.path.isdir(dest):
        os.rename(dest, old)
    os.rename(tmp, dest)
    shutil.rmtree(old, ignore_errors=True)
    with open(os.path.join(dest, "VERSION")) as f:
        print("hermes-init: device plugin", f.read().strip())


def tree_hash(root):
    h = hashlib.sha256()
    for dirpath, dirnames, files in sorted(os.walk(root)):
        dirnames.sort()
        for name in sorted(files):
            path = os.path.join(dirpath, name)
            h.update(os.path.relpath(path, root).encode() + b"\0")
            with open(path, "rb") as f:
                h.update(f.read())
    return h.hexdigest()


def write_public_key():
    """The device plugin checks the hub's signed assertions with the hub's Ed25519 public key."""
    raw = secret("hub-signing-key")
    if raw.startswith("-----BEGIN"):
        key = load_pem_private_key(raw.encode(), password=None)
    else:
        key = Ed25519PrivateKey.from_private_bytes(base64.b64decode(raw))
    pub = key.public_key().public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo)
    path = os.path.join(HOME, "talaria", "hub_public_key.pem")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path + ".tmp", "wb") as f:
        f.write(pub)
    os.replace(path + ".tmp", path)
    return path


def hub_user():
    """The hub account of the person this Hermes works for, or "" when not set (TALARIA_USER)."""
    value = env("TALARIA_USER")
    if not value and os.path.exists(os.path.join(SECRETS, "user")):
        value = secret("user")
    return value


def password_hash():
    # Hermes's own hash, so the plain password never sits in config.yaml
    sys.path.insert(0, "/opt/hermes")
    from plugins.dashboard_auth.basic import hash_password
    return hash_password(secret("password"))


# The agent's personality, SOUL.md, for a new install: Talaria's own text instead of the stock one. The hub's
# Settings, Agent has the same text (hub/internal/agentcfg, kept equal by a test) for "Reset to Talaria's default".
TALARIA_SOUL = """\
You are the agent in Talaria, a personal assistant that works for the person who runs it. Be direct: match the \
length of your reply to the weight of the ask, so a one-line question gets a one-line answer and finished work \
gets a short report of what changed, what you checked, and what is left. No filler, no restating the request, \
no narrating tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree \
because it is right, not because the user said it. Give depth when the user asks for detail or the stakes demand \
it, not by default."""

# SHA-256 of the stock texts (stripped, line endings unified) that the agent's image seeds into SOUL.md. A file that
# holds one of them, or nothing, carries no choice of yours, so it becomes Talaria's text. Any other text is yours.
STOCK_SOULS = frozenset((
    "36c1f5a2e92cd1d018311eaf4c8f1e8886672eae78212e033c681d0e3d5d506f",
    "731f9a403cdc6a5c5c0e5c956a9238f54dae8b6e253bc29f9e47efb38eb6081a",
    "2765a846e1bb371d78d3b93b403dfb0f8d1ba1a9895edb5f608367abfe81194d",
    "82fa3438d86f8b95c6e96be89d853c8b561b3e6973ed61c90e70e0231dd24a7e",
    "6bc146a50e8fe3bd555e89a77e93af1f781cece8cf549f0d7a02ee2389811c72",
))


def soul():
    """Writes TALARIA_SOUL to SOUL.md when it is missing, empty or one of the stock texts."""
    path = os.path.join(HOME, "SOUL.md")
    if os.path.exists(path):
        with open(path, encoding="utf-8", errors="replace") as f:
            text = f.read().replace("\r\n", "\n").replace("\r", "\n").lstrip("\ufeff").strip()
        if text and hashlib.sha256(text.encode()).hexdigest() not in STOCK_SOULS:
            return
    with open(path + ".tmp", "w", encoding="utf-8") as f:
        f.write(TALARIA_SOUL + "\n")
    os.replace(path + ".tmp", path)
    print("hermes-init: personality set to Talaria's")


def talaria_config(pubkey_path):
    hint = ("You are chatting through Talaria: the hub web app or its Telegram bot, which show the same chats. "
            "The web app renders GitHub-flavored Markdown: headings, lists, task lists, tables, links and fenced "
            "code. It does not render math, HTML widgets or ::directives. Show an image with "
            "![description](/absolute/path.png) and link any other file as [name](/absolute/path); the hub serves "
            "files your reply names this way. Save such files in the chat's working folder, never in /tmp. "
            "When someone asks how to set something up or change a setting, point them to Talaria's Settings (the "
            "gear below the chat list) and to the docs at https://usa-reddragon.github.io/talaria/, never to hermes "
            "commands or the agent's config files: they may have no access to the server.")
    cfg = {
        "webapp": {"enabled": True, "host": "0.0.0.0", "port": 9120},
        # detach_removed_default: removing the default model's provider in Settings > Models also drops the default
        # managed_read_only: models and keys the deployment sets (its environment, a managed config) are read-only there
        # apply_to_live_chats: tools, skills, connections and the personality changed in Settings apply to open chats
        # from their next message (patched Hermes)
        "dashboard": {"basic_auth": {"username": "hub", "password_hash": password_hash(),
                                     "secret": secret("session-secret")}, "detach_removed_default": True,
                      "managed_read_only": True, "apply_to_live_chats": True},
        "terminal": {"backend": "device", "cwd": os.path.join(HOME, "empty-cwd")},
        "security": {"image_render_paths": "media"},
        "updates": {"check": False},
        "platform_hints": {"desktop": {"replace": hint}, "cli": {"replace": hint}},
        "plugins": {"entries": {"device": {
            # Lets the plugin hide Hermes's browser_vault tool. The tool registry of Hermes v0.23.1 still reads
            # the older allow_tool_override key, so set both
            "granted_capabilities": ["tools.override"],
            "allow_tool_override": True,
            "settings": {
                "backend_id": "main",
                "placeholder": os.path.join(HOME, "device-placeholder"),
                "fallback": "device",
                "fallback_device": "sandbox",
                "fallback_root": env("TALARIA_FALLBACK_ROOT", "/work"),
                # Unset: a chat that is on no computer belongs to the one hub user whose computers are connected
                "user": hub_user() or None,
                "hub_public_key_file": pubkey_path,
                "reconnect_wait_seconds": 60,
            },
        }}},
    }
    # The model and provider are Hermes's own settings (hermes model, hermes setup); Talaria does not set them
    if env("TALARIA_ROLLING"):
        merge(cfg, ROLLING)
    return cfg


# Zero-downtime rolling updates on Kubernetes (patched Hermes): SIGTERM opens a restart wait where running turns
# finish and approvals stay answerable, the new pod's gateway waits as a standby for this one's lock, and the
# Webapp listener is handed over and relays connections until the new gateway is ready. See https://usa-reddragon.github.io/talaria/deploy/helm.html
ROLLING = {
    "gateway": {
        "sigterm_restart_wait": True,
        "runtime_lock_wait_seconds": 840,
        "restart_wait_plugin_sockets": ["device"],
        "restart_agent_cleanup_seconds": 5,
        "restart_wait_delegations": False,
        "restart_listener_handover": True,
        "restart_listener_relay_seconds": 30,
        "listener_start_hold_seconds": 20,
        "restart_listener_hold_seconds": 90,
    },
    "agent": {"restart_after_turn_timeout": 540, "restart_drain_timeout": 30},
    "delegation": {"restart_handoff": True, "restart_handoff_grace_seconds": 5},
    "terminal": {"process_handoff_backends": ["device"]},
}


# Hermes settings Talaria defaults to (see apply_defaults): a value you set (in config.yaml or your own settings)
# wins, while a key that is missing or still holds the agent's stock value gets Talaria's.
DEFAULTS = {
    # Keep every event of a long reply for the hub when it reconnects mid-reply (patched Hermes; upstream keeps 512
    # events, a few KB of a streaming reply, and the hub then cannot show the reply's start). The replay stays
    # bounded by bytes.
    "gateway": {"replay_buffer_events": 0},
    # Images reach the model the way Claude Code sends them (patched Hermes 0061): scaled once to 2000 px, PNG kept
    # up to 512 KB. Upstream halves anything over 1568 px or 256 KB to a JPEG, so 10 px text is unreadable.
    "vision": {"embed_max_dimension": 2000, "embed_target_bytes": 524288, "resize_mode": "fit"},
    # Leave out the prompt paragraph and the skill that answer "how do I set this up" with the agent's own commands
    # and docs; Talaria's hint above points to its Settings and docs instead (patched Hermes)
    "agent": {"product_help_guidance": False},
    # Long chats (patched Hermes 0069, 0070 and 0075): recalled memory (Honcho) leaves older turns once the prompt
    # cache is cold or at compaction, cut by section to 8000 chars, and older agent messages lose their repeated instructions paragraph. A real chat carried
    # an 18K-char recall block on every user turn, about 99% of each row.
    "memory": {"recall_history": False, "recall_max_chars": 8000},
    "context": {"collapse_agent_messages": True},
    # Compaction keeps the newest loaded skill whole up to 20000 chars instead of a reload marker (patched Hermes
    # 0015; Claude Code re-attaches up to 5K tokens per skill, and 40 of 44 skills measured fit), and old tool
    # output over 8000 chars is cut to a one-line summary once a chat passes 100K tokens (upstream's proactive
    # prune; the trigger of Anthropic's own tool-result clearing), so a long chat compacts less often. The prune
    # spares the skill compaction keeps (patched Hermes 0071). The summary is the compact layout: the model writes
    # three short sections and code writes the user's messages, agents' approval claims and the touched files, with
    # a checked reply and a fallback that needs no model (patched Hermes 0079); a decision ledger (the user's
    # approvals with their questions, dispatched work, tags) rides every handoff verbatim (0078). In the compaction
    # eval this kept recall on models from qwen3.5:2b to Claude Opus through three compactions, with the fewest
    # wrong attributions and about 60% fewer summarizer tokens than upstream's layout.
    "compression": {"keep_last_skill_chars": 20000, "proactive_prune_tokens": 100000,
                    "summary_style": "compact", "decision_ledger_chars": 6000},
    # Smart approval judges each risky step by who asked for it (patched Hermes 0077): only the user's own words
    # can clear a force push, a deletion or a publish, never a file, page, script output or another agent, and a
    # block names its rule. It takes effect with approvals.mode: smart.
    "approvals": {"smart_provenance": True},
}


UNSET = object()


def leaves(tree, prefix=""):
    for key, value in tree.items():
        if isinstance(value, dict):
            yield from leaves(value, prefix + key + ".")
        else:
            yield prefix + key, value


def lookup(tree, dotted):
    for key in dotted.split("."):
        if not isinstance(tree, dict) or key not in tree:
            return UNSET
        tree = tree[key]
    return tree


def same(a, b):
    # By type too: in Python False == 0 and True == 1
    return type(a) is type(b) and a == b


def stock_config():
    """The config file the agent's image seeds into a new home (TALARIA_STOCK_CONFIG), or {}.

    Only the all-in-one image sets it: there the agent's own first-start setup runs before this step and writes that
    file as config.yaml. Compose and the chart run this step first, so a stock value in their config.yaml was set by
    someone."""
    path = env("TALARIA_STOCK_CONFIG")
    if not path or not os.path.exists(path):
        return {}
    try:
        with open(path) as f:
            return safe_load(f) or {}
    except Exception as e:  # noqa: BLE001 - a broken stock file only loses the comparison
        print("hermes-init: could not read the stock config:", e)
        return {}


def apply_defaults(config):
    """Puts DEFAULTS into config, unless you chose a value yourself.

    A key gets Talaria's value when it is missing, empty, already Talaria's value, or the value of the agent's seeded
    config file (see stock_config: on the all-in-one image the agent's own first-start setup writes it before this
    step, so its values carry no choice of yours; an install from before this record has them on disk, and an
    upgrade gives them Talaria's values too). What this step set is remembered in
    talaria/defaults.json. Once a key differs from that, you changed it, and from then on it is yours for good, also
    when you later set it back to the stock value. A key that held any other value when first seen is yours too.
    """
    path = os.path.join(HOME, "talaria", "defaults.json")
    state = {}
    if os.path.exists(path):
        with open(path) as f:
            state = json.load(f)
    applied = dict(state.get("applied") or {})
    yours = set(state.get("yours") or [])
    stock = stock_config()
    changed = []
    for key, want in leaves(DEFAULTS):
        if key in yours:
            continue
        parts = key.split(".")
        node = config
        for part in parts[:-1]:
            if node.get(part) is None:
                # Missing, or a section with every key commented out (the stock gateway: section)
                node[part] = {}
            node = node[part]
            if not isinstance(node, dict):
                break
        if not isinstance(node, dict):
            # A section that is not a mapping is yours; leave it as it is
            continue
        have = node.get(parts[-1], UNSET)
        if have is UNSET or have is None:
            ours = True
        elif key in applied:
            ours = same(have, applied[key])
        else:
            ours = same(have, want) or same(have, lookup(stock, key))
        if not ours:
            yours.add(key)
            applied.pop(key, None)
            continue
        if not same(have, want):
            changed.append(key)
        node[parts[-1]] = applied[key] = want
    new = {"applied": applied, "yours": sorted(yours)}
    if new != state:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path + ".tmp", "w") as f:
            json.dump(new, f, indent=1, sort_keys=True)
        os.replace(path + ".tmp", path)
    if changed:
        print("hermes-init: Talaria defaults set:", ", ".join(changed))


def merge(base, top):
    for key, value in top.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            merge(base[key], value)
        else:
            base[key] = value


def main():
    for d in ("empty-cwd", "device-placeholder"):
        os.makedirs(os.path.join(HOME, d), mode=0o700, exist_ok=True)
    install_plugin()
    if env("TALARIA_SKILLS_SYNC"):
        # The image's own entrypoint does this; the chart runs Hermes without it
        subprocess.run([sys.executable, "/opt/hermes/tools/skills_sync.py"], check=False)
    pubkey = write_public_key()
    soul()
    path = os.path.join(HOME, "config.yaml")
    config = {}
    if os.path.exists(path):
        with open(path) as f:
            config = safe_load(f) or {}
    apply_defaults(config)
    merge(config, talaria_config(pubkey))
    extra = env("TALARIA_EXTRA_CONFIG")
    if extra and os.path.exists(extra):
        # Your own Hermes settings (Helm value hermes.config), merged last
        with open(extra) as f:
            merge(config, safe_load(f) or {})
    time_zone(config)
    demo_provider(config)
    web_search(config)
    with open(path + ".tmp", "w") as f:
        safe_dump(config, f, sort_keys=False)
    os.chmod(path + ".tmp", 0o600)
    os.replace(path + ".tmp", path)
    subprocess.run([HERMES, "plugins", "enable", "device"], check=False)
    setup_honcho()
    print("hermes-init: done")


def time_zone(config):
    """The agent's time zone (timezone) from TZ, while it is still the one this step wrote.

    What it wrote is remembered in talaria/init-state.json. Once the time zone differs from that, someone picked
    another one (in Talaria's Settings, Agent), and from then on that choice wins: this step leaves timezone alone
    for good, also when TZ changes later. Without TZ, a time zone this step wrote is taken back.
    """
    state_path = os.path.join(HOME, "talaria", "init-state.json")
    state = {}
    if os.path.exists(state_path):
        with open(state_path) as f:
            state = json.load(f)
    new = dict(state)
    tz, current, written = env("TZ"), config.get("timezone"), state.get("timezone")
    if not state and current == tz:
        # Written by this step before it kept a record
        written = current
    if current and current != written:
        new["timezone_owner"] = "settings"
    if new.get("timezone_owner") == "settings":
        new.pop("timezone", None)
    elif tz:
        config["timezone"] = new["timezone"] = tz
    else:
        if current and current == written:
            config.pop("timezone", None)
        new.pop("timezone", None)
    if new != state:
        os.makedirs(os.path.dirname(state_path), exist_ok=True)
        with open(state_path + ".tmp", "w") as f:
            json.dump(new, f)
        os.replace(state_path + ".tmp", state_path)


DEMO = "talaria-demo"


def demo_provider(config):
    """With TALARIA_DEMO_URL (the demo model is on), offer it as a Hermes provider named talaria-demo (in Hermes's
    providers: section), so Talaria can be tried without a model provider. It never becomes the default model:
    pick it with `hermes model`. Without it, take that provider out again. Other providers stay as they are."""
    old = config.get("custom_providers")
    if isinstance(old, list):
        # An entry in the older custom_providers list, which Hermes moves into providers:
        old = [p for p in old if not (isinstance(p, dict) and p.get("name") == DEMO)]
        if old:
            config["custom_providers"] = old
        else:
            config.pop("custom_providers", None)
    providers = config.get("providers") if isinstance(config.get("providers"), dict) else {}
    providers.pop(DEMO, None)
    url = env("TALARIA_DEMO_URL")
    if url and env("TALARIA_DEMO_PROBE"):
        # Compose names the demo model always; it runs only with the demo profile
        import urllib.request
        try:
            urllib.request.urlopen(url.rstrip("/") + "/models", timeout=5).read()
        except OSError:
            url = ""
    if url:
        print("hermes-init: the demo model is a Hermes provider, talaria-demo (pick it with: hermes model)")
        providers[DEMO] = {"name": DEMO, "api": url, "api_key": "none", "default_model": DEMO,
                           "models": {DEMO: {"context_length": 131072}}}
    if providers:
        config["providers"] = providers
    else:
        config.pop("providers", None)


def searxng_url():
    """Address of the SearXNG that the install runs (TALARIA_SEARXNG_URL), or "" when there is none or
    TALARIA_SEARXNG turns it off."""
    if env("TALARIA_SEARXNG", "1").lower() in ("0", "false", "no", "off"):
        return ""
    return env("TALARIA_SEARXNG_URL")


SEARXNG_ENV_NOTE = "# Set by Talaria: its own SearXNG (TALARIA_SEARXNG=0 turns it off)"


def web_search(config):
    """Web search through the install's SearXNG, unless you chose another way.

    Hermes reads the SearXNG address from SEARXNG_URL (its environment, then its .env file), and the backend
    from web.search_backend. This writes SEARXNG_URL into the Hermes .env and sets web.search_backend to
    searxng while neither web.backend nor web.search_backend is set. What it wrote is remembered in
    talaria/web-search.json, so it takes back only its own settings: when SearXNG is turned off, when you set
    SEARXNG_URL yourself, or when you later pick another backend in web.backend or web.search_backend.
    """
    url = searxng_url()
    state_path = os.path.join(HOME, "talaria", "web-search.json")
    state = {}
    if os.path.exists(state_path):
        with open(state_path) as f:
            state = json.load(f)
    new = {"env_url": searxng_env(url, state.get("env_url") or ""), "pinned": False}
    web = config.get("web")
    if url and not isinstance(web, dict):
        web = config["web"] = {}
    if isinstance(web, dict):
        backend, search = (str(web.get(k) or "").strip().lower() for k in ("backend", "search_backend"))
        ours = state.get("pinned") and search == "searxng"
        if url and not backend and (not search or ours):
            web["search_backend"] = "searxng"
            new["pinned"] = True
        elif ours:
            # SearXNG is off, or you picked another backend since: take the pin back
            web.pop("search_backend", None)
        if not web:
            config.pop("web", None)
    if new != state and (state or url):
        os.makedirs(os.path.dirname(state_path), exist_ok=True)
        with open(state_path + ".tmp", "w") as f:
            json.dump(new, f)
        os.replace(state_path + ".tmp", state_path)
    if new["pinned"]:
        print("hermes-init: web search through SearXNG at", url)


def searxng_env(url, written):
    """Puts SEARXNG_URL=url in the Hermes .env, or takes out the line written before (written) when url is
    "". A SEARXNG_URL line with any other value is yours and stays. Returns what the .env now holds from us."""
    path = os.path.join(HOME, ".env")
    lines = []
    if os.path.exists(path):
        with open(path) as f:
            lines = f.read().splitlines()
    keep, theirs = [], False
    for line in lines:
        if line == SEARXNG_ENV_NOTE:
            continue
        name, _, value = line.strip().removeprefix("export ").partition("=")
        if name.strip() == "SEARXNG_URL":
            value = value.strip().strip("\"'")
            if not written or value != written:
                theirs = True
            else:
                continue
        keep.append(line)
    if url and not theirs:
        keep += [SEARXNG_ENV_NOTE, "SEARXNG_URL=" + url]
    if keep != lines:
        with open(path + ".tmp", "w") as f:
            f.write("".join(line + "\n" for line in keep))
        os.chmod(path + ".tmp", 0o600)
        os.replace(path + ".tmp", path)
    return url if url and not theirs else ""


def setup_honcho():
    """Honcho memory, when TALARIA_HONCHO_URL is set: the catalog plugin and its settings."""
    url = env("TALARIA_HONCHO_URL")
    if not url:
        return
    # The hub's Memory page asks Honcho for the same peer (init.sh, or the chart's hub config)
    user = hub_user() or "user"
    conf = {"hosts": {"hermes": {"baseUrl": url, "workspace": "talaria", "peerName": user, "aiPeer": "hermes",
                                 "sessionStrategy": "per-session", "pinUserPeer": True}}}
    path = os.path.join(HOME, "honcho.json")
    with open(path + ".tmp", "w") as f:
        json.dump(conf, f, indent=2)
    os.replace(path + ".tmp", path)
    if not os.path.isdir(os.path.join(HOME, "plugins", "honcho")):
        subprocess.run([HERMES, "plugins", "install", "honcho", "--yes-deps"], check=False)
    subprocess.run([HERMES, "plugins", "enable", "honcho"], check=False)
    cfg_path = os.path.join(HOME, "config.yaml")
    with open(cfg_path) as f:
        config = safe_load(f) or {}
    merge(config, {"memory": {"provider": "honcho"}})
    with open(cfg_path + ".tmp", "w") as f:
        safe_dump(config, f, sort_keys=False)
    os.chmod(cfg_path + ".tmp", 0o600)
    os.replace(cfg_path + ".tmp", cfg_path)


if __name__ == "__main__":
    main()
