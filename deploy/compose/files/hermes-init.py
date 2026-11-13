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
            "files your reply names this way. Save such files in the chat's working folder, never in /tmp.")
    cfg = {
        "webapp": {"enabled": True, "host": "0.0.0.0", "port": 9120},
        "dashboard": {"basic_auth": {"username": "hub", "password_hash": password_hash(),
                                     "secret": secret("session-secret")}},
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
    base_url, model = env("TALARIA_MODEL_BASE_URL"), env("TALARIA_MODEL")
    if base_url and model:
        m = {"provider": "custom", "base_url": base_url, "default": model,
             "context_length": int(env("TALARIA_MODEL_CONTEXT", "131072"))}
        key = env("TALARIA_MODEL_API_KEY")
        if key:
            m["api_key"] = key
        cfg["model"] = m
    if env("TALARIA_ROLLING"):
        merge(cfg, ROLLING)
    return cfg


# Zero-downtime rolling updates on Kubernetes (patched Hermes): SIGTERM opens a restart wait where running turns
# finish and approvals stay answerable, the new pod's gateway waits as a standby for this one's lock, and the
# Webapp listener is handed over and relays connections until the new gateway is ready. See docs/helm.md
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
    merge(config, talaria_config(pubkey))
    extra = env("TALARIA_EXTRA_CONFIG")
    if extra and os.path.exists(extra):
        # Your own Hermes settings (Helm value hermes.config), merged last
        with open(extra) as f:
            merge(config, safe_load(f) or {})
    with open(path + ".tmp", "w") as f:
        safe_dump(config, f, sort_keys=False)
    os.chmod(path + ".tmp", 0o600)
    os.replace(path + ".tmp", path)
    subprocess.run([HERMES, "plugins", "enable", "device"], check=False)
    setup_honcho()
    print("hermes-init: done")


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
