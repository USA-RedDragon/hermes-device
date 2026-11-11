"""Verifier for the hub assertion (docs/protocol.md section 3).

It accepts exactly what ``assertion.Verify`` in the Go package accepts: the same encoding, the
same claim checks in the same order, the same clock rules and the same nonce cache. The decoding
helpers copy the Go standard library's behavior (``encoding/base64``, ``encoding/pem`` and
``encoding/json`` into a struct) where it differs from Python's.
"""
from __future__ import annotations

import json
import re
import threading
import time
import unicodedata
from dataclasses import dataclass
from typing import Callable, Optional

HEADER = "X-Hermes-Device-Assertion"
PREFIX = "v1."
TYPE = "hermes-device-assertion"
MAX_LIFETIME_S = 30
CLOCK_SKEW_S = 5
MAX_ENCODED_LEN = 4096
LEG_EXEC = "exec"
LEG_ACP = "acp"
LEG_USAGE = "usage"
SIGNATURE_SIZE = 64
PUBLIC_KEY_SIZE = 32

_NS = 1_000_000_000
_DEVICE_ID = re.compile(r"[A-Za-z0-9_-]{1,64}")
_NONCE = re.compile(r"[A-Za-z0-9_-]{22,128}")
_STD = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
_URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
_INT64_MIN, _INT64_MAX = -(1 << 63), (1 << 63) - 1


class Refused(Exception):
    """The assertion was refused. ``kind`` names the Go error it matches: malformed, signature,
    claims, expired, replay or no_keys."""

    def __init__(self, kind: str, detail: str = ""):
        super().__init__(f"{kind}: {detail}" if detail else kind)
        self.kind = kind
        self.detail = detail


@dataclass(frozen=True)
class Claims:
    typ: str = ""
    aud: str = ""
    leg: str = ""
    htm: str = ""
    htp: str = ""
    sub: str = ""
    device_id: str = ""
    device_name: str = ""
    iat: int = 0
    exp: int = 0
    nonce: str = ""


@dataclass(frozen=True)
class Expect:
    audience: str
    leg: str
    method: str
    path: str


def _b64decode(text: str, alphabet: str, padded: bool) -> bytes:
    """``base64.Encoding.DecodeString`` of Go (non-strict): CR and LF are skipped, nonzero
    trailing bits are accepted, any other byte outside the alphabet is an error."""
    import base64
    import binascii

    text = text.replace("\r", "").replace("\n", "")
    body = text
    if padded:
        if len(text) % 4:
            raise ValueError("bad length")
        stripped = text.rstrip("=")
        pad = len(text) - len(stripped)
        if pad > 2 or (pad and len(stripped) % 4 in (0, 1)):
            raise ValueError("bad padding")
        body = stripped
    elif len(text) % 4 == 1:
        raise ValueError("bad length")
    if any(c not in alphabet for c in body):
        raise ValueError("illegal character")
    std = body.translate(str.maketrans("-_", "+/")) if alphabet is _URL else body
    try:
        return base64.b64decode(std + "=" * (-len(std) % 4), validate=True)
    except binascii.Error as exc:
        raise ValueError(str(exc)) from exc


def _go_fold(name: str) -> str:
    """Folded form used by Go's encoding/json to match object keys to field names
    case-insensitively. Only runes that fold to ASCII letters matter for our ASCII names."""
    out = []
    for c in name:
        if c < "\x80":
            out.append(c.upper())
        elif c == "K":
            out.append("K")
        elif c == "ſ":
            out.append("S")
        else:
            out.append(c)
    return "".join(out)


_STRING_FIELDS = ("typ", "aud", "leg", "htm", "htp", "sub", "device_id", "device_name", "nonce")
_INT_FIELDS = ("iat", "exp")
_FIELDS = _STRING_FIELDS + _INT_FIELDS
_FOLDED = {_go_fold(f): f for f in _FIELDS}


class _NotInt:
    """A JSON number Go's ``strconv.ParseInt`` would not accept (fraction or exponent)."""

    def __init__(self, text: str):
        self.text = text


def _reject_constant(name: str):
    raise ValueError(f"invalid JSON literal {name}")


def _decode_claims(payload: bytes) -> Claims:
    """``json.Unmarshal(payload, &Claims{})`` with Go's rules: keys match exactly, else
    case-insensitively; later duplicates win; null leaves a field unchanged; unknown keys are
    ignored; a type mismatch or an integer that does not fit int64 is an error."""
    text = payload.decode("utf-8", errors="replace")
    pairs_holder: list = []

    def hook(pairs):
        pairs_holder.append(pairs)
        return pairs

    doc = json.loads(text, object_pairs_hook=hook, parse_float=_NotInt, parse_constant=_reject_constant)
    values: dict = {}
    if doc is None:
        return Claims()
    if not isinstance(doc, list) or not pairs_holder or doc is not pairs_holder[-1]:
        raise ValueError("claims are not a JSON object")
    for key, value in doc:
        field = key if key in _FIELDS else _FOLDED.get(_go_fold(key))
        if field is None or value is None:
            continue
        if field in _STRING_FIELDS:
            if not isinstance(value, str):
                raise ValueError(f"{field}: not a string")
        else:
            if isinstance(value, bool) or not isinstance(value, int):
                raise ValueError(f"{field}: not an integer")
            if not _INT64_MIN <= value <= _INT64_MAX:
                raise ValueError(f"{field}: out of range")
        values[field] = value
    return Claims(**values)


def _utf8_len(s: str) -> int:
    return len(s.encode("utf-8", errors="surrogatepass"))


def _check_claims(c: Claims, want: Expect) -> None:
    if c.typ != TYPE:
        raise Refused("claims", "typ")
    if not want.audience or c.aud != want.audience:
        raise Refused("claims", "aud")
    if c.leg != want.leg:
        raise Refused("claims", "leg")
    if c.htm != want.method:
        raise Refused("claims", "htm")
    if c.htp != want.path:
        raise Refused("claims", "htp")
    if not c.sub or _utf8_len(c.sub) > 256:
        raise Refused("claims", "sub")
    if not _DEVICE_ID.fullmatch(c.device_id):
        raise Refused("claims", "device_id")
    if not c.device_name or _utf8_len(c.device_name) > 64:
        raise Refused("claims", "device_name")
    if not _NONCE.fullmatch(c.nonce):
        raise Refused("claims", "nonce")


class Verifier:
    """Checks assertions against a set of Ed25519 public keys and remembers nonces until they
    expire. Safe for concurrent use. ``now_ns`` returns Unix time in nanoseconds."""

    def __init__(self, keys: Optional[list] = None, now_ns: Callable[[], int] = time.time_ns):
        self.keys = list(keys or [])
        self.now_ns = now_ns
        self._lock = threading.Lock()
        self._nonces: dict[str, int] = {}

    def verify(self, encoded, want: Expect) -> Claims:
        """Check ``encoded`` (the raw header bytes, or a str taken as UTF-8) and return its
        claims. A nonce is consumed only when every other check passes."""
        from cryptography.exceptions import InvalidSignature

        keys = list(self.keys)
        if not keys:
            raise Refused("no_keys")
        if isinstance(encoded, str):
            raw = encoded.encode("utf-8", errors="surrogatepass")
        elif isinstance(encoded, (bytes, bytearray)):
            raw = bytes(encoded)
        else:
            raise Refused("malformed")
        encoded = raw.decode("latin-1")
        if len(raw) > MAX_ENCODED_LEN or not encoded.startswith(PREFIX):
            raise Refused("malformed")
        signed, dot, sig_text = encoded[len(PREFIX):].partition(".")
        if not dot or "." in sig_text:
            raise Refused("malformed")
        try:
            payload = _b64decode(signed, _URL, padded=False)
            sig = _b64decode(sig_text, _URL, padded=False)
        except ValueError:
            raise Refused("malformed") from None
        if len(sig) != SIGNATURE_SIZE:
            raise Refused("malformed")
        msg = (PREFIX + signed).encode("latin-1")
        good = False
        for key in keys:
            try:
                key.verify(sig, msg)
                good = True
                break
            except InvalidSignature:
                continue
        if not good:
            raise Refused("signature")
        try:
            claims = _decode_claims(payload)
        except (ValueError, RecursionError):
            raise Refused("malformed") from None
        _check_claims(claims, want)
        now = self.now_ns()
        iat, exp = claims.iat * _NS, claims.exp * _NS
        if exp < iat or exp - iat > MAX_LIFETIME_S * _NS or iat > now + CLOCK_SKEW_S * _NS \
                or now > exp + CLOCK_SKEW_S * _NS:
            raise Refused("expired")
        with self._lock:
            for nonce, until in list(self._nonces.items()):
                if now > until:
                    del self._nonces[nonce]
            if claims.nonce in self._nonces:
                raise Refused("replay")
            self._nonces[claims.nonce] = exp + CLOCK_SKEW_S * _NS
        return claims


# --- public keys ------------------------------------------------------------------------------

def _get_line(data: bytes) -> tuple[bytes, bytes, int]:
    i = data.find(b"\n")
    if i < 0:
        i = j = len(data)
    else:
        j = i + 1
        if i > 0 and data[i - 1:i] == b"\r":
            i -= 1
    return data[:i].rstrip(b" \t"), data[j:], j


def _pem_decode(data: bytes) -> tuple[Optional[tuple[str, bytes]], bytes]:
    """Go's ``pem.Decode``: ``((type, bytes), rest)`` for the first valid block, or
    ``(None, data)``. Text before the block is skipped."""
    pem_start, pem_end, eol = b"\n-----BEGIN ", b"\n-----END ", b"-----"
    rest = data
    end_trailer_index = 0
    while True:
        if end_trailer_index < 0 or end_trailer_index > len(rest):
            return None, data
        rest = rest[end_trailer_index:]
        end_index = rest.find(pem_end)
        if end_index < 0:
            return None, data
        end_trailer_index = end_index + len(pem_end)
        begin_index = rest[:end_index].rfind(pem_start[1:])
        if begin_index < 0 or (begin_index > 0 and rest[begin_index - 1:begin_index] != b"\n"):
            continue
        shift = begin_index + len(pem_start) - 1
        rest = rest[shift:]
        end_index -= shift
        end_trailer_index -= shift
        type_line, rest, consumed = _get_line(rest)
        end_index -= consumed
        end_trailer_index -= consumed
        if not type_line.endswith(eol):
            continue
        type_line = type_line[:-len(eol)]
        headers = 0
        while True:
            if not rest:
                return None, data
            line, nxt, consumed = _get_line(rest)
            if b":" not in line:
                break
            headers += 1
            rest = nxt
            end_index -= consumed
            end_trailer_index -= consumed
        if headers and end_index < 0:
            continue
        if end_trailer_index < 0:
            raise Refused("malformed", "PEM BEGIN and END on one line")
        end_trailer = rest[end_trailer_index:]
        end_trailer_len = len(type_line) + len(eol)
        if len(end_trailer) < end_trailer_len:
            continue
        rest_of_end_line = end_trailer[end_trailer_len:]
        end_trailer = end_trailer[:end_trailer_len]
        if not end_trailer.startswith(type_line) or not end_trailer.endswith(eol):
            continue
        if _get_line(rest_of_end_line)[0]:
            continue
        block = b""
        if end_index > 0:
            b64 = rest[:end_index].replace(b" ", b"").replace(b"\t", b"")
            try:
                block = _b64decode(b64.decode("latin-1"), _STD, padded=True)
            except ValueError:
                continue
        _, rest, _ = _get_line(rest[end_index + len(pem_end) - 1:])
        return (type_line.decode("utf-8", errors="replace"), block), rest


def _is_go_space(c: str) -> bool:
    return c in "\t\n\v\f\r \x85\xa0" or unicodedata.category(c) in ("Zs", "Zl", "Zp")


def _go_trim_space(s: str) -> str:
    start, end = 0, len(s)
    while start < end and _is_go_space(s[start]):
        start += 1
    while end > start and _is_go_space(s[end - 1]):
        end -= 1
    return s[start:end]


def parse_public_keys(text) -> list:
    """Ed25519 public keys from PEM ``PUBLIC KEY`` blocks and base64 lines of 32 raw bytes, read
    line by line as Go's ``assertion.ParsePublicKeys``. Raises :class:`Refused` (malformed or
    no_keys)."""
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

    if isinstance(text, (bytes, bytearray)):
        text = bytes(text).decode("utf-8", errors="surrogateescape")
    keys = []
    lines = text.split("\n")
    i = 0
    while i < len(lines):
        line = _go_trim_space(lines[i])
        if not line or line.startswith("#"):
            i += 1
            continue
        if line.startswith("-----BEGIN "):
            j = i
            while j < len(lines) and not _go_trim_space(lines[j]).startswith("-----END "):
                j += 1
            if j == len(lines):
                raise Refused("malformed", "unterminated PEM block")
            keys.append(_parse_pem_key("\n".join(lines[i:j + 1])))
            i = j + 1
            continue
        try:
            raw = _decode_any_base64(line)
        except ValueError:
            raw = b""
        if len(raw) != PUBLIC_KEY_SIZE:
            raise Refused("malformed", "key line is not 32 bytes of base64")
        keys.append(Ed25519PublicKey.from_public_bytes(raw))
        i += 1
    if not keys:
        raise Refused("no_keys")
    return keys


def _parse_pem_key(text: str):
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from cryptography.hazmat.primitives.serialization import load_der_public_key

    block, _ = _pem_decode(text.encode("utf-8", errors="surrogateescape"))
    if block is None:
        raise Refused("malformed", "bad PEM block")
    kind, der = block
    if kind != "PUBLIC KEY":
        raise Refused("malformed", f"PEM block {kind!r}")
    try:
        pub = load_der_public_key(der)
    except Exception as exc:
        raise Refused("malformed", str(exc)) from None
    if not isinstance(pub, Ed25519PublicKey):
        raise Refused("malformed", "not an Ed25519 key")
    return pub


def _decode_any_base64(s: str) -> bytes:
    """Go's ``strings.TrimRight(s, "=")`` then raw URL or raw standard base64."""
    s = s.rstrip("=")
    if "-" in s or "_" in s:
        return _b64decode(s, _URL, padded=False)
    return _b64decode(s, _STD, padded=False)
