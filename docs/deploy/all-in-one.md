# The all-in-one image

Unraid and TrueNAS install apps as one container with a few settings and folders. The all-in-one image puts the
hub, Hermes, the sandbox and the built-in login in one container, so those platforms need nothing else. On any
Docker host it is also the shortest way to try Talaria:

```bash
docker run -d --name talaria -p 8080:8080 \
  -e TALARIA_PUBLIC_URL=http://localhost:8080 -e TALARIA_DEMO=true \
  -v talaria-hermes:/opt/data -v talaria-data:/talaria \
  ghcr.io/usa-reddragon/talaria-aio:latest
docker logs talaria 2>&1 | grep -A3 Open:
```

The image is built from `aio/Dockerfile` for amd64 and arm64 and published on each release (see
[Images](images.md)). To build it yourself from a checkout: `docker build -f aio/Dockerfile -t talaria-aio .`.

## Design

The image is the Talaria Hermes image with four static binaries copied in (the hub, `talaria`, Caddy and Dex).
Hermes already ships s6-overlay as its process supervisor, so the image adds its services to it instead of
bringing a second supervisor.

| Part | How it runs | User |
| ---- | ----------- | ---- |
| Setup | `/etc/cont-init.d/10-talaria`, once per start, after Hermes's own setup: the same `init.sh` and `hermes-init.py` as the compose install | root, then hermes |
| Hermes gateway | The container's command (`gateway run`), as in the plain Hermes image | hermes (`PUID`) |
| Hub | s6 service `talaria-hub`, on 127.0.0.1:8081 | nobody |
| Front | s6 service `talaria-front` (Caddy), the one port: `TALARIA_PORT` (8080) | nobody |
| Login | s6 service `talaria-dex`, on 127.0.0.1:5556 | nobody |
| Sandbox | s6 service `talaria-sandbox`, `talaria run` as a service device | agent (2568) |
| Demo model | s6 service `talaria-demo`, only with `TALARIA_DEMO=true` | nobody |

Two folders hold all state: `/opt/data` is the Hermes home (chats, memory, settings), and `/talaria` holds the
hub database, the generated keys (`/talaria/secrets`) and the sandbox's files (`/talaria/work`).

Settings are environment variables with the same names as in the compose `.env`: `TALARIA_PUBLIC_URL`,
`TALARIA_PORT`, `TALARIA_MODEL_BASE_URL`, `TALARIA_MODEL`, `TALARIA_MODEL_API_KEY`, `TALARIA_ADMIN_EMAIL`,
`TALARIA_ADMIN_PASSWORD`, `TALARIA_TELEGRAM_TOKEN`, `TZ`, plus `TALARIA_DEMO`, and Hermes's own `PUID` and
`PGID`.

The sandbox runs as its own user. It cannot read the Hermes home (mode 700, owned by hermes) or Talaria's keys
(each part's folder is readable only by its user), and it cannot read other processes' environments. This was
checked in the image: as the sandbox user, `ls /opt/data`, `cat /talaria/secrets/hub/config.yaml` and
`cat /proc/1/environ` all fail with permission denied.

## Trade-offs against compose

| | Compose (or Helm) | All-in-one |
| - | ----------------- | ---------- |
| Setup | A folder with `compose.yaml` and `.env` | One container, a form in the NAS UI |
| Sandbox isolation | Its own container: own filesystem, process and network namespaces, read-only root | Its own user in the same container: same filesystem view (minus permissions), same network, sees other processes' names |
| Sandbox tools | Alpine image with build tools, git, node, python | The Hermes image's Debian, which has python, node, git and gcc |
| Upgrades | Each part separately | All parts at once: one image tag per Talaria release |
| A crash | Docker restarts that container | s6 restarts that service; a gateway crash restarts the container |
| Size | Same images, about 1.2 GB to pull | One image, about the same size (the four extra binaries are about 100 MB) |
| Memory | About 300 MB idle | About 380 MB idle (measured) |
| Optional parts (Ollama, Honcho) | Profiles in the same file | Separate apps, pointed at by URL |
| Several Hermes backends | Possible (edit the hub config) | One |

Use the all-in-one image when the platform wants one container, or to try Talaria. Use compose or Helm when you
want the stronger sandbox, separate upgrades, or optional parts in the same file.

## Ports must match

The built-in login lives at `<TALARIA_PUBLIC_URL>/dex`, and the hub reaches it there too. So the hub must be able
to open `TALARIA_PUBLIC_URL` from inside the container:

- `http://<NAS address>:<host port>`: works, through the host.
- `https://talaria.example.com` behind a reverse proxy: works when the container can resolve that name.
- `http://localhost:<port>`: only when the container listens on the same port. Set `TALARIA_PORT` to that port
  and publish it as `<port>:<port>`.

This goes away with the hub's own accounts, which need no login service.

## Building and publishing (not done yet)

- One multi-arch image (amd64, arm64) per Talaria release, tagged `vX.Y.Z` and `latest`, built from pinned
  digests of the four parts, signed like the other images.
- When the Hermes image bakes in the device plugin and Talaria's settings, the setup step shrinks to the
  secrets and configs.
