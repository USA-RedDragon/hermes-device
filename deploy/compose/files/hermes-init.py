"""Talaria setup of the Hermes home, run before the Hermes gateway starts.

Runs in the Hermes image as its hermes user, with HERMES_HOME on the data volume. Each start it:

- copies the talaria device plugin (written by the plugin step into /plugin-src) into the home,
- writes the hub's public key, derived from the hub's signing key, for the device plugin,
- merges the Talaria settings into config.yaml (other settings you change stay),
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
        "dashboard": {"basic_auth": {"username": "hub", "password_hash": password_hash(),
                                     "secret": secret("session-secret")}, "detach_removed_default": True,
                      "managed_read_only": True},
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
    tz = env("TZ")
    if tz:
        cfg["timezone"] = tz
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


# Hermes settings Talaria defaults to: each is set only when config.yaml does not have it yet, so a value you set
# (in config.yaml or your own settings) wins.
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
}


def fill(base, defaults):
    for key, value in defaults.items():
        if isinstance(value, dict):
            if key not in base:
                base[key] = {}
            if isinstance(base[key], dict):
                fill(base[key], value)
        elif key not in base:
            base[key] = value


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
    path = os.path.join(HOME, "config.yaml")
    config = {}
    if os.path.exists(path):
        with open(path) as f:
            config = safe_load(f) or {}
    fill(config, DEFAULTS)
    merge(config, talaria_config(pubkey))
    extra = env("TALARIA_EXTRA_CONFIG")
    if extra and os.path.exists(extra):
        # Your own Hermes settings (Helm value hermes.config), merged last
        with open(extra) as f:
            merge(config, safe_load(f) or {})
    demo_provider(config)
    with open(path + ".tmp", "w") as f:
        safe_dump(config, f, sort_keys=False)
    os.chmod(path + ".tmp", 0o600)
    os.replace(path + ".tmp", path)
    subprocess.run([HERMES, "plugins", "enable", "device"], check=False)
    setup_honcho()
    print("hermes-init: done")


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
