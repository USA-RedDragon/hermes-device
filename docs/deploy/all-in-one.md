# Install the all-in-one image

Unraid and TrueNAS install apps as one container with a few settings and folders. The all-in-one image puts the
hub, the agent, the sandbox and the hub's own accounts in one container, so those platforms need nothing else. On any
Docker host it is also the shortest way to try Talaria:

```bash
docker run -d --name talaria -p 8080:8080 \
  -e TALARIA_PUBLIC_URL=http://localhost:8080 -e TALARIA_DEMO=true \
  -v talaria-hermes:/opt/data -v talaria-data:/talaria \
  ghcr.io/usa-reddragon/talaria-aio:latest
docker logs talaria 2>&1 | grep -A3 Open:
```

Then:

1. Open the address it prints, right away, and make your account: the first visitor becomes the admin.
2. **Connect a model.** This step is required: until you do it, the chat area says Talaria has no model yet.
   Talaria includes its own agent, the part that talks to models, so you need nothing else.

   As the admin, click **Connect a model** in the chat area (it shows until a model is connected; later it is
   in Settings, Models) and pick where the model runs: a server of your own (Ollama, LM Studio, vLLM, llama.cpp),
   a provider with an API key, or a subscription sign-in. Talaria checks the address or key through the agent, lets
   you pick the model new chats start with, and keeps nothing itself: the agent stores the provider and key.
   [Choosing models and providers](models.md) walks through each kind.

   Or run the agent's own setup command in the container, from a terminal on the server:

   ```bash
   docker exec -it -u hermes talaria hermes model
   ```

   You will see a list of providers; move with the arrow keys. Pick one and give it what it asks for (an API
   key, a sign-in, or an address), or `talaria-demo`, which the `TALARIA_DEMO=true` above turned on. For Ollama,
   LM Studio or vLLM, pick **Custom endpoint**, the last entry. It asks for a display name at the end: it
   becomes the group your models show under in the chat's model menu.

   Then open a new chat (or reload the page) and pick the model in the chat's model menu. No restart is needed,
   and models you pull into Ollama later appear on their own. Details: [Choosing models and providers](models.md).

The image is built from `deploy/aio/Dockerfile` for amd64 and arm64 and published on each release (see
[Images](images.md)). To build it yourself from a checkout: `docker build -f aio/Dockerfile -t talaria-aio .`.

## Design

The image is the Talaria agent image with three static binaries copied in (the hub, `talaria` and Caddy), plus
SearXNG for web search (its code and the same package versions as the upstream image, about 135 MB).
The agent image already ships s6-overlay as its process supervisor, so the image adds its services to it instead of
bringing a second supervisor.

| Part | How it runs | User |
| ---- | ----------- | ---- |
| Setup | `/etc/cont-init.d/10-talaria`, once per start, after the agent's own setup: the same `init.sh` and `hermes-init.py` as the compose install | root, then hermes |
| Agent gateway | The container's command (`gateway run`), as in the plain agent image | hermes (`PUID`) |
| Hub | s6 service `talaria-hub`, on 127.0.0.1:8081 | nobody |
| Front | s6 service `talaria-front` (Caddy), the one port inside the container: `TALARIA_PORT` (8080). Publish it on any host port. Until the agent answers its health check, it holds requests (up to a minute, then a page that reloads itself) | nobody |
| Sandbox | s6 service `talaria-sandbox`, `talaria run` as a service device | agent (2568) |
| Web search | s6 service `talaria-searxng`: SearXNG on 127.0.0.1:8888, for the agent only; off with `TALARIA_SEARXNG=false` ([Web search](reference.md#web-search)) | searxng (977) |
| Demo model | s6 service `talaria-demo`, only with `TALARIA_DEMO=true`, offered to the agent as the provider `talaria-demo` | nobody |

Two folders hold all state: `/opt/data` is the agent's data folder (chats, memory, settings), and `/talaria` holds the
hub database, the generated keys (`/talaria/secrets`) and the sandbox's files (`/talaria/work`).

Settings are environment variables with the same names as in the compose `.env`: `TALARIA_PUBLIC_URL`,
`TALARIA_PORT`, `TALARIA_USER`,
`TALARIA_TELEGRAM_TOKEN`, `TZ`, `TALARIA_SEARXNG`, plus `TALARIA_DEMO`, and the agent image's own `PUID` and
`PGID`. Login settings are the hub's own `HUB_AUTH_*` and `HUB_OIDC_*` variables ([Set up logins](auth.md)); the
default is local accounts, made on the setup page by the first visitor. `TALARIA_INSTALL` (`aio` when unset; the
Unraid template sets `unraid` and the TrueNAS file `truenas`) tells the hub which commands to show for this install.

The sandbox runs as its own user. It cannot read the agent's data folder (mode 700, owned by hermes) or Talaria's keys
(each part's folder is readable only by its user), and it cannot read other processes' environments. This was
checked in the image: as the sandbox user, `ls /opt/data`, `cat /talaria/secrets/hub/config.yaml` and
`cat /proc/1/environ` all fail with permission denied.

## Trade-offs against compose

| | Compose (or Helm) | All-in-one |
| - | ----------------- | ---------- |
| Setup | A folder with `compose.yaml` and `.env` | One container, a form in the NAS UI |
| Sandbox isolation | Its own container: own filesystem, process and network namespaces, read-only root | Its own user in the same container: same filesystem view (minus permissions), same network, sees other processes' names |
| Sandbox tools | Alpine image with build tools, git, node, python | The agent image's Debian, which has python, node, git and gcc |
| Upgrades | Each part separately | All parts at once: one image tag per Talaria release |
| A crash | Docker restarts that container | s6 restarts that service; a gateway crash restarts the container |
| Size | Same images, about 1.2 GB to pull | One image, about the same size (the three extra binaries are about 70 MB) |
| Memory | About 300 MB idle | About 380 MB idle (measured) |
| Optional parts (Ollama, Honcho) | Profiles in the same file | Separate apps, pointed at by URL |
| Several agent servers | Possible (edit the hub config) | One |

Use the all-in-one image when the platform wants one container, or to try Talaria. Use compose or Helm when you
want the stronger sandbox, separate upgrades, or optional parts in the same file.

## Building and publishing (not done yet)

- One multi-arch image (amd64, arm64) per Talaria release, tagged `vX.Y.Z` and `latest`, built from pinned
  digests of the parts, signed like the other images.
- When the agent image bakes in the device plugin and Talaria's settings, the setup step shrinks to the
  secrets and configs.
