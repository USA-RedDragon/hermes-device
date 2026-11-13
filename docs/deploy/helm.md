# Install on Kubernetes with Helm

The chart in `deploy/charts/talaria` runs the same parts as the compose install: the hub, the agent, the sandbox, and SearXNG for the agent's web search. It makes every key it needs on the first install.

It is built on the [bjw-s common library](https://bjw-s-labs.github.io/helm-charts/docs/common-library/) (a copy is in `charts/`, so a plain checkout installs). Its own values stay short; the chart maps them onto the library's controllers, services, volumes and routes.

## What you need

- Kubernetes 1.29 or newer, `kubectl` and Helm 3.18 or newer (Helm 4 works).
- For editors (`talaria acp`): nothing more. On Kubernetes 1.35 or newer the chart mounts the talaria image as a volume for `acp-host`. On older clusters an init container copies the binary into the pod instead. On 1.33 or 1.34 with the `ImageVolume` feature gate on, set `hermes.editorBridge.binary=image`; `copy` forces the copy. To turn the editor bridge off, set `hermes.editorBridge.enabled=false`; the hub then tells editors it has no editor bridge.
- A default StorageClass, or set `*.persistence.storageClass`. The agent needs 20 GiB by default.
- For people outside the cluster: an Ingress controller or a Gateway API gateway, and a host name.

## Quick start (try it, with port-forward)

1. Install the chart and open a port to it:

   ```bash
   curl -fsSL https://github.com/USA-RedDragon/talaria/archive/refs/heads/main.tar.gz \
     | tar -xz --strip-components=3 talaria-main/deploy/charts
   helm install talaria ./talaria --namespace talaria --create-namespace \
     --set publicUrl=http://localhost:8080 --set demo.enabled=true
   kubectl -n talaria rollout status deploy/talaria-hermes --timeout=10m
   kubectl -n talaria port-forward svc/talaria 8080:8080
   ```

2. Open http://localhost:8080 right away: the setup page makes the first account, an admin. The first person who
   opens it gets that account, so do it before the hub is reachable from anywhere else. Add other people under
   Settings, Users ([Set up logins](auth.md)).
3. **Connect a model.** This step is required: until you do it, the chat area says Talaria has no model yet.
   Talaria includes its own agent, the part that talks to models, so you need nothing else.

   As the admin, click **Connect a model** in the chat area (it shows until a model is connected; later it is
   in Settings, Models) and pick where the model runs: a server of your own (Ollama, LM Studio, vLLM, llama.cpp),
   a provider with an API key, or a subscription sign-in. Talaria checks the address or key through the agent, lets
   you pick the model new chats start with, and keeps nothing itself: the agent stores the provider and key.
   [Choosing models and providers](models.md) walks through each kind.

   Or run the agent's own setup command in the agent's pod (the Deployment is
   named `<release>-hermes`, and its container `gateway`; here the release is `talaria`):

   ```bash
   kubectl -n talaria exec -it deploy/talaria-hermes -c gateway -- hermes model
   ```

   You will see a list of providers; move with the arrow keys. Pick one and give it what it asks for (an API
   key, a sign-in, or an address), or `talaria-demo` to look around (the `demo.enabled=true` above). For Ollama,
   LM Studio or vLLM, pick **Custom endpoint**, the last entry. It asks for a display name at the end: it
   becomes the group your models show under in the chat's model menu.

   Then open a new chat (or reload the page) and pick the model in the chat's model menu. No restart is needed,
   and models you pull into Ollama later appear on their own. Details: [Choosing models and providers](models.md).

## With a host name

```bash
helm upgrade --install talaria ./talaria --namespace talaria --create-namespace \
  --set publicUrl=https://talaria.example.com \
  --set ingress.enabled=true --set ingress.className=nginx --set ingress.tlsSecret=talaria-tls \
```

Then make your account and connect a model, as in steps 2 and 3 of the quick start. For Gateway
API, use `httpRoute.enabled=true` and `httpRoute.parentRefs` instead of the Ingress. The routes use the newest
`HTTPRoute` version the cluster serves; for `helm template` without a cluster, add
`--api-versions gateway.networking.k8s.io/v1/HTTPRoute`. The chart makes two routes:
`<release>-device` for `/api/device/` (device WebSockets, no request timeout) and `<release>` for the rest (24
hours, for server-sent events). An authenticating policy belongs on `<release>` only ([Login](auth.md)).

With `auth.mode=oidc` and a provider whose certificate comes from a private CA (your own, or step-ca), give the
hub that CA with `--set-file hub.caBundle=ca.crt`.

## What runs

| Deployment | Containers | What they do |
| ---------- | ---------- | ------------ |
| `<release>-hub` | `hub` | The hub: the web app, its API and the device connections, on the Service `<release>` (port 8080) and `<release>-hub-internal` (8081, for the sandbox). It flushes each chat event as it writes it, so no proxy sits in front of it in the pod. |
| `<release>-hermes` | `gateway`, `acp-host` (init: `plugin`, `device-bin` below 1.35, `init`) | `gateway` is the agent; its Webapp listener (port 9120) serves every agent route the hub uses: chats, models, settings and Connect a model. `acp-host` runs one `hermes acp` per editor connection (`hermes.editorBridge`). The agent's pod runs no Hermes dashboard. |
| `<release>-sandbox` | `sandbox` | Where the agent runs commands for chats that are on none of your computers. |
| `<release>-searxng` | `searxng` | The agent's web search (`searxng.enabled`). |
| `<release>-demo-llm` | `demo-llm` | The stand-in model (`demo.enabled`). |

## Zero-downtime updates

With `hermes.rolling.enabled` (the default), an upgrade or a restart of the agent loses nothing:

1. The new pod starts on the same node (pod affinity, because the volume is ReadWriteOnce) and its gateway waits
   as a standby for the old one's lock.
2. The old pod's `preStop` waits until the standby is warm (up to 2 minutes), then the old gateway gets SIGTERM
   and opens its restart wait: running turns finish (up to 9 minutes), approvals stay answerable, new messages
   wait for the new pod.
3. The old gateway hands its Webapp listener over and relays connections until the new gateway is ready. The
   hub holds messages for up to 10 minutes and reconnects by itself.

This needs Talaria's agent image (the default) and the settings the chart writes into the agent's config
(`TALARIA_ROLLING` in `hermes-init.py`). `terminationGracePeriodSeconds` is 780 seconds. With a ReadWriteMany
volume, set `hermes.persistence.accessMode=ReadWriteMany` and the pod affinity is left out.

The hub with SQLite (the default) is one pod that is replaced (`Recreate`), which takes a few seconds; the
browser reconnects. For a hub with no gap, use PostgreSQL (`hub.store.driver=postgres`,
`hub.store.existingSecret` with key `dsn`) and `hub.replicas=2`: the chart then uses a
rolling update with `maxUnavailable: 0` and a PodDisruptionBudget.

The sandbox is one pod that is replaced: a second sandbox would take over the first one's connection.

## Web search

`searxng.enabled` (the default) runs SearXNG as `<release>-searxng`, reachable only inside the cluster, and points
the agent's web search at it while no other search provider is chosen in the agent. Turn it off with
`--set searxng.enabled=false`. Details: [Web search](reference.md#web-search).

## Upgrade from chart 0.2.x

Chart 0.3.0 drops the Caddy container that sat in front of the hub in its pod: the hub flushes chat streams itself.
Nothing else moves, so a plain `helm upgrade` keeps every volume, Secret and Service. The hub's pod is replaced
once, and the agent hands over as in a rolling update.

The hub now sees your ingress controller or gateway as the client, not a proxy on `127.0.0.1`. `auth.trustedProxies`
therefore defaults to the private address ranges of pods (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`fc00::/7`). Upgrade with your own values file, not `--reuse-values`, which would keep the old `127.0.0.1/32`.
With `auth.mode=proxy` and plain headers, set it to the addresses of your proxy if you can, as the hub reads the
user header only from them.

## Keys and secrets

On the first install the chart makes the hub's Ed25519 signing key, the Web Push key, the session secret, the
password the hub uses with the agent, the sandbox token and the SearXNG key, and keeps
them in the Secret `<release>-keys`. Upgrades read them back, so they never change. Uninstall leaves that Secret
and the volumes in place (`helm.sh/resource-policy: keep`); delete them by hand to start over.

`helm template` and `--dry-run` cannot read the cluster, so they show new random keys each time. That is
expected.

## Values

Every value of the chart is listed, with its comment, in the [settings reference](reference.md#helm-values).
