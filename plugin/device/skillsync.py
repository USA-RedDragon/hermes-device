"""Mirror every skill_manage change into a git repository on a device, so skills have history.

After a successful ``skill_manage`` call (create, patch, write_file, remove_file, delete; single or
batch) the changed skill is copied into the repository and committed there, by a script that runs on
the device through the same exec leg as every other device command. The device's own git identity
and signing settings apply. Off unless ``plugins.entries.device.settings`` names both
``skill_repo_device`` (a device id) and ``skill_repo_path`` (an absolute path of a git checkout there).

A skill save never waits on this and never fails because of it: the hook only appends to a queue
file (``$HERMES_HOME/device/skill-sync.json``), and a worker thread drains it. A device that is
offline, a busy repository or a failed commit leave the entry queued; the worker retries until it
lands, also after a restart. Each entry is sent as the skill's state at send time, so a late retry
mirrors the newest state, and repeated entries for one skill merge into one commit.
"""
from __future__ import annotations

import base64
import io
import json
import logging
import os
import re
import sys
import tarfile
import threading
import time
from pathlib import Path
from typing import Any, Callable, Optional

logger = logging.getLogger("hermes_device")

FILE_NAME = "skill-sync.json"
ACTIONS = ("create", "patch", "edit", "write_file", "remove_file", "delete")
MAX_PAYLOAD_BYTES = 4 * 1024 * 1024
RETRY_SECONDS = 30.0
RUN_TIMEOUT_S = 60.0
ORIGIN = "made by agents in Hermes"
_NAME = re.compile(r"[a-z0-9][a-z0-9._-]*")

_lock = threading.RLock()
_worker: Optional[threading.Thread] = None
_wake = threading.Event()
# Tests replace these two seams; the worker sleeps and sends through them.
_sleep: Callable[[float], bool] = lambda seconds: _wake.wait(seconds)


def _core():
    return sys.modules["hermes_device_core"]


def config() -> Optional[tuple[str, str]]:
    """``(device id, repository path)`` when mirroring is configured, else None."""
    cfg = _core().settings()
    device, path = cfg.get("skill_repo_device"), cfg.get("skill_repo_path")
    if isinstance(device, str) and device.strip() and isinstance(path, str) and path.strip().startswith("/"):
        return device.strip(), path.strip()
    return None


# --- queue ------------------------------------------------------------------------------------

def _queue_path() -> Path:
    from hermes_constants import get_hermes_home
    return Path(get_hermes_home()) / "device" / FILE_NAME


def _load() -> list[dict[str, Any]]:
    try:
        data = json.loads(_queue_path().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    entries = data.get("entries") if isinstance(data, dict) else None
    return [e for e in entries or [] if isinstance(e, dict) and e.get("action") in ACTIONS
            and _NAME.fullmatch(str(e.get("name") or ""))]


def _save(entries: list[dict[str, Any]]) -> None:
    path = _queue_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(json.dumps({"version": 1, "entries": entries}), encoding="utf-8")
    os.replace(tmp, path)


def pending() -> list[dict[str, Any]]:
    with _lock:
        return _load()


def enqueue(action: str, name: str) -> None:
    """Add one change to the queue and wake the worker. Never raises."""
    try:
        name = Path(str(name)).name
        if action not in ACTIONS or not _NAME.fullmatch(name):
            return
        with _lock:
            entries = _load()
            entries.append({"action": action, "name": name, "at": time.strftime("%Y-%m-%d")})
            _save(entries)
        _ensure_worker()
        _wake.set()
    except Exception:
        logger.warning("device: skill mirror could not queue %s %s", action, name, exc_info=True)


def merge(entries: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """One entry per skill, in order of first appearance. A skill created in the batch stays a
    create unless it was deleted again; otherwise the newest action names the commit."""
    merged: dict[str, dict[str, Any]] = {}
    for e in entries:
        old = merged.get(e["name"])
        if old is None:
            merged[e["name"]] = dict(e)
        elif e["action"] == "delete" or old["action"] != "create":
            merged[e["name"]] = {**e, "at": old["at"] if old["action"] == "create" else e["at"]}
    return list(merged.values())


# --- hook ---------------------------------------------------------------------------------------

def _changes(args: Any, result: Any) -> list[tuple[str, str]]:
    """``(action, name)`` for each op of a skill_manage call that landed."""
    if not isinstance(args, dict):
        return []
    try:
        res = json.loads(result) if isinstance(result, str) else result
    except ValueError:
        return []
    if not isinstance(res, dict) or res.get("success") is not True or res.get("staged"):
        return []
    ops = args.get("operations")
    if not isinstance(ops, list):
        ops = [args]
    found = []
    for op in ops:
        if isinstance(op, dict) and op.get("action") in ACTIONS and op.get("name"):
            found.append((op["action"], op["name"]))
    return found


def post_tool_call(tool_name: str = "", args: Any = None, result: Any = None, **_kwargs) -> None:
    if tool_name != "skill_manage":
        return
    try:
        if config() is None:
            return
        for action, name in _changes(args, result):
            enqueue(action, name)
    except Exception:
        logger.warning("device: skill mirror hook failed", exc_info=True)


# --- worker -------------------------------------------------------------------------------------

def _skill_roots() -> list[Path]:
    roots = []
    try:
        from tools.skill_manager_tool import _skills_dir
        roots.append(Path(_skills_dir()))
    except Exception:
        return []
    try:
        from agent.skill_utils import get_skill_create_dir
        extra = get_skill_create_dir()
        if extra:
            roots.append(Path(extra))
    except Exception:
        pass
    return roots


def locate(name: str) -> Optional[tuple[Path, str]]:
    """``(skill dir, path relative to its skills root)`` of an installed skill, or None."""
    from tools.skill_manager_tool import _find_skill
    found = _find_skill(name)
    if not found:
        return None
    skill_dir = Path(found["path"]).resolve()
    for root in _skill_roots():
        try:
            return skill_dir, skill_dir.relative_to(root.resolve()).as_posix()
        except ValueError:
            continue
    return None


def pack(skill_dir: Path) -> bytes:
    """The skill's files as a tar archive with fixed metadata, so equal content packs equal."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for path in sorted(skill_dir.rglob("*")):
            rel = path.relative_to(skill_dir)
            if "__pycache__" in rel.parts or path.suffix == ".pyc" or path.is_symlink() or not path.is_file():
                continue
            info = tarfile.TarInfo(rel.as_posix())
            data = path.read_bytes()
            info.size, info.mode, info.mtime = len(data), 0o755 if os.access(path, os.X_OK) else 0o644, 0
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


# $1 repo, $2 mode (put or delete), $3 path of the skill in the repo (put) or its name (delete),
# $4 commit subject, $5 README row to add (put), $6 name. A put replaces the skill's directory
# with the archive on stdin. Local edits there are committed first, so nothing is lost. Prints the
# new commit or "nochange".
SCRIPT = r"""
set -eu
repo=$1; mode=$2; rel=$3; msg=$4; row=$5; name=$6
cd "$repo"
git rev-parse --git-dir >/dev/null
if [ "$mode" = delete ]; then
  rel=$(git ls-files | grep -E "(^|/)$name/SKILL\.md\$" | head -n 1 | sed 's#/SKILL\.md$##' || true)
  [ -n "$rel" ] || { echo nochange; exit 0; }
fi
if [ -n "$(git status --porcelain -- "$rel")" ]; then
  git add -A -- "$rel"
  git commit -q -m "$name: save local edits" -- "$rel" || true
fi
if [ "$mode" = put ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT INT TERM
  base64 -d | tar -xf - -C "$tmp"
  rm -rf -- "$rel"; mkdir -p -- "$rel"; cp -a "$tmp"/. "$rel"/
  paths="$rel"
  if [ -n "$row" ] && [ -f README.md ] && ! grep -qF "| $name |" README.md; then
    awk -v row="$row" '{l[NR]=$0} /^\| /{last=NR} END{for(i=1;i<=NR;i++){print l[i]; if(i==last) print row}}' README.md >"$tmp/readme"
    cat "$tmp/readme" >README.md
    paths="$paths README.md"
  fi
else
  rm -rf -- "$rel"
  paths="$rel"
  if [ -f README.md ] && grep -qF "| $name |" README.md; then
    grep -vF "| $name |" README.md >"${TMPDIR:-/tmp}/readme.$$" || true
    cat "${TMPDIR:-/tmp}/readme.$$" >README.md; rm -f "${TMPDIR:-/tmp}/readme.$$"
    paths="$paths README.md"
  fi
fi
# shellcheck disable=SC2086
git add -A -- $paths
# shellcheck disable=SC2086
if git diff --cached --quiet -- $paths; then echo nochange; exit 0; fi
# shellcheck disable=SC2086
git commit -q -m "$msg" -- $paths
git rev-parse --short HEAD
"""


def _send(device_id: str, repo: str, mode: str, rel: str, subject: str, row: str, name: str,
          payload: str) -> tuple[int, str, str]:
    core = _core()
    return core.run_script(device_id, SCRIPT, [repo, mode, rel, subject, row, name], cwd=repo,
                           timeout=RUN_TIMEOUT_S, stdin=payload or None)


def _row(name: str, rel: str, day: str) -> str:
    category = rel.rsplit("/", 1)[0] if "/" in rel else ""
    return f"| {name} | {category or '(top level)'} | {ORIGIN} ({day}) |"


def sync_one(entry: dict[str, Any], device_id: str, repo: str) -> bool:
    """Mirror one skill. True when it is done (committed, or nothing to commit)."""
    name, action = entry["name"], entry["action"]
    where = None if action == "delete" else locate(name)
    if where is None:  # deleted, or gone since it was queued: the repository follows the pod
        code, out, err = _send(device_id, repo, "delete", "", f"{name}: delete", "", name, "")
    else:
        skill_dir, rel = where
        payload = base64.b64encode(pack(skill_dir)).decode("ascii")
        if len(payload) > MAX_PAYLOAD_BYTES:
            logger.warning("device: skill mirror skips %s: %d bytes is over the limit", name, len(payload))
            return True
        row = _row(name, rel, entry.get("at") or time.strftime("%Y-%m-%d")) if action == "create" else ""
        subject = f"{name}: {action}"
        code, out, err = _send(device_id, repo, "put", rel, subject, row, name, payload)
    if code == 0 and err == "":
        logger.info("device: skill mirror %s %s: %s", action, name, out.strip() or "done")
        return True
    logger.warning("device: skill mirror %s %s failed (exit %s): %s", action, name, code,
                   (err or out).strip()[:200])
    return False


def drain_once() -> bool:
    """Try every queued skill once. True when the queue is empty afterwards (or mirroring is off)."""
    target = config()
    if target is None:
        return True
    device_id, repo = target
    if _core().hub().device(device_id) is None:
        return not pending()
    with _lock:
        taken = _load()
    counts: dict[str, int] = {}
    for e in taken:
        counts[e["name"]] = counts.get(e["name"], 0) + 1
    for entry in merge(taken):
        try:
            ok = sync_one(entry, device_id, repo)
        except Exception:
            logger.warning("device: skill mirror of %s raised", entry["name"], exc_info=True)
            ok = False
        if not ok:
            continue
        with _lock:  # drop only what was sent: a change queued meanwhile stays for the next round
            skip, kept = counts[entry["name"]], []
            for e in _load():
                if e["name"] == entry["name"] and skip > 0:
                    skip -= 1
                else:
                    kept.append(e)
            _save(kept)
    return not pending()


def _run() -> None:
    global _worker
    while True:
        try:
            if config() is None:
                with _lock:
                    _worker = None
                return
            if drain_once():
                with _lock:
                    if not _load():
                        _worker = None
                        return
        except Exception:
            logger.warning("device: skill mirror worker error", exc_info=True)
        _wake.clear()
        _sleep(RETRY_SECONDS)


def _ensure_worker() -> None:
    global _worker
    with _lock:
        if _worker is not None and _worker.is_alive():
            return
        _worker = threading.Thread(target=_run, name="device-skill-mirror", daemon=True)
        _worker.start()


def register(ctx: Any, core: Any) -> None:
    ctx.register_hook("post_tool_call", post_tool_call)
    if config() is not None and pending():
        _ensure_worker()
