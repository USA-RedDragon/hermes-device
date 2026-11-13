# Users, backends and computers

This page is for the person who runs the hub: adding people, connecting Hermes backends, managing computers, and keeping the hub healthy. If you have not installed the hub yet, start with the [install kit](https://github.com/USA-RedDragon/talaria/blob/main/docs/deploy/getting-started.md), which installs the hub, Hermes and a sandbox with one command.

## First start

1. Start the hub. With the default `auth.mode: local` and no account, it logs a warning with the setup address, `<public URL>/setup`.
2. Open that address, give a name and a password. The first account is an admin with every backend. Do this before you expose the hub to the internet.
3. Add the other people under Settings, then Users.

[Login settings reference](auth.md) describes the other login modes: a reverse-proxy header and OpenID Connect.

## Users

![Settings, Users: the list of people with each person's backends and an Admin switch](images/users.png)

Under Settings, then Users, an admin can:

- **Add a person** (local mode): a name and a first password. The person changes it under Settings, Account.
- **Choose the backends** each person may use.
- **Make an admin** or take admin away. An admin cannot demote, disable or delete themselves, and the hub keeps at least one admin.
- **Disable** an account, which ends its sessions on every replica within seconds, or **delete** it.
- **Set a new password**. This also unlocks an account that locked after too many wrong passwords.

In `proxy` and `oidc` mode, the list shows the accounts that logged in or that an admin made ahead of time. An admin can make an account by name or email before the person's first login, so the first login matches it.

The names in `auth.admins` are admins too, whatever the list says. That is how you make an admin before anyone logged in, with `proxy` or `oidc`.

## Backends

A backend is one Hermes Agent the hub talks to. A user may use a backend when:

- the backend has `everyone: true`,
- `backends[].users` lists them, or `backends[].groups` lists one of their groups, or
- an admin gave them the backend under Settings, Users.

For one person with one Hermes, you do not need a `backends[]` list. Set three environment variables:

```sh
HUB_HERMES_URL=http://hermes:9120
HUB_HERMES_USERNAME=hub
HUB_HERMES_PASSWORD_FILE=/run/secrets/hermes-password
```

This adds a backend with the id `hermes` that every user may use. For anything else, write `backends[]` in the config file:

```yaml
backends:
  - id: alice
    name: Alice's agent
    url: http://hermes-alice:9120
    username: hub
    password: "<the Hermes dashboard password>"
    users: [alice]
    devices: true        # allow computers (talaria) on this backend
```

The hub signs in to Hermes with the username and password of the Hermes dashboard. Hermes needs its Webapp listener on, with a password sign-in, and that port must be open to the hub only:

```yaml
webapp:
  enabled: true
  host: 0.0.0.0
  port: 9120
dashboard:
  basic_auth:
    username: hub
    password_hash: "scrypt$..."
    secret: "<32 or more random bytes>"
```

Give each person their own backend when you want separate memory: a backend has one `MEMORY.md`, one `USER.md` and one Honcho user, so everyone on a shared backend sees the same memory.

What a backend needs beyond stock Hermes, and what you lose without it, is in the [README](../README.md#set-up-a-hermes-backend). The hub detects missing parts when it starts to use them and hides the feature, so a stock Hermes works with fewer features.

## Computers (devices)

Set `device.enabled`, give the hub a signing key, and set `devices: true` on each backend that may have computers:

```sh
openssl genpkey -algorithm ed25519 -out hub-device.pem
```

Give the PEM to the hub as `device.signing_key` (environment variable `HUB_DEVICE_SIGNING_KEY`) or as a file in `device.signing_key_file`. Give each Hermes only the public key: `openssl pkey -in hub-device.pem -pubout`.

Each person pairs their own computers with the `talaria` program ([add a computer](https://github.com/USA-RedDragon/talaria/blob/main/docs/start/add-a-computer.md)). In Settings, Computers, a person sees their computers with when and from where each last connected, and can rename or remove them. Removing a computer ends its token at once.

A computer is pinned to one backend when it pairs. Sandbox devices, the pods that run the agent's commands for chats that are not on a person's computer, are listed in the config as `backends[].service_devices` and show up as Sandbox.

## Telegram and notifications

Both are optional and off by default. See [Telegram](../README.md#telegram) and [Notifications](../README.md#notifications) for every setting. In short:

- Telegram: `telegram.enabled: true`, `telegram.token` from BotFather, and an `https` `http.public_url` for the links.
- Web Push: a key made once with `openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt`, in `notify.vapid_private_key`. Keep it: a new key ends every browser's subscription.

## The database

The hub keeps everything it owns (accounts, Telegram links, paired computers, drafts, saved prompts, notification settings) in one database:

- **SQLite**, the default: one file, `store.path`. This is for one replica. Put it on a persistent volume.
- **PostgreSQL**: `store.driver: postgres` and `store.dsn`, for two or more replicas. Connect to the database directly, not through PgBouncer in transaction mode.

Chats and memory live in Hermes, not in the hub. Back up the hub's database and Hermes's data together. For SQLite, copy the file while the hub is stopped, or use `sqlite3 <file> ".backup <copy>"`. To move from SQLite to PostgreSQL, set `store.import_path` to the old file for one start.

## Reverse proxy notes

The hub speaks plain HTTP on port 8080 by default. Put HTTPS in front of it: pages and push notifications need `https` (or `localhost`), and the login cookies are `Secure` when `http.public_url` is `https`. Set `http.public_url` to the address people open. The hub accepts requests only for that address.

Whatever proxy you use must:

- **Not buffer responses and not time them out.** Chats stream as server-sent events. In nginx: `proxy_buffering off;` and `proxy_read_timeout` of an hour or more. Caddy streams by default. In Envoy Gateway or a Gateway API route, set `timeouts.request: 0s`.
- **Allow WebSockets on `/api/device/`**, for computers. Send the `Upgrade` and `Connection` headers, and do not time the connection out.
- **Not compress the event stream** in a way that holds it back.
- **Send the real client address** if you use `auth.trusted_proxies`.

An nginx location block that works for the hub:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

with `map $http_upgrade $connection_upgrade { default upgrade; '' close; }` in the `http` block. For Caddy, `reverse_proxy 127.0.0.1:8080` is enough.

## Health, logs and metrics

- `GET /healthz` answers `ok` while the process runs. `GET /readyz` answers `ok` once the hub has started and `503` while it shuts down, so use it as the readiness probe.
- Logs go to standard error, as text. `log_level` and the other keys are in the [configuration reference](configuration.md).
- Metrics: set `http.metrics.enabled` and scrape the metrics port (loopback by default).
- On `SIGTERM` the hub fails `/readyz`, keeps serving for `shutdown.drain_delay` (5 seconds), then waits up to `shutdown.timeout` (20 seconds) for open requests.

## Upgrades

Run the new release. The hub migrates its database on start. The `talaria` programs on people's computers update themselves. The hub pins the Hermes release it was tested with (the README names it), so upgrade Hermes through the hub's release notes, not ahead of them.

## Settings reference

Every setting, with its type, default, environment variable and flag, is in the [configuration reference](configuration.md).
