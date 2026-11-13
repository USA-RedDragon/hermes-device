# Install on Kubernetes with Helm

The chart in `deploy/charts/talaria` runs the same parts as the compose install: the hub (with its web front in one pod), Hermes, and the sandbox. It makes every key it needs on the first install.

## What you need

- Kubernetes 1.29 or newer, `kubectl` and Helm 3.14 or newer (Helm 4 works).
- For editors (`talaria acp`): Kubernetes 1.33 or newer, because the chart mounts the talaria image as a volume for `acp-host`. On an older cluster, set `hermes.editorBridge.enabled=false`; the hub then tells editors it has no editor bridge.
- A default StorageClass, or set `*.persistence.storageClass`. Hermes needs 20 GiB by default.
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
3. **Connect a model.** This step is required: until you do it, a chat says Hermes is not connected to any AI
   provider. Talaria includes its own Hermes, the agent that talks to models, so you do not need a separate
   Hermes, and an existing one is not used. Run Hermes's own setup command in the Hermes pod (the Deployment is
   named `<release>-hermes`, and its container `gateway`; here the release is `talaria`):

   ```bash
   kubectl -n talaria exec -it deploy/talaria-hermes -c gateway -- hermes model
   ```

   You will see a list of providers; move with the arrow keys. Pick one and give it what it asks for (an API
   key, a sign-in, or an address), or `talaria-demo` to look around (the `demo.enabled=true` above). For Ollama,
   LM Studio or vLLM, pick **Custom endpoint**, the last entry. Hermes asks for a display name at the end: it
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
API, use `httpRoute.enabled=true` and `httpRoute.parentRefs` instead of the Ingress. The chart makes two routes:
`<release>-device` for `/api/device/` (device WebSockets, no request timeout) and `<release>` for the rest (24
hours, for server-sent events). An authenticating policy belongs on `<release>` only ([Login](auth.md)).

With `auth.mode=oidc` and a provider whose certificate comes from a private CA (your own, or step-ca), give the
hub that CA with `--set-file hub.caBundle=ca.crt`.

## Zero-downtime updates

With `hermes.rolling.enabled` (the default), an upgrade or a restart of Hermes loses nothing:

1. The new pod starts on the same node (pod affinity, because the volume is ReadWriteOnce) and its gateway waits
   as a standby for the old one's lock.
2. The old pod's `preStop` waits until the standby is warm (up to 2 minutes), then the old gateway gets SIGTERM
   and opens its restart wait: running turns finish (up to 9 minutes), approvals stay answerable, new messages
   wait for the new pod.
3. The old gateway hands its Webapp listener over and relays connections until the new gateway is ready. The
   hub holds messages for up to 10 minutes and reconnects by itself.

This needs the patched Hermes image (the default) and the settings the chart writes into Hermes's config
(`TALARIA_ROLLING` in `hermes-init.py`). `terminationGracePeriodSeconds` is 780 seconds. With a ReadWriteMany
volume, set `hermes.persistence.accessMode=ReadWriteMany` and the pod affinity is left out.

The hub with SQLite (the default) is one pod that is replaced (`Recreate`), which takes a few seconds; the
browser reconnects. For a hub with no gap, use PostgreSQL (`hub.store.driver=postgres`,
`hub.store.existingSecret` with key `dsn`) and `hub.replicas=2`: the chart then uses a
rolling update with `maxUnavailable: 0` and a PodDisruptionBudget.

The sandbox is one pod that is replaced: a second sandbox would take over the first one's connection.

## Keys and secrets

On the first install the chart makes the hub's Ed25519 signing key, the Web Push key, the session secret, the
password the hub uses with Hermes and the sandbox token, and keeps
them in the Secret `<release>-keys`. Upgrades read them back, so they never change. Uninstall leaves that Secret
and the volumes in place (`helm.sh/resource-policy: keep`); delete them by hand to start over.

`helm template` and `--dry-run` cannot read the cluster, so they show new random keys each time. That is
expected.

## Values

Every value of the chart is listed, with its comment, in the [settings reference](reference.md#helm-values).
