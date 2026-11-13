"""Publish a page from an HTML file: the ``page_publish`` tool reads an HTML file through the chat's
device (the same path the file tools use, so it works on a computer, the sandbox and the Hermes
server), copies it into the Hermes server's page store and returns the one line the agent puts in
its reply. The hub shows that line as the same page card as an inline ``page`` block (docs/protocol.md
section 12.4), so the agent never writes the HTML into its reply.

The copy is made at publish time under a name taken from the file's content: editing the source
later does not change a page already shown, and publishing a changed file makes a new page.
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import secrets
import shutil
import stat
import sys
import threading
import time
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("hermes_device")

TOOLSET = "device-agents"
EMOJI = "\U0001f4c4"
# The largest page the tool publishes. The hub serves pages up to 16 MB; a page that needs more than
# this holds data that belongs in a file the user downloads.
MAX_PAGE_BYTES = 2 * 1024 * 1024
# Pages older than a week go, and each session folder keeps its newest 50 (as screenshots do).
PAGE_MAX_AGE_S = 7 * 24 * 3600
PAGE_MAX_PER_SESSION = 50

_HTML_MARK = re.compile(r"<!doctype\s+html|<html[\s>]|<head[\s>]|<body[\s>]|<title[\s>]", re.IGNORECASE)
_TITLE = re.compile(r"<title[^>]*>(.*?)</title>", re.IGNORECASE | re.DOTALL)
_LOCK = threading.Lock()


def _core():
    return sys.modules["hermes_device_core"]


def _err(message: str, code: str) -> str:
    return json.dumps({"error": message, "code": code}, ensure_ascii=False)


def pages_root() -> Path:
    """Where the tool keeps its copies: one folder per session under the Hermes cache, which the hub
    reads like browser screenshots (hub.DefaultPage)."""
    return _core().hermes_home() / "cache" / "pages" / "hermes-device"


def _session_dir_name(session_id: str) -> str:
    name = re.sub(r"[^A-Za-z0-9_.-]", "_", session_id or "")[:128].strip(".")
    return name or "no-session"


def check_html(raw: bytes) -> str:
    """The page's text, or ValueError saying why the bytes are not an HTML page: empty, over the size
    cap, not UTF-8 text, or without an HTML document marker near the start."""
    if not raw.strip():
        raise ValueError("the file is empty")
    if len(raw) > MAX_PAGE_BYTES:
        raise ValueError(f"the file is larger than {MAX_PAGE_BYTES // (1024 * 1024)} MB")
    if b"\0" in raw:
        raise ValueError("the file is binary, not HTML")
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError:
        raise ValueError("the file is not UTF-8 text") from None
    if not _HTML_MARK.search(text[:8192]):
        raise ValueError("the file is not an HTML document: it has no <!doctype html>, <html>, <head>, <body> or "
                         "<title> near the start")
    return text


def page_title(text: str) -> str:
    m = _TITLE.search(text[:65536])
    return " ".join(m.group(1).split())[:120] if m else ""


def _read_local(path: str) -> bytes:
    """A file of the Hermes server, for a chat that runs there: the file tools' read rules (Hermes's
    credential denylist) and a regular file of at most MAX_PAGE_BYTES."""
    from agent import file_safety

    core = _core()
    p = os.path.expanduser(path)
    if not os.path.isabs(p):
        p = os.path.join(core.session_cwd() or str(core.terminal_cwd() or "") or os.getcwd(), p)
    p = os.path.normpath(p)
    for candidate in dict.fromkeys((p, os.path.realpath(p))):
        why = file_safety.get_read_block_error(candidate)
        if why:
            raise core.BindError(422, "read_refused", why)
    try:
        st = os.stat(p)
    except FileNotFoundError:
        raise core.BindError(404, "not_found", f"{p} does not exist") from None
    except OSError as exc:
        raise core.BindError(422, "read_failed", f"{p} cannot be read: {exc.strerror}") from None
    if not stat.S_ISREG(st.st_mode):
        raise core.BindError(422, "read_refused", f"{p} is not a regular file")
    if st.st_size > MAX_PAGE_BYTES:
        raise core.BindError(413, "too_large", f"{p} is larger than {MAX_PAGE_BYTES // (1024 * 1024)} MB")
    with open(p, "rb") as fh:
        return fh.read(MAX_PAGE_BYTES + 1)


def read_source(session_id: str, path: str) -> bytes:
    """The bytes of ``path`` on the machine the session runs on. Raises :class:`BindError` when the
    path is refused, missing or too large, the device is offline, or none serves the session."""
    core = _core()
    path = (path or "").strip()
    try:
        res = core.upload_resolution(session_id)
    except core.BindError as exc:
        if exc.code != "runs_on_pod":
            raise
        return _read_local(path)
    try:
        absolute = core.device_abs_path(res, path)
    except core.DeviceError as exc:
        raise core.BindError(400, "bad_path", str(exc)) from None
    _, raw = core.get_session_file(session_id, absolute, max_bytes=MAX_PAGE_BYTES)
    return raw


def prune_pages(root: Path, *, now: Optional[float] = None, max_age: float = PAGE_MAX_AGE_S,
                max_files: int = PAGE_MAX_PER_SESSION, keep_room_in: Optional[Path] = None) -> int:
    """Delete pages older than max_age, then in each session folder the oldest ones over max_files
    (one fewer in keep_room_in, the folder about to get a page). Returns how many went."""
    now = time.time() if now is None else now
    gone = 0
    try:
        folders = [d for d in root.iterdir() if d.is_dir() and not d.is_symlink()]
    except OSError:
        return 0
    for folder in folders:
        files = []
        try:
            for f in folder.iterdir():
                try:
                    st = f.lstat()
                except OSError:
                    continue
                if stat.S_ISREG(st.st_mode):
                    files.append((st.st_mtime, f))
        except OSError:
            continue
        files.sort(key=lambda t: t[0], reverse=True)
        limit = max_files - (1 if folder == keep_room_in else 0)
        for i, (mtime, f) in enumerate(files):
            if now - mtime > max_age or i >= limit:
                try:
                    f.unlink()
                    gone += 1
                except OSError:
                    pass
        try:
            folder.rmdir()
        except OSError:
            pass
    return gone


def save_page(session_id: str, raw: bytes, *, root: Optional[Path] = None) -> Path:
    """Write the page to the session's folder under a name from its content and return the path. The
    same content gives the same file."""
    root = pages_root() if root is None else root
    folder = root / _session_dir_name(session_id)
    path = folder / ("page-" + hashlib.sha256(raw).hexdigest()[:16] + ".html")
    with _LOCK:
        prune_pages(root, keep_room_in=folder)
        folder.mkdir(mode=0o700, parents=True, exist_ok=True)
        if path.is_file() and not path.is_symlink():
            os.utime(path)
            return path
        tmp = folder / (".tmp-" + secrets.token_hex(6))
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(tmp, flags, 0o600)
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(raw)
            os.replace(tmp, path)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise
    return path


def media_line(path: Path) -> str:
    """The reply line the hub shows as a page card: MEDIA: and the absolute path, in backticks when
    the path has white space."""
    p = str(path)
    return f"MEDIA:`{p}`" if re.search(r"\s", p) else f"MEDIA:{p}"


def publish(session_id: str, path: str, *, root: Optional[Path] = None) -> dict:
    raw = read_source(session_id, path)
    text = check_html(raw)
    saved = save_page(session_id, text.encode("utf-8") if raw.startswith(b"\xef\xbb\xbf") else raw, root=root)
    title = page_title(text)
    out = {"ok": True, "path": str(saved), "bytes": len(raw), "title": title,
           "show_user": media_line(saved)}
    out["note"] = ("The page is saved. To show it to the user, put the show_user line, exactly as written, on its own "
                   "line in your reply, with a sentence before it that says what the page is for. The hub shows a "
                   "page card with the page's title and an Open button, as for an inline page block; do not paste "
                   "the HTML. The saved copy does not change when you edit the file: to change the page, edit the "
                   "file and call page_publish again, then put the new line in your reply.")
    if not title:
        out["warning"] = "the page has no <title>, so its card reads \"Page\"; add one and publish again"
    return out


def _handler(args: dict, task_id: str = "", session_id: str = "", **_kwargs) -> str:
    core = _core()
    args = args if isinstance(args, dict) else {}
    path = args.get("path")
    if not isinstance(path, str) or not path.strip():
        return _err("path must be the path of an HTML file", "bad_arguments")
    sid = session_id or core.current_session_id()
    if not sid:
        return _err("this tool call has no session", "no_session")
    try:
        return json.dumps(publish(sid, path), ensure_ascii=False)
    except core.BindError as exc:
        return _err(exc.message, exc.code)
    except ValueError as exc:
        return _err(str(exc), "not_html")
    except core.DeviceError as exc:
        return _err(str(exc), "device_error")
    except OSError as exc:
        logger.warning("device: page_publish could not save a page", exc_info=True)
        return _err(f"the page could not be saved: {exc.strerror or exc}", "save_failed")


def on_session_delete(session_id: str = "", reason: str = "", deleted_ids: Any = None, **_kwargs) -> None:
    """Hermes's on_session_delete hook: remove the pages of deleted sessions. An archived session
    keeps them, since it can come back; the age limit removes them later."""
    if reason in ("archive", "profile_repair"):
        return
    root = pages_root()
    for sid in (session_id, *(deleted_ids or ())):
        if sid:
            shutil.rmtree(root / _session_dir_name(str(sid)), ignore_errors=True)


SCHEMAS = {
    "page_publish": {
        "name": "page_publish",
        "description": (
            "Show an HTML file as a page in the Talaria hub without writing its HTML into your reply. Give the "
            "path of the file on the machine this chat runs on (a computer, the sandbox or the server; a "
            "relative path starts in the chat's folder). The file must be one self-contained HTML document of "
            "at most 2 MB. The tool copies it and returns a show_user line, \"MEDIA:<path>\"; put that line, "
            "exactly as written, on its own line in your reply. The user then sees a page card with the file's "
            "<title> and an Open button, with the same hermes.send, hermes.draft and hermes.copy buttons as an "
            "inline page block, and in Telegram a link. Later edits of the file do not change a page already "
            "shown; call the tool again for a new version. Prefer this to an inline ```page block when the page "
            "is longer than a screen."),
        "parameters": {
            "type": "object",
            "properties": {"path": {"type": "string", "description": "The HTML file to publish"}},
            "required": ["path"],
            "additionalProperties": False,
        },
    },
}


def available() -> bool:
    """check_fn: the hub that shows the page is connected."""
    agents = sys.modules.get("hermes_device_agents")
    return bool(agents is not None and agents.BUS.connected())


def register(ctx: Any, core: Any) -> None:
    for name, schema in SCHEMAS.items():
        ctx.register_tool(name, TOOLSET, schema, _handler, check_fn=available,
                          description=schema["description"], emoji=EMOJI)
    ctx.register_hook("on_session_delete", on_session_delete)
