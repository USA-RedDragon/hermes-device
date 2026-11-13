# Login options

The hub has three ways to log people in. Which one you use decides who can sign in and where the accounts live.

| Mode | Who says who you are | Good for |
| ---- | -------------------- | -------- |
| Local accounts | The hub: accounts with passwords in its own database | One person or a household, with nothing else to run. The default of the hub's own accounts release |
| Reverse-proxy header | A proxy in front of the hub that logs people in and sends their name | A setup that already has Authelia, an Authentik proxy outpost, oauth2-proxy, Tailscale Serve, Cloudflare Access or Envoy Gateway |
| OpenID Connect (OIDC) | An identity provider: Keycloak, Authentik, Authelia, Pocket ID, Google and others | Several people, with accounts you already manage in one place |

Every mode ends in the same session cookie. The hub's own page, [Logging in](https://github.com/USA-RedDragon/talaria-hub/blob/main/docs/auth.md), has every setting.

## What these install files do today

The install files here pin a hub release from before the hub's own accounts. They bring a small built-in login instead: [Dex](https://dexidp.io), with one admin account made on the first start. It runs next to the hub and answers under `<public URL>/dex`. This is the same for Compose, Helm and the all-in-one image.

| Install | Built-in login (Dex) | Your own OIDC provider | Local accounts and proxy header |
| ------- | -------------------- | ---------------------- | ------------------------------- |
| Compose | Yes, the default | Edit `files/init.sh` (see below) | When the pinned hub release has them |
| Helm | Yes, `auth.mode=builtin`, the default | Yes, `auth.mode=oidc` | When the pinned hub release has them |
| All-in-one | Yes | Not wired up | When the pinned hub release has them |

When the install files move to a hub release with `auth.mode`, local accounts become the default, the built-in Dex login goes away, and this page changes. How an install that used the built-in login moves over is not decided yet.

## Built-in login (today's default)

- One account: `TALARIA_ADMIN_EMAIL` (compose, all-in-one) or `auth.admin.email` (Helm), with a random password printed on the first start, or the one you set in `TALARIA_ADMIN_PASSWORD` or `auth.admin.password`.
- The first start prints the address and the login:

  ```bash
  docker compose logs init
  ```

  ```text
   Open:      http://localhost:8080
   Log in as: admin@talaria.local
   Password:  <random>
  ```

  Helm: `kubectl -n talaria get secret talaria-keys -o jsonpath='{.data.admin-password}' | base64 -d; echo`.
- To change the password, set it in `.env` (or the Helm value) and apply (`docker compose up -d`, or `helm upgrade`). The hub user name stays `admin` (`TALARIA_ADMIN_USER`).
- More people: not with the built-in login. Use your own identity provider.

## Local accounts

When the hub has its own accounts, the first start works like this:

1. The hub logs a warning with the setup address, `<public URL>/setup`, while it has no account.
2. Open it, give a name and a password. That makes the first account, an admin, with every backend. **There is no setup token.** Whoever opens the page first gets the admin account, so open it yourself before you expose the hub to the internet. Until then keep the port closed to everyone else.
3. Add the other people under Settings, then Users.

Passwords are at least 12 characters, stored as argon2id hashes, and an account locks for 15 minutes after 5 wrong tries. Each person changes their own password under Settings, Account. An admin sets a new password for someone, disables an account or deletes it under Settings, Users. An admin cannot disable, demote or delete themselves, and the hub always keeps one admin.

Two-factor codes and a command to reset a password from the shell are not built yet. If you lose the only admin password and no other admin exists, restore a backup or start over. See [Upgrades and backups](upgrades.md).

## Reverse-proxy header

Use this when a proxy already logs your people in. The hub trusts the user name from a header, and only when the request comes from an address you list as a trusted proxy. Make sure the proxy removes that header from what clients send, or anyone could set it.

| Proxy | User header |
| ----- | ----------- |
| Authelia | `Remote-User` |
| Authentik proxy outpost | `X-authentik-username` |
| oauth2-proxy | `X-Auth-Request-User` |
| Tailscale Serve | `Tailscale-User-Login` |

The hub's [auth page](https://github.com/USA-RedDragon/talaria-hub/blob/main/docs/auth.md) has the full table and how an identity maps to a hub account. Settings: `auth.mode: proxy`, `auth.trusted_proxies`, `auth.proxy.user_header`, and optionally `auth.proxy.auto_create` to make accounts on first login.

## Envoy Gateway (Kubernetes Gateway API)

An Envoy Gateway `SecurityPolicy` can log people in with OIDC in front of the hub. Forward the ID token and let
the hub check it against the provider's keys, so the hub does not have to trust a plain header. The hub's auth
page has a complete `SecurityPolicy`, `HTTPRoute` and hub config for this. Use Envoy Gateway 1.10 or later: 1.8.0
accepts `forwardIDToken` but does not send the header, and the hub then says "The proxy did not say who you are".

With the Helm chart, turn on the routes (`httpRoute.enabled=true`). The chart makes two `HTTPRoute`s: `<release>`
for browsers and `<release>-device` for `/api/device/`. Point the `SecurityPolicy` at `<release>` only: computers
log in to `/api/device/` with their own tokens. Give the hub the proxy settings in `hub.env`, for example:

```bash
--set hub.env.HUB_AUTH_MODE=proxy \
--set hub.env.HUB_AUTH_PROXY_LOGOUT_URL=/oauth2/logout \
--set hub.env.HUB_AUTH_PROXY_JWT_JWKS_URL=https://id.example.com/keys \
--set hub.env.HUB_AUTH_PROXY_JWT_ISSUER=https://id.example.com \
--set hub.env.HUB_AUTH_PROXY_JWT_AUDIENCES=<the SecurityPolicy's clientID> \
--set hub.env.HUB_AUTH_PROXY_JWT_HEADER=X-Id-Token
```

Tested with Envoy Gateway 1.10.0 and Dex as the provider, with a hub built from its accounts branch (not
released yet when this was written).

## Your own OIDC provider

Any standard provider works. Make a client for the hub, with the redirect address `<public URL>/auth/callback`, and keep the client ID and secret.

**Helm:**

```sh
helm upgrade talaria ./talaria -n talaria --reuse-values \
  --set auth.mode=oidc \
  --set auth.oidc.issuerUrl=https://id.example.com/realms/home \
  --set auth.oidc.clientId=talaria \
  --set auth.oidc.existingSecret=talaria-oidc \
  --set 'auth.oidc.users[0].sub=<alice-sub>' \
  --set 'auth.oidc.users[0].name=admin'
```

`talaria-oidc` is a Secret with the key `client-secret`. `auth.oidc.users` lists who may log in: each person's `sub` claim and the hub user name for it. The `auth.admin.user` name must be one of them.

To find a person's `sub`: let them try to log in. The hub refuses and logs the `sub` it saw.

**Compose:** not wired up yet. The `oidc:` and `users:` blocks of the hub config are written in `files/init.sh`.

Setting up the provider itself (Keycloak, Authentik, Authelia and others) is the provider's own documentation. The hub's [auth page](https://github.com/USA-RedDragon/talaria-hub/blob/main/docs/auth.md) lists what the hub needs from each.

## Which one

- Just you, one server: local accounts, or the built-in login until the hub's accounts ship.
- A family or a small team, no identity provider: local accounts, with an admin who adds people.
- You already run one login for your services: OIDC, or the proxy header if you want one login in front of everything.
- Several hub replicas (Helm with PostgreSQL): OIDC. The built-in login keeps state in its pod.
