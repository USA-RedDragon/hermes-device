"""Waiting without sleep polling, for chats and their subagents (part of subagent-routes).

``GUIDANCE`` is the line ``pre_llm_call`` adds for a subagent on each turn and for a chat once. ``sleep_poll`` finds a
foreground ``terminal`` command that waits by sleeping, which the ``pre_tool_call`` guard refuses with ``REFUSAL``:
a ``sleep`` of more than ``MAX_SLEEP`` seconds alone or chained with ``;``, ``&&``, ``||`` or ``|``, and any
``while`` or ``until`` loop that sleeps. Quoted text and comments are not commands, a script the command runs is not
read, a ``sleep`` sent to the background with ``&`` does not block, and a ``background=true`` command is never
refused: a background loop that sleeps is the right way to wait for a condition.
"""

from __future__ import annotations

import os
import re
import shlex
from typing import Any, Mapping, Optional

MAX_SLEEP = 30.0
GUIDANCE = ("[Waiting: to wait for something slow, start it with terminal(background=true), then call "
            "process_manage(action='wait', session_id=..., timeout=...). Repeat the wait if it times out. Never use sleep "
            "to poll. To wait for a condition, run one background command that loops until the condition holds, then "
            "wait on it.]")
GUIDANCE_MARK = "[Waiting: to wait for something slow"
REFUSAL = ("Refused: this waits by sleeping. Start the slow part with terminal(background=true), then call "
           "process_manage(action='wait', session_id=<id>, timeout=<seconds>), and repeat wait if it times out. To wait "
           "for a condition, run one background command that loops until it holds, then wait on that.")

_SEPARATORS = frozenset({";", "&&", "||", "|", "|&", "&", "\n", "(", ")", "{", "}", ";;"})
_PREFIX_WORDS = frozenset({"do", "then", "else", "elif", "!", "time", "command", "exec", "nohup", "nice"})
_LOOP_OPEN = frozenset({"while", "until", "for", "select"})
_DURATION = re.compile(r"^(\d+(?:\.\d*)?|\.\d+)([smhd]?)$")
_UNITS = {"": 1.0, "s": 1.0, "m": 60.0, "h": 3600.0, "d": 86400.0}
_ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")


def _tokens(command: str) -> Optional[list[str]]:
    lex = shlex.shlex(command, posix=True, punctuation_chars=";&|()<>\n")
    lex.whitespace = " \t\r"
    lex.whitespace_split = True
    try:
        return list(lex)
    except ValueError:  # unbalanced quotes: the shell refuses it anyway
        return None


def _seconds(args: list[str]) -> Optional[float]:
    """GNU sleep's total for its arguments; None when one is not a literal duration ($DELAY, a substitution)."""
    total = 0.0
    for arg in args:
        if arg in ("inf", "infinity"):
            return float("inf")
        match = _DURATION.match(arg)
        if match is None:
            return None
        total += float(match.group(1)) * _UNITS[match.group(2)]
    return total


def sleep_poll(command: Any) -> bool:
    """Whether a foreground ``command`` waits by sleeping (see the module docstring)."""
    if not isinstance(command, str) or "sleep" not in command:
        return False
    tokens = _tokens(command)
    if not tokens:
        return False
    loops: list[str] = []
    at_command = True
    i = 0
    while i < len(tokens):
        token = tokens[i]
        if token in _SEPARATORS:
            at_command = True
            i += 1
            continue
        if not at_command:
            i += 1
            continue
        if token in _LOOP_OPEN:
            loops.append(token)
            at_command = token in ("while", "until")
            i += 1
            continue
        if token == "done":
            if loops:
                loops.pop()
            at_command = False
            i += 1
            continue
        if token in _PREFIX_WORDS or _ASSIGNMENT.match(token):
            i += 1
            continue
        at_command = False
        if os.path.basename(token) != "sleep":
            i += 1
            continue
        end = i + 1
        while end < len(tokens) and tokens[end] not in _SEPARATORS:
            end += 1
        if end < len(tokens) and tokens[end] == "&":
            i = end
            continue
        seconds = _seconds([arg for arg in tokens[i + 1:end] if not arg.startswith("-")])
        if seconds is not None and (seconds > MAX_SLEEP or (seconds > 0 and {"while", "until"} & set(loops))):
            return True
        i = end
    return False


def refusal(tool_name: Any, args: Any) -> Optional[str]:
    """``REFUSAL`` for a foreground ``terminal`` call that waits by sleeping, else None."""
    if tool_name != "terminal" or not isinstance(args, Mapping):
        return None
    background = args.get("background")
    if background is True or (isinstance(background, str) and background.strip().lower() == "true"):
        return None
    return REFUSAL if sleep_poll(args.get("command")) else None


def has_guidance(history: Any) -> bool:
    """Whether a message in ``history`` already carries ``GUIDANCE`` (``pre_llm_call`` context is kept with the
    user row it rode, in ``api_content``)."""
    for message in history if isinstance(history, list) else ():
        if not isinstance(message, Mapping) or message.get("role") != "user":
            continue
        for key in ("api_content", "content"):
            value = message.get(key)
            if isinstance(value, str) and GUIDANCE_MARK in value:
                return True
            if isinstance(value, list) and any(isinstance(p, Mapping) and GUIDANCE_MARK in str(p.get("text") or "")
                                               for p in value):
                return True
    return False
