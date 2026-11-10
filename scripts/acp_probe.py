"""Scripted ACP client: what an editor bridge on the device would do.

1. Start `hermes-acp` (it attaches to the home's gateway).
2. session/new with the server placeholder as cwd (the editor's real folder lives on the device).
3. Send "/device <name> <dir>" as a prompt; ACP dispatches plugin commands.
4. session/prompt asking for pwd, and print what came back.

Usage: acp_probe.py <hermes-acp> <placeholder> <device> <device-dir>
Stdlib only.
"""
import json
import subprocess
import sys
import threading

acp, placeholder, device, device_dir = sys.argv[1:5]
proc = subprocess.Popen([acp], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=open("acp.stderr.log", "w"),
                        text=True, bufsize=1)
lock = threading.Lock()
pending: dict[int, dict] = {}
events: dict[int, threading.Event] = {}
updates: list[dict] = []
next_id = 0


def send(obj: dict) -> None:
    with lock:
        proc.stdin.write(json.dumps(obj) + "\n")
        proc.stdin.flush()


def reader() -> None:
    for line in proc.stdout:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            continue
        if "method" in msg and "id" in msg:  # request from agent
            if msg["method"] == "session/request_permission":
                opts = msg["params"].get("options") or []
                allow = next((o for o in opts if "allow" in o.get("kind", "")), opts[0] if opts else {"optionId": "allow"})
                send({"jsonrpc": "2.0", "id": msg["id"], "result": {"outcome": {"outcome": "selected", "optionId": allow["optionId"]}}})
            else:
                send({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": "not supported"}})
        elif "method" in msg:
            updates.append(msg)
        elif "id" in msg:
            pending[msg["id"]] = msg
            if msg["id"] in events:
                events[msg["id"]].set()


threading.Thread(target=reader, daemon=True).start()


def call(method: str, params: dict, timeout: float = 300) -> dict:
    global next_id
    next_id += 1
    rid = next_id
    events[rid] = threading.Event()
    send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    if not events[rid].wait(timeout):
        raise TimeoutError(method)
    return pending.pop(rid)


def text_of(start: int) -> str:
    out = []
    for u in updates[start:]:
        upd = (u.get("params") or {}).get("update") or {}
        kind = upd.get("sessionUpdate")
        if kind == "agent_message_chunk":
            out.append((upd.get("content") or {}).get("text", ""))
        elif kind in ("tool_call", "tool_call_update"):
            for c in upd.get("content") or []:
                inner = c.get("content") or {}
                if inner.get("type") == "text":
                    out.append(f"\n[tool] {inner.get('text', '')[:400]}\n")
    return "".join(out)


print("initialize:", json.dumps(call("initialize", {"protocolVersion": 1, "clientCapabilities": {
    "fs": {"readTextFile": False, "writeTextFile": False}, "terminal": False}}).get("result", {}))[:300])
new = call("session/new", {"cwd": placeholder, "mcpServers": []})
print("session/new:", json.dumps(new)[:400])
sid = new["result"]["sessionId"]

mark = len(updates)
r = call("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": f"/device {device} {device_dir}"}]})
print("slash /device via ACP:", json.dumps(r.get("result"))[:200], "|", text_of(mark)[:400].replace("\n", " "))

mark = len(updates)
r = call("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text":
    "Use the terminal tool to run exactly: echo $DEVICE_MARK; pwd. Reply with the raw output only."}]})
print("prompt:", json.dumps(r.get("result"))[:200])
print("text:", text_of(mark)[-800:])
proc.stdin.close()
proc.terminate()
proc.wait(timeout=20)
