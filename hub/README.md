# hermes-hub

[![Release](https://github.com/USA-RedDragon/hermes-hub/actions/workflows/release.yaml/badge.svg)](https://github.com/USA-RedDragon/hermes-hub/actions/workflows/release.yaml) [![go.mod version](https://img.shields.io/github/go-mod/go-version/USA-RedDragon/hermes-hub.svg)](https://github.com/USA-RedDragon/hermes-hub) [![License](https://badgen.net/github/license/USA-RedDragon/hermes-hub)](https://github.com/USA-RedDragon/hermes-hub/blob/main/LICENSE)

One web front end, behind OpenID Connect login, for many [Hermes Agent](https://github.com/NousResearch/hermes-agent) gateways. It is made for a phone first.

- One list of the sessions on every Hermes backend that you may use, live and stored.
- Open a session and continue it, also a live session that VS Code or the CLI started. The gateway keeps ownership of the session, so you can go back to VS Code at any time.
- Reasoning, tool calls (arguments, progress, result) and the reply stream live, with no refresh.
- New sessions, interrupt, and approval prompts.
- An optional Telegram bot (`telegram.*`) that uses the same backends and permissions.

The hub speaks the canonical session protocol of Hermes <!-- docgen:const pkg=internal/gateway name=HermesVersion -->`v0.22.7`<!-- /docgen:const -->: JSON-RPC over the gateway's `/api/ws` WebSocket. A contract test (`internal/gateway/contract_test.go`) checks the protocol lines it depends on against a Hermes checkout (`HERMES_AGENT_SRC`, default `~/.hermes/hermes-agent`).

## How it works

Each user logs in through the OIDC provider. The hub identifies the user only by the ID token's `sub` claim, which the user cannot change, and maps it to a hub user name with `users[]`. A login whose `sub` is not in `users[]` is refused and gets no session. The `oidc.display_claim` (default `preferred_username`) is shown on screen and never used for access. A user may use a backend when their hub user name is in `backends[].users`, or a group from `oidc.groups_claim` is in `backends[].groups`. Every API call and every stream checks this. The hub stores each user's groups from the last login, so the Telegram bot can apply the same rules. Telegram links and `ui.default_backends` also use the hub user name.

```yaml
users:
  - sub: "<sub of the first user>"
    name: alice
  - sub: "<sub of the second user>"
    name: bob
```

A refused login logs its `sub` (`login refused: sub is not in users`), so you can copy the value from the hub log after a user's first try.

For each backend the hub keeps one WebSocket to the Hermes Webapp listener. It signs in with a dashboard password provider (`POST /auth/password-login`), gets a single-use ticket (`POST /api/auth/ws-ticket`) and dials `/api/ws` with the subprotocols `hermes-gateway-v1` and `hermes-gateway-ticket.<ticket>`. Viewers of the same session share one gateway subscription.

The browser gets each session as server-sent events. Each event has the hub's sequence as its id. When the browser reconnects, it sends the last id back (`Last-Event-ID`) and gets only the events that it missed. If the hub no longer has them, the browser gets a new snapshot. When the connection to a gateway drops, the hub dials again, replays the missed frames with `session.events.since` and attaches again with `session.resume`.

When the hub attaches to a session, it sets the session's `show_reasoning` control on (`config.set`), so that sessions that VS Code started through ACP also stream reasoning. The gateway reads this flag when a turn starts, so the change shows from the next turn. Set `backends[].show_reasoning: false` to turn this off.

## Simple view

Users in `ui.simple_users` see one chat with their default backend (`ui.default_backends`, as `user=backend`, else their first backend) in plain words, with "New chat" and "More". "More" opens the full list of backends and sessions. The simple view changes only what the page shows first; the user can do everything that the full view can, and authorization is the same.

## Set up a Hermes backend

The hub needs the Hermes Webapp listener on an address that it can reach, with a password sign-in. On a non-loopback bind, Hermes always requires sign-in. Add this to the backend's Hermes `config.yaml` (the device's `~/.hermes/config.yaml`, or the cluster instance's config) and restart its gateway:

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

- Make `password_hash` with `plugins.dashboard_auth.basic.hash_password('<password>')` in the Hermes Python environment. The environment variables `HERMES_DASHBOARD_BASIC_AUTH_USERNAME`, `_PASSWORD_HASH`, `_PASSWORD` and `_SECRET` override these keys.
- Set `secret`. Without it, Hermes makes a random key at each start, so each restart signs the hub out (the hub signs in again by itself).
- Open TCP port 9120 only to the hub (on a device: a host firewall rule for the cluster's node addresses or the tailnet; in the cluster: a network policy).
- Put the same user name and password in the hub's `backends[]` entry, with `provider: basic` (the default).

## Set up Authentik

1. Create an OAuth2/OpenID provider: client type confidential, redirect URI `https://<public host>/auth/callback` (strict), signing key set, scopes `openid`, `profile` and `email`. Authentik's default `profile` scope mapping sends `preferred_username` and `groups`.
2. Create an application with slug `hermes-hub` that uses the provider. The issuer is then `https://<authentik host>/application/o/hermes-hub/`.
3. Bind the users or groups that may log in to the application.
4. Keep the provider's subject mode at "Based on the User's hashed ID" (the default). Put each user's `sub` in the hub's `users[]`; the values belong in the deployment config, not in this repository.
5. Give the hub `oidc.issuer_url`, `oidc.client_id` and `oidc.client_secret`.

## Telegram

Set `telegram.enabled`, `telegram.token` and `http.public_url`. A Telegram user sends `/link` to the bot and gets a link to `/link/telegram?token=...`. That page needs a hub login and asks the user to confirm; only the confirmation (a POST with the CSRF token) links the account. A link token is valid for <!-- docgen:const pkg=internal/store name=LinkTokenTTL -->`10m0s`<!-- /docgen:const --> and works once. Run one replica only: the bot uses long polling.

## Deploy

- Run one replica. Put `store.path` on a persistent volume.
- Give `http.public_url` the address that users open. It sets the redirect URI, the only allowed `Origin`, and `Secure` cookies (for `https`).
- The event stream is a long-lived HTTP response. Turn off the request timeout on the route (Gateway API `timeouts.request: 0s`). The hub sends a comment every 15 seconds, so idle timeouts above that are fine.
- `/healthz` answers `ok` without a login.

## Configuration

Configuration is read from `config.yaml` in the working directory, or from the file given with `--config`/`-c` or the `CONFIG` environment variable. Environment variables and flags override the file. A full example is in [`config.example.yaml`](config.example.yaml).

<!-- configulator:begin -->

| Key                         | Type            | Default                    | Environment                     | Flag                          | Description                                                                                                                                                                             |
|-----------------------------|-----------------|----------------------------|---------------------------------|-------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `log_level`                 | string          | `info`                     | `HUB_LOG_LEVEL`                 | `--log_level`                 | Log level: debug, info, warn or error                                                                                                                                                   |
| `http.ipv4_host`            | string          | `0.0.0.0`                  | `HUB_HTTP_IPV4_HOST`            | `--http.ipv4_host`            | HTTP server IPv4 host                                                                                                                                                                   |
| `http.ipv6_host`            | string          | `::`                       | `HUB_HTTP_IPV6_HOST`            | `--http.ipv6_host`            | HTTP server IPv6 host                                                                                                                                                                   |
| `http.port`                 | integer         | `8080`                     | `HUB_HTTP_PORT`                 | `--http.port`                 | HTTP server port                                                                                                                                                                        |
| `http.public_url`           | string          |                            | `HUB_HTTP_PUBLIC_URL`           | `--http.public_url`           | Public base URL of the hub, such as https://hub.example.com. Sets the OIDC redirect URI (<public_url>/auth/callback), the only allowed Origin and whether cookies are Secure (required) |
| `http.metrics.enabled`      | boolean         | `false`                    | `HUB_HTTP_METRICS_ENABLED`      | `--http.metrics.enabled`      | Enable the metrics server                                                                                                                                                               |
| `http.metrics.ipv4_host`    | string          | `127.0.0.1`                | `HUB_HTTP_METRICS_IPV4_HOST`    | `--http.metrics.ipv4_host`    | Metrics server IPv4 host                                                                                                                                                                |
| `http.metrics.ipv6_host`    | string          | `::1`                      | `HUB_HTTP_METRICS_IPV6_HOST`    | `--http.metrics.ipv6_host`    | Metrics server IPv6 host                                                                                                                                                                |
| `http.metrics.port`         | integer         | `8081`                     | `HUB_HTTP_METRICS_PORT`         | `--http.metrics.port`         | Metrics server port                                                                                                                                                                     |
| `oidc.issuer_url`           | string          |                            | `HUB_OIDC_ISSUER_URL`           | `--oidc.issuer_url`           | OIDC issuer URL, such as https://auth.example.com/application/o/hermes-hub/ (required)                                                                                             |
| `oidc.client_id`            | string          |                            | `HUB_OIDC_CLIENT_ID`            | `--oidc.client_id`            | OIDC client ID (required)                                                                                                                                                               |
| `oidc.client_secret`        | string          |                            | `HUB_OIDC_CLIENT_SECRET`        | `--oidc.client_secret`        | OIDC client secret (required) (secret)                                                                                                                                                  |
| `oidc.scopes`               | list of string  | `openid,profile,email`     | `HUB_OIDC_SCOPES`               | `--oidc.scopes`               | Scopes to request. Authentik's profile scope carries preferred_username and groups                                                                                                      |
| `oidc.display_claim`        | string          | `preferred_username`       | `HUB_OIDC_DISPLAY_CLAIM`        | `--oidc.display_claim`        | ID token claim shown as the user's name. Display only: identity is always the sub claim                                                                                                 |
| `oidc.groups_claim`         | string          | `groups`                   | `HUB_OIDC_GROUPS_CLAIM`         | `--oidc.groups_claim`         | ID token claim that holds the group names that backends[].groups lists                                                                                                                  |
| `session.secret`            | string          |                            | `HUB_SESSION_SECRET`            | `--session.secret`            | Key for the session and login cookies. At least 32 characters (required) (secret)                                                                                                       |
| `session.ttl`               | string          | `168h`                     | `HUB_SESSION_TTL`               | `--session.ttl`               | How long a login lasts                                                                                                                                                                  |
| `ui.simple_users`           | list of string  |                            | `HUB_UI_SIMPLE_USERS`           | `--ui.simple_users`           | User names that get the simple view: one chat with their default backend and plain wording. Backends, sessions and details stay one tap away                                            |
| `ui.default_backends`       | list of string  |                            | `HUB_UI_DEFAULT_BACKENDS`       | `--ui.default_backends`       | Default backend per user, as user=backend pairs. A user without one gets their first backend                                                                                            |
| `store.path`                | string          | `/data/hermes-hub.db`      | `HUB_STORE_PATH`                | `--store.path`                | Path of the SQLite database that holds Telegram links, link tokens and users' last seen groups                                                                                          |
| `users`                     | list of objects |                            | —                               | —                             | Who may log in: each OIDC sub and its hub user name. A login with any other sub is refused. Only settable in the config file                                                            |
| `users[].sub`               | string          |                            | —                               | —                             | OIDC sub claim of the user. Authentik's default sub mode is the hashed user ID                                                                                                          |
| `users[].name`              | string          |                            | —                               | —                             | Hub user name, used by backends[].users, ui.default_backends and Telegram links                                                                                                         |
| `backends`                  | list of objects |                            | —                               | —                             | Hermes backends and who may use them. Only settable in the config file                                                                                                                  |
| `backends[].id`             | string          |                            | —                               | —                             | Stable backend ID: lowercase letters, digits and dashes                                                                                                                                 |
| `backends[].name`           | string          |                            | —                               | —                             | Display name                                                                                                                                                                            |
| `backends[].url`            | string          |                            | —                               | —                             | Base URL of the Hermes Webapp listener, such as http://desktop:9120                                                                                                                 |
| `backends[].provider`       | string          |                            | —                               | —                             | Hermes dashboard password provider. Default basic                                                                                                                                       |
| `backends[].username`       | string          |                            | —                               | —                             | Hermes dashboard user name                                                                                                                                                              |
| `backends[].password`       | string          |                            | —                               | —                             | Hermes dashboard password (secret)                                                                                                                                                      |
| `backends[].users`          | list of string  |                            | —                               | —                             | Hub user names that may use this backend                                                                                                                                                |
| `backends[].groups`         | list of string  |                            | —                               | —                             | Hub groups whose members may use this backend                                                                                                                                           |
| `backends[].cwd`            | string          |                            | —                               | —                             | Working directory for new sessions. Default: the gateway's own                                                                                                                          |
| `backends[].show_reasoning` | boolean         |                            | —                               | —                             | Turn on reasoning streaming for each session the hub attaches to. Default true                                                                                                          |
| `telegram.enabled`          | boolean         | `false`                    | `HUB_TELEGRAM_ENABLED`          | `--telegram.enabled`          | Run the Telegram bot                                                                                                                                                                    |
| `telegram.token`            | string          |                            | `HUB_TELEGRAM_TOKEN`            | `--telegram.token`            | Telegram bot token from BotFather (secret)                                                                                                                                              |
| `telegram.api_url`          | string          | `https://api.telegram.org` | `HUB_TELEGRAM_API_URL`          | `--telegram.api_url`          | Telegram Bot API base URL                                                                                                                                                               |
| `telegram.default_backends` | list of string  |                            | `HUB_TELEGRAM_DEFAULT_BACKENDS` | `--telegram.default_backends` | Default backend per hub user, as user=backend pairs. A user without one gets their first backend                                                                                        |
| `telegram.allowed_groups`   | list of integer |                            | `HUB_TELEGRAM_ALLOWED_GROUPS`   | `--telegram.allowed_groups`   | Group chat IDs the bot answers in. The bot ignores every other group                                                                                                                    |
| `telegram.poll_timeout`     | string          | `50s`                      | `HUB_TELEGRAM_POLL_TIMEOUT`     | `--telegram.poll_timeout`     | Long poll timeout for getUpdates                                                                                                                                                        |
| `telegram.edit_interval`    | string          | `1500ms`                   | `HUB_TELEGRAM_EDIT_INTERVAL`    | `--telegram.edit_interval`    | Minimum time between edits of one live message                                                                                                                                          |
| `telegram.max_retries`      | integer         | `5`                        | `HUB_TELEGRAM_MAX_RETRIES`      | `--telegram.max_retries`      | Retries of a Bot API call that Telegram answers with 429                                                                                                                                |

<!-- configulator:end -->

## Development

Go <!-- docgen:gomod field=go -->`1.27.2`<!-- /docgen:gomod --> or later.

```sh
go generate ./...
go test -race ./...
golangci-lint run ./...
```

`go generate` writes `internal/config/config_configulator.go`. The configuration table above, `config.example.yaml` and the marked values in this README are generated: run the configulator pre-commit hooks and `go run github.com/USA-RedDragon/docgen@v0.0.0-20261109224744-ef1962a8c996 update`.
