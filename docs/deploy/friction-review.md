# Friction review: a fresh VM, following only these docs

Date: 2026-11-13. Kit commit: the one that added this page.

Setup: a Debian 13 cloud image in QEMU (6 vCPU, 12 GB RAM, user-mode networking, no GPU). Docker from
get.docker.com (Docker 29.9, Compose 5.7). For Helm: kind v0.33.0 (Kubernetes 1.37), kubectl 1.37, Helm 3.22.
Times are wall-clock seconds per step. "Chat" is `hack/smoke.py`, which does what a person does in the browser:
log in through the built-in login, open a chat, send a message, wait for the reply, and with the demo model send
`run: ...` and check the command ran in the sandbox. No browser was used, and no real API keys.

Two steps are not in the docs, because the repository and the Hermes image were private at the time (the images
are public now, as `talaria-hermes` and `talaria-aio`): the files came
from a local copy of the repository archive instead of GitHub, and the Hermes image was loaded from a file
(`docker load`, `kind load docker-image`) and named by tag with `TALARIA_HERMES_IMAGE` / `images.hermes`.

## Docker Compose

| Step | Pass 1 | Pass 2 |
| ---- | ------ | ------ |
| Install Docker (get.docker.com) | 75 s | (done) |
| Load the private Hermes image (not in docs) | 44 s | (done) |
| Step 1: get the files, copy `.env` | 0 s | 1 s |
| Point `TALARIA_HERMES_IMAGE` at the loaded image | 0 s (not in docs) | 0 s (documented in `.env`) |
| Step 2: `docker compose up -d --wait` (public images pulled fresh) | 26 s | 24 s |
| Step 3: `docker compose logs init`, log in, chat, command in sandbox | 8 s | 8 s |
| Step 4: switch to Ollama, apply | 96 s, then a failed chat, a model change (23 s) and a chat (86 s) | 93 s, demo removed |
| **From the files to a working chat** | **34 s** | **33 s** |

Snags in pass 1, and what changed:

1. The docs did not say how to install Docker, nor that the user needs the `docker` group (and a new login)
   to run `docker` without `sudo`. Added both to "What you need".
2. The Hermes image is private (expected). The compose file pins it by digest, so an image loaded from a file
   does not match. `TALARIA_HERMES_IMAGE` existed but only `docs/images.md` named it. It is now in
   `.env.example`, and Step 1 points to it.
3. `docker compose up -d --wait --remove-orphans` did not stop the demo model: a service of a profile that is
   off is not an orphan. Step 4 now uses `docker compose rm -sf demo-llm`.
4. The Ollama example model (`qwen3:8b`, and `qwen3:0.6b` in the test) has a 40K window in Ollama, and Hermes
   refuses windows under 64K. Hermes said so in the chat, clearly. The examples now use `llama3.2:3b`, and the
   guide says to pick a 64K-capable model.
5. A 1B local model on 6 CPU cores answered in 80 s, used 5.5 GB of RAM with the 128K window, and wrote a tool
   call as text. The guide now says this plainly.

Pass 2 had no snags.

Found while building the kit (before the review), fixed in the kit:

- The hub refuses to start when `http.public_url` is `http://...`: `notify.vapid_subject` defaults to it and
  must be `mailto:` or `https:`. The kit sets `vapid_subject: mailto:<admin email>`. Hub fix: default to
  nothing, or skip Web Push, when the public URL is not https.
- The device plugin could not hide `browser_vault`: Hermes v0.23.1's tool registry still reads
  `plugins.entries.device.allow_tool_override`, not only `granted_capabilities`. The kit sets both.
- With the built-in login, the hub must reach `<public URL>/dex` itself, so the container port must equal the
  host port when the public URL is `http://localhost:<port>`. The kit publishes `<port>:<port>`.
- The Hermes image's entrypoint starts a gateway in any container whose last state was "running", including a
  one-shot setup container on the same volume. The kit runs its setup script directly, not through the
  entrypoint.

## Helm (kind in the VM)

| Step | Pass 1 | Pass 2 |
| ---- | ------ | ------ |
| Install kind, kubectl, Helm | about 20 s (two retries of my own install line) | (done) |
| `kind create cluster` | 18 s | (done) |
| Load the private Hermes image into kind (not in docs) | 84 s | (done) |
| Get the chart | failed: wrong `--strip-components` | 0 s |
| `helm install` | 1 s | 0 s |
| Rollout of Hermes and the hub | 26 s, hub in CrashLoopBackOff | 15 s |
| Port-forward, password, log in and chat | after two chart fixes: 5 s | 10 s |
| **From the chart to a working chat** | **about 6 min, with fixes** | **25 s** |

Snags in pass 1, and what changed:

1. The chart download command extracted `charts/talaria` instead of `talaria`, so `helm install ./talaria`
   failed. Fixed `--strip-components`.
2. The front container (Caddy) failed with `exec /usr/bin/caddy: operation not permitted`: the binary carries
   the file capability `cap_net_bind_service`, and the chart dropped all capabilities with no privilege
   escalation. The chart now keeps `NET_BIND_SERVICE` for that container.
3. The login container never became ready: an `httpGet` probe goes to the pod address, and Dex listens on
   127.0.0.1 only. The probe now runs `wget` inside the container.

## Zero-downtime update (Helm)

`kubectl rollout restart deploy/talaria-hermes` while a pod polled the Hermes Service every 0.25 s and three
chats ran through the hub:

- The rollout took 16 s (37 s on the first try). The new pod started next to the old one, and the Service
  answered **942 of 942** health checks: no gap.
- Two of three chats passed. In one, the hub answered a transcript read with 502: Hermes refused it with
  `gateway error 4001: runtime_draining` during the old gateway's restart wait, and the hub passed the error
  on. Hub fix: hold or retry read-only calls such as `session.transcript` through a drain, as it already does
  for other read-only calls.

## Not tested

- A real browser, the Telegram bot, Web Push, pairing a computer.
- The `honcho` and `honcho-embeddings` profiles (the compose file validates; the services were not started).
- Unraid and TrueNAS themselves. The all-in-one image was built and passed the same chat check on Docker, with
  the default user and with `PUID=568`.
- HTTPS with Caddy, an Ingress or a Gateway API route.
- The hub with PostgreSQL and 2 replicas.
