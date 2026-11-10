"""Hub assertion verifier tests. assertion_vectors.json was produced by the Go package
(assertion.Sign, Verify and ParsePublicKeys), so these tests check that the Python verifier
accepts and refuses exactly what Go does."""
from __future__ import annotations

import base64
import importlib.util
import json
import sys
import time
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("hermes_device_assertion", _HERE.parent / "assertion.py")
assertion = sys.modules.get("hermes_device_assertion") or importlib.util.module_from_spec(_spec)
if "hermes_device_assertion" not in sys.modules:
    sys.modules["hermes_device_assertion"] = assertion
    _spec.loader.exec_module(assertion)

VECTORS = json.loads((_HERE / "assertion_vectors.json").read_text(encoding="utf-8"))
_NS = 1_000_000_000


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


class Signer:
    def __init__(self, seed: bytes = bytes(range(1, 33))):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        self.key = Ed25519PrivateKey.from_private_bytes(seed)
        self.public = self.key.public_key()

    def raw(self, payload: bytes) -> str:
        signed = "v1." + _b64(payload)
        return signed + "." + _b64(self.key.sign(signed.encode()))

    def sign(self, **overrides) -> str:
        now = int(time.time())
        claims = {"typ": assertion.TYPE, "aud": "alice", "leg": "exec", "htm": "GET",
                  "htp": "/api/plugins/device/ws", "sub": "user-1", "device_id": "d_8f2c",
                  "device_name": "desktop", "iat": now, "exp": now + 30, "nonce": _b64(uuid_bytes())}
        claims.update(overrides)
        return self.raw(json.dumps(claims).encode())


def uuid_bytes() -> bytes:
    import os
    return os.urandom(16)


WANT = assertion.Expect(audience="alice", leg="exec", method="GET", path="/api/plugins/device/ws")


class GoVectorTests(unittest.TestCase):
    def test_verify_vectors_match_go(self):
        keys = assertion.parse_public_keys(VECTORS["public_key"])
        e = VECTORS["expect"]
        want = assertion.Expect(audience=e["audience"], leg=e["leg"], method=e["method"], path=e["path"])
        self.assertGreater(len(VECTORS["verify"]), 40)
        for v in VECTORS["verify"]:
            with self.subTest(v["name"]):
                verifier = assertion.Verifier(keys, now_ns=lambda v=v: v["now_ns"])
                try:
                    verifier.verify(v["encoded"], want)
                    got = "ok"
                except assertion.Refused as exc:
                    got = exc.kind
                self.assertEqual(got, v["result"])

    def test_key_vectors_match_go(self):
        for v in VECTORS["keys"]:
            with self.subTest(v["name"]):
                try:
                    got, count = "ok", len(assertion.parse_public_keys(v["text"]))
                except assertion.Refused as exc:
                    got, count = exc.kind, 0
                self.assertEqual((got, count), (v["result"], v["count"]))


class VerifierTests(unittest.TestCase):
    def setUp(self):
        self.signer = Signer()
        self.verifier = assertion.Verifier([self.signer.public])

    def test_valid_returns_claims(self):
        claims = self.verifier.verify(self.signer.sign(), WANT)
        self.assertEqual((claims.device_id, claims.device_name, claims.sub), ("d_8f2c", "desktop", "user-1"))

    def test_replay_refused_and_nonce_only_recorded_on_success(self):
        token = self.signer.sign()
        nonce = json.loads(base64.urlsafe_b64decode(token.split(".")[1] + "==="))["nonce"]
        with self.assertRaises(assertion.Refused) as ctx:
            self.verifier.verify(token, assertion.Expect("bob", "exec", "GET", "/api/plugins/device/ws"))
        self.assertEqual(ctx.exception.kind, "claims")
        self.verifier.verify(token, WANT)
        with self.assertRaises(assertion.Refused) as ctx:
            self.verifier.verify(token, WANT)
        self.assertEqual(ctx.exception.kind, "replay")
        self.assertIn(nonce, self.verifier._nonces)

    def test_nonces_expire(self):
        now = [int(time.time()) * _NS]
        v = assertion.Verifier([self.signer.public], now_ns=lambda: now[0])
        iat = now[0] // _NS
        v.verify(self.signer.sign(iat=iat, exp=iat + 10, nonce="N" * 22), WANT)
        now[0] += 16 * _NS
        v.verify(self.signer.sign(iat=iat + 16, exp=iat + 26, nonce="M" * 22), WANT)
        self.assertNotIn("N" * 22, v._nonces)
        v.verify(self.signer.sign(iat=iat + 16, exp=iat + 26, nonce="N" * 22), WANT)

    def test_no_keys(self):
        with self.assertRaises(assertion.Refused) as ctx:
            assertion.Verifier([]).verify(self.signer.sign(), WANT)
        self.assertEqual(ctx.exception.kind, "no_keys")

    def test_other_key_refused(self):
        other = Signer(bytes(32))
        with self.assertRaises(assertion.Refused) as ctx:
            assertion.Verifier([other.public]).verify(self.signer.sign(), WANT)
        self.assertEqual(ctx.exception.kind, "signature")

    def test_any_configured_key_verifies(self):
        other = Signer(bytes(32))
        assertion.Verifier([other.public, self.signer.public]).verify(self.signer.sign(), WANT)

    def test_length_counts_bytes(self):
        token = self.signer.sign(device_name="é" * 32)
        self.verifier.verify(token, WANT)
        padded = token.encode() + b"\xc3\xa9" * ((assertion.MAX_ENCODED_LEN - len(token)) // 2 + 1)
        with self.assertRaises(assertion.Refused) as ctx:
            self.verifier.verify(padded, WANT)
        self.assertEqual(ctx.exception.kind, "malformed")

    def test_raw_header_bytes_accepted(self):
        self.verifier.verify(self.signer.sign().encode(), WANT)

    def test_empty_audience_refuses(self):
        with self.assertRaises(assertion.Refused):
            self.verifier.verify(self.signer.sign(aud=""), assertion.Expect("", "exec", "GET", "/api/plugins/device/ws"))


if __name__ == "__main__":
    unittest.main()
