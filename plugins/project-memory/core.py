"""Shared core of project-memory: one curated MEMORY.md per project, for chats bound to a device folder.

A project is the git repository of the chat's bound folder (the device plugin's ``/device <device> <dir>``),
identified by its normalized origin URL (``github.com/alice/demo``), else the URL of the current branch's
upstream remote, else the URL of the only remote, else the repository's oldest root commit (``root:<sha>``). A
folder outside git is identified by device and folder (``folder:<device id>:<dir>``). A new identity with the
same root commit and repository name as a known project joins that project as an alias, so mirrors on two hosts
share one memory; a remote identity also joins a remote-less project with its root commit.

The identity is resolved on the device by a short git probe through the device plugin's hub, when the session
prompt is built and when the tool first runs in a binding; it is cached per binding generation in process and
per device folder in ``folders.json``, so the block and the tool still work while the device is offline. A
probe that takes longer than PROBE_WAIT_S finishes in the background and the cached answer is used meanwhile.

Storage: ``$HERMES_HOME/project-memories/<slug>/MEMORY.md`` (Hermes's own MemoryStore format and rules) and
``meta.json`` (identity, aliases, root commit, folders and devices seen). The gateway (tool, prompt section) and
the dashboard routes (hermes-hub's Memory page) run in one process and share this module.
"""
from __future__ import annotations

import concurrent.futures
import contextlib
import hashlib
import json
import logging
import os
import posixpath
import re
import shlex
import sys
import threading
import time
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("project_memory")

PLUGIN = "project-memory"
TOOL = "project_memory"
TOOLSET = "project-memory"
SECTION_ID = "project-memory"
DEVICE_CORE = "hermes_device_core"
DIR_NAME = "project-memories"
META = "meta.json"
FOLDERS = "folders.json"
MEMORY_FILE = "MEMORY.md"

KIND_REMOTE, KIND_ROOT, KIND_FOLDER = "remote", "root", "folder"
MIN_CHAR_LIMIT = 200
# A plugin section holds at most 4,000 chars; the note, the header and the frame take the rest.
MAX_CHAR_LIMIT = 3000
SECTION_BUDGET = 3990
MAX_LABEL = 200
MAX_ALIASES_SHOWN = 3
RESOLVE_TIMEOUT_S = 10
PROBE_WAIT_S = 6
MAX_FOLDERS = 50
MAX_FOLDER_INDEX = 2000
MAX_CACHE = 5000
SEEN_REFRESH_S = 3600
MAX_TEXT = 64 * 1024

SHA_RE = re.compile(r"[0-9a-f]{40}(?:[0-9a-f]{24})?")
SLUG_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9.~-]{0,159}")
_SLUG_BAD = re.compile(r"[^A-Za-z0-9.-]+")
_HOST_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9.-]*")
_URL_SCHEMES = frozenset({"ssh", "git+ssh", "ssh+git", "https", "http", "git"})

NOTE = ("This chat works in a project folder, so it has a project memory next to the global one. Save facts about "
        "this project (its code, rules, decisions, people involved, current state) with project_memory. Use memory "
        "only for facts about the user or rules that apply in every project. When unsure, use project_memory.")
CHILD_NOTE = ("This task works in a project folder. Its project memory is below; subagents can read it but not "
              "change it, so report new project facts in your result.")

MEMORY_TOOL_NOTE = ("\n\nPROJECTS: in a chat bound to a project folder, facts about that project (its code, rules, "
                    "decisions, people involved, state) go in project_memory, not here. Use memory only for facts "
                    "about the user or rules that apply in every project. When unsure, use project_memory.")

# Runs with /bin/sh on the device in the bound folder; prints only the lines the plugin parses. Remote URLs lose
# their credentials on the device, before they are sent.
PROBE = r"""export GIT_TERMINAL_PROMPT=0 GIT_OPTIONAL_LOCKS=0 LC_ALL=C
command -v git >/dev/null 2>&1 || { echo "@@pm error git is not installed on the device"; exit 0; }
g() { git -c safe.directory='*' "$@" 2>/dev/null; }
clean() { printf '%s\n' "$1" | sed -e '/:\/\//s#^\([^:/]*://\).*@#\1#' -e '/:\/\//!s#^.*@##'; }
if ! top=$(g rev-parse --show-toplevel); then
  d=$(pwd -P)
  while :; do
    if [ -e "$d/.git" ]; then echo "@@pm error git cannot read the repository"; exit 0; fi
    [ "$d" = / ] && break
    d=$(dirname "$d")
  done
  echo "@@pm nogit"
  exit 0
fi
printf '@@pm top %s\n' "$top"
u=$(g remote get-url origin) && printf '@@pm origin %s\n' "$(clean "$u")"
b=$(g symbolic-ref -q --short HEAD) && r=$(g config --get "branch.$b.remote") && [ "$r" != . ] && u=$(g remote get-url "$r") && printf '@@pm upstream %s\n' "$(clean "$u")"
set -- $(g remote)
[ $# -eq 1 ] && u=$(g remote get-url "$1") && printf '@@pm single %s\n' "$(clean "$u")"
if [ "$(g rev-parse --is-shallow-repository)" != true ]; then
  c=$(g rev-list --max-parents=0 --reverse HEAD | head -n 1) && [ -n "$c" ] && printf '@@pm root %s\n' "$c"
fi
exit 0
"""


def probe_command() -> str:
    """The probe as one command line that runs with /bin/sh whatever the device user's shell is."""
    return "exec /bin/sh -c " + shlex.quote(PROBE)


class Refused(Exception):
    """A refused change from the hub: an HTTP status, an error code and a message for the user."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status, self.code, self.message = status, code, message


# --- identity ------------------------------------------------------------------------------------

def normalize_remote(url: Any) -> Optional[str]:
    """``host/owner/repo`` of a git remote URL, lowercased, without scheme, credentials, port or ``.git``;
    None for a local path, a ``file:`` URL or anything that is not a remote. Credentials end at the last ``@``
    of the part before the path, even when they hold ``:`` or ``/``."""
    if not isinstance(url, str):
        return None
    url = url.strip()
    if not url or url.startswith(("/", ".", "~")) or any(c in url for c in "\n\r\t "):
        return None
    if "://" in url:
        scheme, _, rest = url.partition("://")
        if scheme.lower() not in _URL_SCHEMES:
            return None
        rest = rest.rsplit("@", 1)[-1]
        host, _, path = rest.partition("/")
        host = host.rsplit(":", 1)[0] if host.count(":") == 1 else host
    else:
        rest = url.rsplit("@", 1)[-1]
        host, sep, path = rest.partition(":")
        if not sep:
            return None
    host = host.strip().lower().rstrip(".")
    if not _HOST_RE.fullmatch(host):
        return None
    path = re.sub(r"/{2,}", "/", path.strip()).strip("/")
    if path.lower().endswith(".git"):
        path = path[:-4].rstrip("/")
    if not path or any(seg in ("", ".", "..") for seg in path.split("/")):
        return None
    return f"{host}/{path}".lower()


def parse_probe(out: str) -> dict[str, str]:
    """The probe's ``@@pm`` lines as a dict: ``nogit``, ``error``, or ``top`` and any of ``origin``,
    ``upstream``, ``single``, ``root``."""
    found: dict[str, str] = {}
    for line in (out or "").splitlines():
        if not line.startswith("@@pm "):
            continue
        key, _, value = line[5:].partition(" ")
        if key == "nogit":
            return {"nogit": "1"}
        if key == "error":
            return {"error": value.strip() or "git failed on the device"}
        if key in ("top", "origin", "upstream", "single", "root") and key not in found:
            found[key] = value.strip()
    return found


def identity_from_probe(probe: dict[str, str], device_id: str, directory: str) -> Optional[dict[str, str]]:
    """``{identity, kind, name, root}`` for a probed folder; ``{error}`` when git failed on the device; None
    when the probe said nothing usable."""
    if probe.get("error"):
        return {"error": probe["error"]}
    if probe.get("nogit"):
        return folder_identity(device_id, directory)
    top = probe.get("top") or ""
    if not top.startswith("/"):
        return None
    root = probe.get("root", "")
    root = root if SHA_RE.fullmatch(root) else ""
    for key in ("origin", "upstream", "single"):
        ident = normalize_remote(probe.get(key))
        if ident:
            return {"identity": ident, "kind": KIND_REMOTE, "name": ident.rsplit("/", 1)[-1], "root": root}
    if root:
        return {"identity": f"root:{root}", "kind": KIND_ROOT, "name": posixpath.basename(top.rstrip("/")) or top,
                "root": root}
    return folder_identity(device_id, top)


def folder_identity(device_id: str, directory: str) -> dict[str, str]:
    directory = posixpath.normpath(directory)
    return {"identity": f"folder:{device_id}:{directory}", "kind": KIND_FOLDER,
            "name": posixpath.basename(directory) or directory, "root": ""}


def _hash8(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8", "surrogatepass")).hexdigest()[:8]


def slug_for(identity: str) -> str:
    """A readable, path-safe directory name for an identity: ``github.com-alice-demo``, ``root-<sha>``,
    ``<device>-home-alice-demo``. Long ones are cut and get a hash suffix. Always matches SLUG_RE."""
    if identity.startswith("root:"):
        base = "root-" + identity[5:]
    elif identity.startswith("folder:"):
        device_id, _, directory = identity[7:].partition(":")
        base = _SLUG_BAD.sub("-", device_id) + "-" + _SLUG_BAD.sub("-", directory)
    else:
        base = _SLUG_BAD.sub("-", identity)
    base = re.sub(r"\.{2,}", ".", re.sub(r"-{2,}", "-", base)).strip("-.") or "project"
    if len(base) > 120:
        base = base[:120].rstrip("-.") + "~" + _hash8(identity)
    return base


# --- storage -------------------------------------------------------------------------------------

_LOCK = threading.RLock()


def base_dir() -> Path:
    from hermes_constants import get_hermes_home
    return Path(get_hermes_home()) / DIR_NAME


def _mkdir(path: Path) -> None:
    try:
        from hermes_constants import mkdir_under_hermes_home
        mkdir_under_hermes_home(path)
    except ImportError:
        path.mkdir(parents=True, exist_ok=True)


@contextlib.contextmanager
def _locked():
    """The process lock and a file lock on ``.lock``, for meta.json and folders.json."""
    with _LOCK:
        base = base_dir()
        _mkdir(base)
        try:
            import fcntl
        except ImportError:
            yield
            return
        fd = os.open(base / ".lock", os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            os.close(fd)


def _read_json(path: Path) -> Optional[dict]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) else None


def _write_json(path: Path, doc: dict) -> None:
    _mkdir(path.parent)
    tmp = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, indent=2, sort_keys=True) + "\n")
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def valid_slug(slug: Any) -> bool:
    return isinstance(slug, str) and bool(SLUG_RE.fullmatch(slug)) and ".." not in slug


def read_meta(slug: str) -> Optional[dict]:
    if not valid_slug(slug):
        return None
    meta = _read_json(base_dir() / slug / META)
    return meta if meta and isinstance(meta.get("identity"), str) else None


def _all_metas() -> dict[str, dict]:
    base = base_dir()
    out: dict[str, dict] = {}
    try:
        names = sorted(p.name for p in base.iterdir() if p.is_dir())
    except OSError:
        return out
    for name in names:
        meta = read_meta(name)
        if meta is not None:
            out[name] = meta
    return out


def _identities(meta: dict) -> list[str]:
    return [meta["identity"], *[a for a in meta.get("aliases") or [] if isinstance(a, str)]]


def _same_repo(ident: dict[str, str], meta: dict) -> bool:
    """A mirror: the same root commit and repository name. A remote-less project's name is its folder's, so a
    project or identity of kind root needs only the root commit."""
    if meta.get("kind") == KIND_FOLDER or meta.get("root_commit") != ident.get("root"):
        return False
    if KIND_ROOT in (ident["kind"], meta.get("kind")):
        return True
    return str(meta.get("name") or "").lower() == ident.get("name", "").lower()


def attach(ident: dict[str, str]) -> str:
    """The slug of the project an identity belongs to, created when new. A remote or root identity of the same
    repository as a known project (``_same_repo``) joins it as an alias. Call with the lock."""
    identity, root = ident["identity"], ident.get("root", "")
    metas = _all_metas()
    for slug, meta in metas.items():
        if identity in _identities(meta):
            if root and not meta.get("root_commit") and meta.get("kind") != KIND_FOLDER:
                meta["root_commit"] = root
                _write_json(base_dir() / slug / META, meta)
            return slug
    if ident["kind"] != KIND_FOLDER and root:
        same = [slug for slug, meta in metas.items() if _same_repo(ident, meta)]
        named = [s for s in same if str(metas[s].get("name") or "").lower() == ident.get("name", "").lower()]
        pick = named[0] if named else (same[0] if len(same) == 1 else None)
        if pick is not None:
            meta = metas[pick]
            meta["aliases"] = [*(meta.get("aliases") or []), identity]
            _write_json(base_dir() / pick / META, meta)
            logger.info("project-memory: %s joined project %s as an alias", identity, pick)
            return pick
    slug = slug_for(identity)
    if slug in metas or (base_dir() / slug).exists():
        slug = slug[:150].rstrip("-.") + "~" + _hash8(identity)
    meta = {"version": 1, "identity": identity, "kind": ident["kind"], "name": ident.get("name", ""),
            "aliases": [], "root_commit": root, "folders": [], "created_at": round(time.time(), 3)}
    _write_json(base_dir() / slug / META, meta)
    logger.info("project-memory: new project %s", slug)
    return slug


def _folder_key(device_id: str, directory: str) -> str:
    return f"{device_id}\n{directory}"


def _note_folder(slug: str, device_id: str, device: str, directory: str) -> None:
    """Record the folder in the project's meta and in folders.json. Call with the lock."""
    now = round(time.time(), 3)
    path = base_dir() / slug / META
    meta = _read_json(path) or {}
    folders = [f for f in meta.get("folders") or [] if isinstance(f, dict)]
    old = next((f for f in folders if f.get("device_id") == device_id and f.get("dir") == directory), None)
    if old is None or old.get("device") != device or now - float(old.get("seen_at") or 0) > SEEN_REFRESH_S:
        folders = [f for f in folders if f is not old]
        folders.insert(0, {"device_id": device_id, "device": device, "dir": directory, "seen_at": now})
        meta["folders"] = folders[:MAX_FOLDERS]
        _write_json(path, meta)
    index_path = base_dir() / FOLDERS
    index = _read_json(index_path) or {}
    entries = index.get("folders") if isinstance(index.get("folders"), dict) else {}
    key = _folder_key(device_id, directory)
    if (entries.get(key) or {}).get("project") != slug:
        entries[key] = {"project": slug, "resolved_at": now}
        if len(entries) > MAX_FOLDER_INDEX:
            keep = sorted(entries, key=lambda k: float((entries[k] or {}).get("resolved_at") or 0))
            entries = {k: entries[k] for k in keep[-MAX_FOLDER_INDEX:]}
        _write_json(index_path, {"version": 1, "folders": entries})


def cached_folder(device_id: str, directory: str) -> Optional[str]:
    index = _read_json(base_dir() / FOLDERS) or {}
    entry = (index.get("folders") or {}).get(_folder_key(device_id, directory)) or {}
    slug = entry.get("project")
    return slug if valid_slug(slug) and read_meta(slug) is not None else None


# --- binding lookup --------------------------------------------------------------------------------

_SESSION_CACHE: dict[tuple[str, str], str] = {}
_CHILDREN: set[str] = set()
_POOL = concurrent.futures.ThreadPoolExecutor(max_workers=4, thread_name_prefix="project-memory-probe")
_INFLIGHT: dict[tuple[str, str], concurrent.futures.Future] = {}
_INFLIGHT_LOCK = threading.Lock()


def _remember(cache: Any, key: Any, value: Any = None) -> None:
    if len(cache) >= MAX_CACHE:
        cache.clear()
    if isinstance(cache, set):
        cache.add(key)
    else:
        cache[key] = value


def device_core():
    return sys.modules.get(DEVICE_CORE)


def binding_of(session_id: str):
    """``(binding, why)``: the explicit device binding of the session's lineage (the chat itself, or the chat a
    subagent or a compressed segment came from), or None and why there is none. The implicit sandbox binding of
    an unbound chat is not a project."""
    core = device_core()
    if core is None:
        return None, "the device plugin is not loaded, so no chat has a project folder"
    if not session_id:
        return None, "this call has no session"
    res = core.hub().resolve(session_id)
    if res.state != core.STATE_BOUND or res.binding is None or getattr(res, "sandbox", False):
        return None, ("this chat is not bound to a project folder (bind it with /device <device> <dir>), so it has "
                      "no project memory; use memory for facts that apply everywhere")
    return res.binding, ""


def _device_name(core, device_id: str) -> str:
    conn = core.hub().device(device_id)
    return str(getattr(conn, "name", "") or device_id) if conn is not None else device_id


def _probe(device_id: str, directory: str) -> tuple[Optional[str], str]:
    """Run the git probe and record the answer: ``(slug, "")``, or None and why it could not."""
    core = device_core()
    if core is None or core.hub().device(device_id) is None:
        return None, "offline"
    try:
        code, out, _proc = core.hub().run(device_id, probe_command(), directory, timeout=RESOLVE_TIMEOUT_S)
    except Exception as exc:
        logger.warning("project-memory: the git probe on device %s failed: %s", device_id, type(exc).__name__)
        return None, "offline"
    if code != 0:
        logger.info("project-memory: the git probe on device %s exited %s", device_id, code)
        return None, "offline"
    ident = identity_from_probe(parse_probe(out), device_id, directory)
    if ident is None:
        return None, "offline"
    if ident.get("error"):
        return None, ident["error"]
    with _locked():
        slug = attach(ident)
        _note_folder(slug, device_id, _device_name(core, device_id), directory)
    return slug, ""


def resolve_live(device_id: str, directory: str, wait: float = PROBE_WAIT_S) -> tuple[Optional[str], str]:
    """The probe for one device folder, waited for at most ``wait`` seconds. A slower probe keeps running and
    records its answer for the next lookup; one probe per folder runs at a time."""
    key = (device_id, directory)
    with _INFLIGHT_LOCK:
        fut = _INFLIGHT.get(key)
        if fut is None or fut.done():
            fut = _POOL.submit(_probe, device_id, directory)
            _INFLIGHT[key] = fut
    try:
        return fut.result(timeout=wait)
    except concurrent.futures.TimeoutError:
        return None, "offline"
    except Exception as exc:
        logger.warning("project-memory: resolving %s on device %s failed: %s", directory, device_id,
                       type(exc).__name__)
        return None, "offline"


def project_for_session(session_id: str, *, live: bool) -> tuple[Optional[str], str]:
    """``(slug, why)`` of the session's project. ``live`` probes the device even when this binding generation
    was resolved before (a prompt build); otherwise the cached answer is used. Falls back to the last answer
    for the folder when the device is offline or slow."""
    binding, why = binding_of(session_id)
    if binding is None:
        return None, why
    key = (binding.session_id, binding.generation)
    if not live and key in _SESSION_CACHE and read_meta(_SESSION_CACHE[key]) is not None:
        return _SESSION_CACHE[key], ""
    slug, failed = resolve_live(binding.device_id, binding.dir)
    if slug is None:
        slug = _SESSION_CACHE.get(key) or cached_folder(binding.device_id, binding.dir)
    if slug is None:
        if failed and failed != "offline":
            return None, (f"{failed} ({binding.dir} on device {binding.device_id}), so this folder's project is not "
                          "known and project memory is not available now. Nothing was read or written.")
        return None, (f"device {binding.device_id} is offline or did not answer, and the project of {binding.dir} is "
                      "not known yet, so project memory is not available now. Nothing was read or written; try "
                      "again when the device is connected.")
    _remember(_SESSION_CACHE, key, slug)
    return slug, ""


# --- the store --------------------------------------------------------------------------------------

_STORE_CLASS = None
_STORES: dict[tuple[str, str], Any] = {}


def _store_class():
    global _STORE_CLASS
    if _STORE_CLASS is None:
        from tools.memory_tool_store import MemoryStore

        class ProjectStore(MemoryStore):
            """Hermes's MemoryStore on one project's MEMORY.md (target ``memory`` only)."""

            def __init__(self, path: Path, limit: int):
                super().__init__(memory_char_limit=limit, user_char_limit=0, memory_enabled=True,
                                 user_profile_enabled=False)
                self._project_path = path

            def _path_for(self, target: str) -> Path:
                return self._project_path

        _STORE_CLASS = ProjectStore
    return _STORE_CLASS


def session_store(session_id: str, slug: str):
    """The session's store for a project, kept so Hermes's per-turn cap on failed consolidation holds."""
    limit = char_limit()
    st = _STORES.get((session_id, slug))
    if st is None or st.memory_char_limit != limit:
        st = store(slug, limit)
        _remember(_STORES, (session_id, slug), st)
    return st


def on_turn_start(session_id: str = "", **_kwargs) -> None:
    for (sid, _slug), st in list(_STORES.items()):
        if sid == session_id:
            st.reset_consolidation_failures()

def _config() -> dict:
    try:
        from hermes_cli.config import load_config_readonly
        cfg = load_config_readonly()
    except Exception:
        return {}
    return cfg if isinstance(cfg, dict) else {}


def char_limit(cfg: Optional[dict] = None) -> int:
    """The plugin's ``char_limit`` setting, else the global memory_char_limit, at most MAX_CHAR_LIMIT."""
    cfg = _config() if cfg is None else cfg
    settings = (((cfg.get("plugins") or {}).get("entries") or {}).get(PLUGIN) or {}).get("settings") or {}
    value = settings.get("char_limit") if isinstance(settings, dict) else None
    if not isinstance(value, int) or isinstance(value, bool):
        mem = cfg.get("memory") if isinstance(cfg.get("memory"), dict) else {}
        value = mem.get("memory_char_limit", 2200)
        value = value if isinstance(value, int) and not isinstance(value, bool) else 2200
    return max(MIN_CHAR_LIMIT, min(value, MAX_CHAR_LIMIT))


def memory_path(slug: str) -> Path:
    return base_dir() / slug / MEMORY_FILE


def store(slug: str, limit: Optional[int] = None):
    return _store_class()(memory_path(slug), char_limit() if limit is None else limit)


def entries(slug: str) -> list[str]:
    from tools.memory_tool_store import MemoryStore
    return list(dict.fromkeys(MemoryStore._read_file(memory_path(slug))))


def _delim() -> str:
    from tools.memory_tool_store import ENTRY_DELIMITER
    return ENTRY_DELIMITER


def _label(meta: dict) -> str:
    ident = meta.get("identity", "")
    if meta.get("kind") == KIND_ROOT:
        ident = f"{meta.get('name') or 'repository'} (git repository without a remote, root commit {ident[5:17]})"
    elif meta.get("kind") == KIND_FOLDER:
        ident = ident[7:].replace(":", " folder ", 1)
    aliases = [a for a in meta.get("aliases") or [] if isinstance(a, str)]
    shown = ", ".join(aliases[:MAX_ALIASES_SHOWN])
    if len(aliases) > MAX_ALIASES_SHOWN:
        shown += f" and {len(aliases) - MAX_ALIASES_SHOWN} more"
    label = ident + (f", also {shown}" if aliases else "")
    return label if len(label) <= MAX_LABEL else label[:MAX_LABEL - 3] + "..."


def _sanitize(entry: str) -> str:
    try:
        from tools.threat_patterns import scan_for_threats
        findings = scan_for_threats(entry, scope="strict") if entry and not entry.startswith("[BLOCKED:") else None
    except Exception:
        findings = ["the threat scan failed"]
    if not findings:
        return entry
    return (f"[BLOCKED: project memory entry contained threat pattern(s): {', '.join(findings)}. Removed from the "
            f"system prompt; use project_memory remove to delete the original.]")


def render_block(slug: str, limit: Optional[int] = None, note: str = NOTE) -> str:
    """The project block for the system prompt: the note, a header with the project and usage, and the entries
    that fit the section budget, in order; an entry that does not fit is left out and counted."""
    meta = read_meta(slug)
    if meta is None:
        return ""
    limit = char_limit() if limit is None else limit
    raw = entries(slug)
    items = [_sanitize(e) for e in raw]
    delim = _delim()
    used = len(delim.join(raw))
    sep = "═" * 46
    pct = min(100, int(used * 100 / limit)) if limit > 0 else 0
    head = f"{note}\n\n{sep}\nPROJECT MEMORY ({_label(meta)}) [{pct}%, {used:,}/{limit:,} chars]\n{sep}\n"
    if not items:
        return head + "(no project notes yet)"
    if len(head) + len(delim.join(items)) <= SECTION_BUDGET:
        return head + delim.join(items)
    marker = ("\n[{n} more project notes are not shown: they do not fit in the prompt. Read them with "
              "project_memory action read, then shorten or remove some.]")
    room = SECTION_BUDGET - len(head) - len(marker) - 8
    shown: list[str] = []
    for item in items:
        if len(delim.join([*shown, item])) <= room:
            shown.append(item)
    return head + delim.join(shown) + marker.format(n=len(items) - len(shown))


def section(info: Any) -> str:
    """The plugin system prompt section: rendered once per session and at each prompt rebuild."""
    session_id = str((info or {}).get("session_id") or "")
    if not offered():
        return ""
    try:
        slug, _why = project_for_session(session_id, live=True)
        if not slug:
            return ""
        child = children_blocked() and is_child(session_id)
    except Exception as exc:
        logger.warning("project-memory: could not find the project of session %s: %s", session_id,
                       type(exc).__name__)
        return ""
    return render_block(slug, note=CHILD_NOTE if child else NOTE)


# --- the tool ---------------------------------------------------------------------------------------

def _tool_error(message: str, **extra) -> str:
    return json.dumps({"success": False, "error": message, **extra}, ensure_ascii=False)


def children_blocked() -> bool:
    """Subagents may not write project memory while Hermes keeps them from writing the global memory."""
    try:
        from tools.delegate_tool_toolsets import DELEGATE_BLOCKED_TOOLS
    except Exception:
        return True
    return "memory" in DELEGATE_BLOCKED_TOOLS


def note_child(child_session_id: Any = None, **_kwargs) -> None:
    if isinstance(child_session_id, str) and child_session_id:
        _remember(_CHILDREN, child_session_id)


def is_child(session_id: str) -> bool:
    """A delegate subagent's session (or a compressed segment of one, which keeps its source). True when the
    session store cannot tell, so a write is refused rather than allowed."""
    if session_id in _CHILDREN:
        return True
    try:
        from hermes_cli.goals import _get_session_db
        db = _get_session_db()
        row = db.get_session(session_id) if db is not None else None
    except Exception:
        return True
    return isinstance(row, dict) and row.get("source") == "subagent"


def _unattended_review() -> bool:
    try:
        from tools.skill_provenance import is_unattended_review
        return bool(is_unattended_review())
    except Exception:
        return False


def _approval_setting(cfg: Optional[dict] = None) -> Optional[bool]:
    """The plugin's ``write_approval`` setting: True, False, or None when unset (or not a bool)."""
    cfg = _config() if cfg is None else cfg
    plugins = cfg.get("plugins") if isinstance(cfg.get("plugins"), dict) else {}
    entry = (plugins.get("entries") or {}).get(PLUGIN) if isinstance(plugins.get("entries"), dict) else None
    settings = entry.get("settings") if isinstance(entry, dict) else None
    value = settings.get("write_approval") if isinstance(settings, dict) else None
    return value if isinstance(value, bool) else None


def _approval_on() -> bool:
    """Whether a new project write waits for approval: the plugin's ``write_approval`` when set, else Hermes's
    ``memory.write_approval``. Writes already staged stay staged either way."""
    setting = _approval_setting()
    if setting is False:
        return False
    try:
        from tools import write_approval as wa
        if setting is True:
            return True
        return bool(wa.write_approval_enabled(wa.MEMORY))
    except Exception:
        return False


WRITE_ACTIONS = ("add", "patch", "replace", "remove")
DESTRUCTIVE = ("patch", "replace", "remove")
# The target of a staged project write in Hermes's memory approval queue. Hermes's own replay refuses it, so a
# process without this plugin can never apply a project write to the global MEMORY.md.
PENDING_TARGET = "project"


def _op_line(op: dict) -> str:
    act, content, old = op.get("action", "?"), op.get("content") or "", op.get("old_text") or ""
    if act == "remove":
        return f"- remove: {old}"
    if act == "patch":
        return f"- patch span '{old}' -> '{content}' (rest of the entry is kept)"
    if act == "replace":
        return f"- replace entry matching '{old}' with: {content}"
    return f"- {act}: {content}"


def _stage(st, slug: str, project: str, operations: list, *, summary: str, message: str,
           origin: Optional[str] = None) -> dict:
    """Stage a batch in Hermes's memory approval queue (``/memory pending`` and the hub's approval controls),
    pinned to the entries it selects now, as Hermes pins its own staged writes."""
    from tools import write_approval as wa
    pinned = st.resolve_batch_entries("memory", operations)
    if not pinned.get("success"):
        return pinned
    ops = [op if entry is None else {**op, "matched_entry": entry}
           for op, entry in zip(operations, pinned["matched_entries"])]
    payload = {"action": "batch", "target": PENDING_TARGET, "project": slug, "project_identity": project,
               "operations": ops}
    record = wa.stage_write(wa.MEMORY, payload, summary=summary[:200], origin=origin or wa.current_origin())
    return {"success": True, "staged": True, "pending_id": record["id"], "project": project, "message": message}


def _gate(st, slug: str, project: str, operations: list) -> Optional[dict]:
    """The same rules as Hermes's memory tool: unattended background review stages patch, replace and remove;
    with memory.write_approval on, a write is approved inline, staged for approval, or refused when the user
    denies it. None lets the write go ahead."""
    detail = "\n".join(_op_line(op) for op in operations)
    label = f"project memory {project}"
    if _unattended_review() and any(op.get("action") in DESTRUCTIVE for op in operations):
        try:
            return _stage(st, slug, project, operations,
                          summary=f"background review consolidation (batch on {label}): {detail}",
                          message=("Background review may not change or delete project memory entries unattended. "
                                   "The proposed batch was staged for the user's approval (/memory pending)."),
                          origin="background_review")
        except Exception:
            logger.warning("project-memory: could not stage a background-review batch; denying", exc_info=True)
            return {"success": False, "error": "Background review may not change or delete project memory "
                                               "entries; add is still available."}
    if not _approval_on():
        return None
    from tools import write_approval as wa
    summary = f"apply {len(operations)} op(s) to {label}"
    decision = wa.evaluate_gate(wa.MEMORY, inline_summary=summary, inline_detail=detail)
    if decision.allow:
        return None
    if decision.blocked:
        return {"success": False, "error": decision.message, "project": project}
    return _stage(st, slug, project, operations, summary=f"{summary}: {detail[:120]}", message=decision.message)


def apply_pending(payload: dict, original) -> dict:
    """Replay a staged project write on its project's MEMORY.md with Hermes's own replay rules."""
    slug = payload.get("project")
    if not valid_slug(slug) or read_meta(slug) is None:
        return {"success": False, "error": f"The project of this pending write ({payload.get('project_identity') or slug}) "
                                           "no longer exists; nothing was applied. Reject it."}
    return original({**payload, "target": "memory"}, store(slug))


def install_pending_replay() -> None:
    """Route staged project writes in Hermes's memory approval replay to their project, once."""
    try:
        from tools import memory_tool
    except Exception:
        return
    original = memory_tool.apply_memory_pending
    if getattr(original, "_project_memory", False):
        return

    def apply_memory_pending(payload, store):
        if isinstance(payload, dict) and payload.get("target") == PENDING_TARGET:
            return apply_pending(payload, original)
        return original(payload, store)

    apply_memory_pending._project_memory = True
    apply_memory_pending.__doc__ = original.__doc__
    memory_tool.apply_memory_pending = apply_memory_pending


def handle(args: Any, **kwargs) -> str:
    """The project_memory tool."""
    args = args if isinstance(args, dict) else {}
    session_id = kwargs.get("session_id") or ""
    if not session_id:
        core = device_core()
        session_id = core.current_session_id() if core is not None else ""
    action = args.get("action") or ("write" if args.get("operations") else "read")
    if action not in ("read", "write"):
        return _tool_error(f"Unknown action {action!r}. Use read, or write with operations.")
    operations = args.get("operations")
    if action == "write":
        if not isinstance(operations, list) or not operations or not all(isinstance(o, dict) for o in operations):
            return _tool_error("write needs operations: a list of {action: add/patch/replace/remove, content?, "
                               "old_text?}; a single change is a one-item list.")
        if any(o.get("action") not in WRITE_ACTIONS for o in operations):
            return _tool_error("Each operation's action must be add, patch, replace or remove.")
        if children_blocked() and is_child(session_id):
            return _tool_error("Subagents may not write project memory, as they may not write memory. Report the "
                               "fact in your result; the parent agent can save it.")
    slug, why = project_for_session(session_id, live=False)
    if slug is None:
        return _tool_error(why)
    meta = read_meta(slug) or {}
    project = meta.get("identity", slug)
    st = session_store(session_id, slug)
    if action == "read":
        items = entries(slug)
        used = len(_delim().join(items))
        return json.dumps({"success": True, "project": project, "aliases": meta.get("aliases") or [],
                           "entries": items, "usage": f"{used:,}/{st.memory_char_limit:,} chars"}, ensure_ascii=False)
    try:
        from tools.memory_tool_store import FAILURE_CLASS
        token = FAILURE_CLASS.set("other")
    except Exception:
        FAILURE_CLASS, token = None, None
    try:
        held = _gate(st, slug, project, operations)
        if held is not None:
            return json.dumps(held, ensure_ascii=False)
        result = st.apply_batch("memory", operations)
    finally:
        if FAILURE_CLASS is not None:
            FAILURE_CLASS.reset(token)
    result = dict(result)
    result.pop("target", None)
    result["project"] = project
    return json.dumps(result, ensure_ascii=False)


SCHEMA = {
    "name": TOOL,
    "description": (
        "Notes for the project this chat works in: the git repository of the chat's bound device folder (every "
        "clone, worktree and mirror of it shares one memory). Its notes are injected into the system prompt of "
        "every chat in this project, next to the global memory, so keep entries compact and high-signal.\n\n"
        "PREFER THIS over memory for anything about the project: its code, rules, conventions, decisions, people "
        "involved, current state. Use memory only for facts about the user or rules that apply in every project. "
        "When unsure, use project_memory.\n\n"
        "HOW: action 'write' with an 'operations' array (each item: {action: add/patch/replace/remove, content?, "
        "old_text?}), applied atomically against the project's char limit, so one call can remove stale entries "
        "and add new ones. 'patch' replaces the old_text span inside one entry; 'replace' rewrites the whole entry "
        "that old_text matches; 'remove' deletes it. action 'read' lists the current entries.\n\n"
        "Works only in chats bound to a project folder (/device <device> <dir>); elsewhere it refuses. "
        "SKIP: trivial or easily re-discovered facts, task progress, temporary TODO state."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "action": {"type": "string", "enum": ["read", "write"],
                       "description": "'read' lists the project's notes; 'write' applies operations."},
            "operations": {
                "type": "array",
                "description": "For 'write': changes applied atomically in one call; a single change is a one-item list.",
                "items": {
                    "type": "object",
                    "properties": {
                        "action": {"type": "string", "enum": list(WRITE_ACTIONS)},
                        "content": {"type": "string",
                                    "description": "For add, the new entry; for replace, the whole new entry; for "
                                                   "patch, the text that replaces old_text."},
                        "old_text": {"type": "string",
                                     "description": "For patch, replace and remove: text from one entry."},
                    },
                    "required": ["action"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["action"],
    },
}


def offered() -> bool:
    """check_fn: the built-in memory store is on and the device plugin is loaded."""
    if device_core() is None:
        return False
    try:
        from tools.memory_tool import get_builtin_memory_store_flags
        return bool(get_builtin_memory_store_flags()[0])
    except Exception:
        return True


def extend_memory_schema() -> None:
    """Add the project rule to the built-in memory tool's description, once."""
    try:
        from tools import memory_tool
    except Exception:
        return
    desc = memory_tool.MEMORY_SCHEMA.get("description", "")
    if MEMORY_TOOL_NOTE.strip() not in desc:
        memory_tool.MEMORY_SCHEMA["description"] = desc + MEMORY_TOOL_NOTE


# --- hub routes ---------------------------------------------------------------------------------------

def project_json(slug: str, meta: dict, limit: int) -> dict:
    items = entries(slug)
    folders = []
    for f in meta.get("folders") or []:
        if isinstance(f, dict) and isinstance(f.get("dir"), str):
            folders.append({"device_id": str(f.get("device_id") or ""),
                            "device": str(f.get("device") or f.get("device_id") or ""),
                            "dir": f["dir"], "seen_at": float(f.get("seen_at") or 0)})
    root = meta.get("root_commit")
    return {"id": slug, "identity": meta["identity"], "kind": meta.get("kind") or KIND_REMOTE,
            "name": str(meta.get("name") or slug),
            "aliases": [a for a in meta.get("aliases") or [] if isinstance(a, str)],
            "root_commit": root if isinstance(root, str) and SHA_RE.fullmatch(root) else "",
            "folders": folders, "count": len(items), "used": len(_delim().join(items)), "limit": limit}


def list_projects() -> dict:
    limit = char_limit()
    projects = [project_json(slug, meta, limit) for slug, meta in _all_metas().items()]
    projects.sort(key=lambda p: (p["name"].lower(), p["identity"]))
    return {"projects": projects}


def _meta_or_404(slug: str) -> dict:
    meta = read_meta(slug)
    if meta is None:
        raise Refused(404, "unknown_project", "There is no such project memory.")
    return meta


def notes(slug: str) -> dict:
    """One project's notes in the shape of the device plugin's GET /memory, plus ``project``."""
    meta = _meta_or_404(slug)
    limit = char_limit()
    items = entries(slug)
    return {"approval": False, "pending": [], "project": project_json(slug, meta, limit),
            "targets": {"memory": {"file": str(memory_path(slug)), "limit": limit,
                                   "used": len(_delim().join(items)), "enabled": True, "entries": items},
                        "user": {"file": "", "limit": 0, "used": 0, "enabled": False, "entries": []}}}


def _check_text(*texts: Any) -> None:
    for t in texts:
        if not isinstance(t, str) or not t.strip():
            raise Refused(400, "bad_request", "the text must not be empty")
        if len(t) > MAX_TEXT:
            raise Refused(413, "too_large", "the text is too long")


def _failure(failure: str, limit: int, error: str) -> tuple[int, str, str]:
    if failure in {"over_budget", "retry_cap"}:
        return 409, "over_limit", (f"That would put this project's notes over their limit of {limit:,} characters. "
                                   "Shorten or delete a note first.")
    if failure in {"stale_entry", "no_match", "ambiguous"}:
        return 409, "changed", "The project's notes changed since you opened them. Reload and try again."
    if failure == "scan_blocked":
        return 422, "blocked", f"Hermes refused this text: {error}"
    if failure == "would_empty":
        return 409, "would_empty", "The notes cannot be emptied in one edit. Delete the last notes one at a time."
    return 409, failure or "refused", error or "Hermes did not accept that change"


def _apply(slug: str, call) -> dict:
    from tools.memory_tool_store import FAILURE_CLASS
    st = store(slug)
    token = FAILURE_CLASS.set("other")
    try:
        result = call(st)
        failure = FAILURE_CLASS.get()
    finally:
        FAILURE_CLASS.reset(token)
    if not result.get("success"):
        raise Refused(*_failure(failure, st.memory_char_limit, str(result.get("error") or "")))
    return notes(slug)


def add(slug: str, content: Any) -> dict:
    _meta_or_404(slug)
    _check_text(content)
    return _apply(slug, lambda st: st.add("memory", content))


def edit(slug: str, entry: Any, content: Any) -> dict:
    _meta_or_404(slug)
    _check_text(entry, content)
    if content.strip() == entry.strip():
        return notes(slug)
    return _apply(slug, lambda st: st.replace("memory", entry, content, entry))


def remove(slug: str, entry: Any) -> dict:
    _meta_or_404(slug)
    _check_text(entry)
    return _apply(slug, lambda st: st.remove("memory", entry, entry))


def parse_text(text: str) -> list[str]:
    """Entries of a whole-file edit: lines that hold only § separate entries."""
    out, cur = [], []
    for line in text.replace("\r\n", "\n").split("\n"):
        if line.strip() == "§":
            out.append("\n".join(cur).strip())
            cur = []
        else:
            cur.append(line)
    out.append("\n".join(cur).strip())
    return list(dict.fromkeys(e for e in out if e))


def replace_all(slug: str, text: Any, base: Any) -> dict:
    """A whole-file edit as one all-or-nothing batch, with the device plugin's rules: entries of ``base`` that
    the text no longer has are removed (pinned), new ones are added, and a meanwhile change fails as changed."""
    _meta_or_404(slug)
    if not isinstance(text, str) or len(text) > MAX_TEXT:
        raise Refused(413 if isinstance(text, str) else 400, "bad_request", "the text is too long or missing")
    if not isinstance(base, list) or not all(isinstance(e, str) for e in base) or len(base) > 10000:
        raise Refused(400, "bad_request", 'the body must have "base", the list of entries the editor showed')
    current, new = entries(slug), parse_text(text)
    base = [e.strip() for e in base if e.strip()]
    gone = [e for e in base if e not in new]
    ops: list[dict[str, Any]] = [{"action": "remove", "old_text": e, "matched_entry": e} for e in gone]
    ops += [{"action": "add", "content": e} for e in new if e not in base]
    if not ops:
        return notes(slug)
    if current and not new and set(current) <= set(gone):
        raise Refused(*_failure("would_empty", char_limit(), ""))
    return _apply(slug, lambda st: st.apply_batch("memory", ops))


def pending() -> dict:
    """The staged project writes in Hermes's memory approval queue, with their project, for the hub to show next
    to the global pending writes."""
    from tools import write_approval as wa
    out = []
    for rec in wa.list_pending(wa.MEMORY):
        payload = rec.get("payload") if isinstance(rec.get("payload"), dict) else {}
        if payload.get("target") != PENDING_TARGET:
            continue
        slug = payload.get("project")
        meta = read_meta(slug) if valid_slug(slug) else None
        out.append({"id": str(rec.get("id") or ""), "project": slug if isinstance(slug, str) else "",
                    "identity": (meta or {}).get("identity") or str(payload.get("project_identity") or slug or "")})
    return {"pending": out}
