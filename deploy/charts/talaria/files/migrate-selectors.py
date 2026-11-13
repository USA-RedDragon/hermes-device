"""Upgrade hook of the Talaria chart: replace Deployments made by chart 0.1.x.

Chart 0.2.0 renders its Deployments with the bjw-s common library, whose pod selector also has the label
app.kubernetes.io/controller. A Deployment's selector cannot change, so this deletes each old Deployment (the
volumes, Secrets and Services stay) and Helm makes the new one right after. It waits until the old hub and sandbox
pods are gone, so the new ones never share the SQLite file or the sandbox connection with them. The old agent pod
hands over to the new one as in a rolling update (with hermes.rolling.enabled) or is waited for too.

Arguments: the Deployment names, then --wait and the names whose old pods must be gone first.
"""

import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SA = "/var/run/secrets/kubernetes.io/serviceaccount"
LABEL = "app.kubernetes.io/controller"


def main():
    args = sys.argv[1:]
    names, wait = (args[: args.index("--wait")], args[args.index("--wait") + 1 :]) if "--wait" in args else (args, [])
    ns = open(f"{SA}/namespace").read().strip()
    token = open(f"{SA}/token").read().strip()
    ctx = ssl.create_default_context(cafile=f"{SA}/ca.crt")
    host = os.environ["KUBERNETES_SERVICE_HOST"]
    port = os.environ.get("KUBERNETES_SERVICE_PORT", "443")
    base = f"https://{host if ':' not in host else '[' + host + ']'}:{port}"

    def call(method, path, body=None):
        req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body else None)
        req.add_header("Authorization", f"Bearer {token}")
        req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, context=ctx, timeout=30) as resp:
                return resp.status, json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, {}

    old_selectors = {}
    for name in names:
        code, dep = call("GET", f"/apis/apps/v1/namespaces/{ns}/deployments/{name}")
        if code == 404:
            continue
        if code != 200:
            sys.exit(f"read deployment {name}: HTTP {code}")
        sel = dep["spec"]["selector"].get("matchLabels", {})
        if LABEL in sel:
            print(f"{name}: selector already current")
            continue
        old_selectors[name] = sel
        code, _ = call("DELETE", f"/apis/apps/v1/namespaces/{ns}/deployments/{name}",
                       {"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"})
        if code not in (200, 202, 404):
            sys.exit(f"delete deployment {name}: HTTP {code}")
        print(f"{name}: deleted, Helm makes it again with the new selector", flush=True)

    end = time.time() + 240
    for name in wait:
        sel = old_selectors.get(name)
        if not sel:
            continue
        query = ",".join([f"{k}={v}" for k, v in sel.items()] + [f"!{LABEL}"])
        while True:
            code, pods = call("GET", f"/api/v1/namespaces/{ns}/pods?labelSelector={urllib.parse.quote(query)}")
            left = [p["metadata"]["name"] for p in pods.get("items", [])] if code == 200 else ["?"]
            if not left:
                print(f"{name}: old pods gone", flush=True)
                break
            if time.time() > end:
                sys.exit(f"{name}: old pods still running: {left}")
            time.sleep(2)


if __name__ == "__main__":
    main()
