#!/usr/bin/env python3
"""End-to-end check of a Talaria install, with Python's standard library only.

Logs in with a hub account (making it on the setup page when the hub has none yet), starts a chat, sends a message and waits for the reply, then asks
the demo model to run a command and checks that it ran in the sandbox.

  python3 deploy/hack/smoke.py http://localhost:8080 alice '<password of 12 or more characters>'

Exit 0 when every step passed. Works with the demo model (profile demo); a real model will not follow the
"run:" convention reliably, so with a real model add --real: it only waits for any reply.
"""

import http.cookiejar
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def main():
    base, name, password = sys.argv[1].rstrip("/"), sys.argv[2], sys.argv[3]
    real = "--real" in sys.argv[4:]
    real_timeout = 900
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

    def step(name):
        print(f"[{time.strftime('%H:%M:%S')}] {name}", flush=True)

    def post(path, body):
        req = urllib.request.Request(base + path, data=json.dumps(body).encode(), method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Origin", base)
        try:
            with op.open(req) as resp:
                return resp.status, resp.read().decode()
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode()

    r = op.open(base + "/")
    r.read()
    if urllib.parse.urlparse(r.geturl()).path == "/setup":
        step(f"setup: make the first account, {name}")
        code, text = post("/auth/setup", {"name": name, "display": name.capitalize(), "password": password})
        if code != 201:
            sys.exit(f"setup failed: {code} {text}")
    step("log in")
    code, text = post("/auth/password", {"name": name, "password": password})
    if code != 200:
        sys.exit(f"login failed: {code} {text}")

    def api(method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(base + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        req.add_header("Origin", base)
        if csrf:
            req.add_header("X-CSRF-Token", csrf)
        with op.open(req) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None

    csrf = None
    me = api("GET", "/api/me")
    csrf = me["csrf"]
    step(f"logged in as {me['user']}")

    def wait_for(sess, needle, timeout=180):
        end = time.time() + timeout
        while time.time() < end:
            t = api("GET", f"/api/sessions/main/{sess}/transcript")
            if needle in json.dumps(t):
                return True
            time.sleep(2)
        return False

    step("new chat")
    s = api("POST", "/api/sessions", {"backend_id": "main"})
    sess = s.get("id") or s.get("session_id")
    if real:
        # A real model: any reply will do
        step(f"chat {sess}: ask a question (real model, up to {real_timeout} s)")
        api("POST", f"/api/sessions/main/{sess}/prompt", {"text": "Reply with one short sentence: what is 2+2?"})
        end = time.time() + real_timeout
        while time.time() < end:
            t = api("GET", f"/api/sessions/main/{sess}/transcript")
            msgs = [m for m in (t.get("messages") or []) if m.get("role") == "assistant" and (m.get("content") or m.get("text"))]
            if msgs:
                step("reply: " + str(msgs[-1].get("content") or msgs[-1].get("text"))[:200].replace("\n", " "))
                print("SMOKE OK")
                return
            time.sleep(3)
        sys.exit("no reply from the model: " + json.dumps(t)[:600])
    step(f"chat {sess}: say hello")
    api("POST", f"/api/sessions/main/{sess}/prompt", {"text": "hello from the smoke test"})
    if not wait_for(sess, "Talaria demo model"):
        sys.exit("no reply from the model")
    step("reply arrived")
    step("run a command in the sandbox")
    api("POST", f"/api/sessions/main/{sess}/prompt", {"text": "run: echo smoke-$((40+2)) && pwd"})
    if not wait_for(sess, "smoke-42"):
        sys.exit("the command did not run")
    if not wait_for(sess, "/work/", 30):
        sys.exit("the command ran, but not in the sandbox's /work")
    step("command ran in the sandbox")
    print("SMOKE OK")


if __name__ == "__main__":
    main()
