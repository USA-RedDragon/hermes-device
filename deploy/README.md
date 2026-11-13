# Talaria install kit

Install Talaria on your own server: the hub (web app and Telegram bot, with its own accounts), the agent (Hermes Agent with
the Talaria patches), and a sandbox for the agent's commands. Every secret and key is made for you on the first
start; the first person who opens Talaria makes the admin account.

Talaria is built on Hermes Agent by Nous Research (MIT). Not affiliated with or endorsed by Nous Research.

| You have | Use | Guide |
| -------- | --- | ----- |
| A Linux host with Docker | Docker Compose, [`compose/`](compose) | [Docker Compose](../docs/deploy/compose.md) |
| A Kubernetes cluster | Helm chart [`charts/talaria`](charts/talaria) | [Kubernetes](../docs/deploy/helm.md) |
| Unraid | All-in-one container, [`aio/unraid`](aio/unraid) | [Unraid](../docs/deploy/unraid.md) |
| TrueNAS SCALE 24.10 or later | All-in-one container, [`aio/truenas`](aio/truenas) | [TrueNAS](../docs/deploy/truenas.md) |

## Quick start (Docker Compose)

```bash
mkdir talaria && cd talaria
curl -fsSL https://github.com/USA-RedDragon/talaria/archive/refs/heads/main.tar.gz \
  | tar -xz --strip-components=3 talaria-main/deploy/compose
cp .env.example .env
docker compose up -d --wait
docker compose logs init
```

Open the address it prints right away and make your account on the setup page (the first visitor becomes the
admin). Then click **Connect a model** in the chat area, or pick a model and provider with the agent's own setup
command:

```bash
docker compose exec -u hermes hermes hermes model
```

To look around without a provider, set `COMPOSE_PROFILES=demo` in `.env`, run `docker compose up -d --wait` and
pick `talaria-demo` there ([Step 4](../docs/deploy/compose.md#step-4-connect-a-model)).

All guides: https://usa-reddragon.github.io/talaria/

## What is here

- `compose/`: the Docker Compose install and its setup scripts (`compose/files`).
- `charts/talaria/`: the Helm chart. It carries copies of `compose/files/hermes-init.py` and `demo-llm.py`;
  `hack/check-sync.sh` checks that they match.
- `aio/`: the all-in-one image (`aio/Dockerfile`, build context `deploy/`), with the Unraid template and the
  TrueNAS app file. A tag `aio/vX.Y.Z` publishes `ghcr.io/usa-reddragon/talaria-aio:vX.Y.Z`.
- `hack/smoke.py`: logs in, chats and runs a command in the sandbox against a running install with the demo
  model.

The settings reference in [docs/deploy/reference.md](../docs/deploy/reference.md) is generated from these files
by docgen; run `go run github.com/USA-RedDragon/docgen@<version in .github/workflows/docs.yaml> update` at the
repository root after you change them.
