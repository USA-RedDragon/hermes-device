# Set up logins

The hub has three ways to log people in. Which one you use decides who can sign in and where the accounts live.

| Mode | Who says who you are | Good for |
| ---- | -------------------- | -------- |
| Local accounts (the default) | The hub: accounts with passwords in its own database | One person, a household or a small team, with nothing else to run |
| Reverse proxy | A proxy in front of the hub that logs people in and says who they are | A setup that already has Authelia, an Authentik proxy outpost, oauth2-proxy, Tailscale Serve, Cloudflare Access or Envoy Gateway |
| OpenID Connect (OIDC) | Your identity provider: Keycloak, Authentik, Authelia, Pocket ID, Google and others | Several people, with accounts you already manage in one place |

Every mode ends in the same session cookie. The hub's own page, [Login settings reference](../../hub/docs/auth.md), has every setting.

| Install | Local accounts | Reverse proxy | OIDC |
| ------- | -------------- | ------------- | ---- |
| Compose | The default | `compose.override.yaml` (below) | `compose.override.yaml` (below) |
| Helm | The default, `auth.mode=local` | `auth.mode=proxy` and `auth.proxy` | `auth.mode=oidc` and `auth.oidc` |
| All-in-one (Unraid, TrueNAS) | The default | `HUB_AUTH_*` variables on the container | `HUB_AUTH_MODE` and `HUB_OIDC_*` variables on the container |

## Local accounts (the default)

There is no password in the install files and none in the log. The first person who opens Talaria makes the admin
account:

1. Start Talaria and open its address. While the hub has no account, every page sends you to the setup page, and
   the hub's log warns:

   ```text
   level=WARN msg="Setup is not finished. The first visitor of the setup page becomes the admin, so finish setup before the hub is open to the internet" url=https://talaria.example.com/setup
   ```

2. Give a user name, a display name and a password (at least 12 characters). That makes the first account, an
   admin, and logs you in.

   ![The setup page](images/setup.png)

   The first account is the admin. Make it right after the first start, before Talaria is reachable from the
   internet.
3. Later visits show the login page.

   ![The login page](images/login.png)

### More people

An admin adds people under Settings, Users: a user name, a display name and a first password, and the agents
(backends) the person may use. The first account gets every backend; a new account gets the ones the admin ticks.
Each person can then change their own password under Settings, Account.

![Settings, Users](images/settings-users.png)

The install files run one agent, shared by everyone you give it to: the people who share it see the same chats
and memory. Give it to people you would share a computer with. For separate agents per person, run more agent
backends (see the hub's [Login settings reference](../../hub/docs/auth.md) page,
`backends[]`).

![Settings, Account](images/settings-account.png)

Passwords are stored as argon2id hashes, and an account locks for 15 minutes after 5 wrong tries in a row. An
admin sets a new password for someone (which also unlocks the account), disables an account or deletes it. An admin
cannot disable, demote or delete themselves, and the hub always keeps one admin.

Two-factor codes and a command to reset a password from the shell are not built yet. If you lose the only admin
password, restore a backup of the hub's data or start over (see [Upgrades and backups](upgrades.md)).

### Whose agent this is: `TALARIA_USER`

Optional. Set it to the hub user name of the person this agent works for, once that account exists
(`TALARIA_USER` in `.env` or on the all-in-one container, `auth.user` with Helm), then apply (`docker compose up
-d`, or `helm upgrade`). It does two things:

- **Memory:** with Honcho on, it is that person's name in Honcho, and the hub's Memory page shows what Honcho
  learned about them. Empty: the name is `user`.
- **Computers:** a chat that runs on no computer belongs to this person when the agent's `/device` command moves it
  to a computer. Empty: it belongs to the one person whose computers are connected; with several people's
  computers connected, move such chats from the hub instead.

## Behind a reverse proxy

Use this when a proxy already logs your people in. The proxy sends the person's name in a header, and the hub
believes that header from the address of the proxy right in front of it. In the install files that is the Caddy
of Talaria itself (`front`), which passes on what your proxy sends. So **only your proxy may reach Talaria's port**:
anyone else could send the header themselves. Bind the port to the proxy's network or to `127.0.0.1`, and make your
proxy replace the header on every request (Authelia, oauth2-proxy and the Authentik outpost do).

| Proxy | `HUB_AUTH_PROXY_USER_HEADER` | `..._NAME_HEADER` | `..._EMAIL_HEADER` | `..._GROUPS_HEADER` |
| ----- | ---------------------------- | ----------------- | ------------------ | ------------------- |
| Authelia (the defaults) | `Remote-User` | `Remote-Name` | `Remote-Email` | `Remote-Groups` |
| Authentik proxy outpost | `X-authentik-username` | `X-authentik-name` | `X-authentik-email` | `X-authentik-groups` |
| oauth2-proxy (`--set-xauthrequest`) | `X-Auth-Request-User` | `X-Auth-Request-Preferred-Username` | `X-Auth-Request-Email` | `X-Auth-Request-Groups` |
| Tailscale Serve | `Tailscale-User-Login` | `Tailscale-User-Name` | `Tailscale-User-Login` | none |

The first time someone logs in, the hub uses an account an admin made with that user name or email, or, with
`HUB_AUTH_PROXY_AUTO_CREATE=true`, makes one. The first account it makes is an admin.

**Compose with Authelia** (your reverse proxy, such as Caddy, Traefik or nginx, asks Authelia and forwards to
Talaria on port 8080 of this host). Put this next to `compose.yaml` as `compose.override.yaml` and run `docker
compose up -d`:

```yaml
services:
  hub:
    environment:
      HUB_AUTH_MODE: proxy
      HUB_AUTH_PROXY_AUTO_CREATE: "true"
      HUB_AUTH_PROXY_LOGOUT_URL: https://auth.example.com/logout
  front:
    # Only this host (where your proxy runs) reaches Talaria
    ports: !override
      - "127.0.0.1:8080:8080"
```

With Caddy as your proxy, for example:

```text
talaria.example.com {
	forward_auth authelia:9091 {
		uri /api/authz/forward-auth
		copy_headers Remote-User Remote-Groups Remote-Email Remote-Name
	}
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

Computers you pair log in to `/api/device/` with their own tokens, not through the proxy: let `/api/device/*`
through without the login (in Authelia, an `access_control` rule with `policy: bypass` for that path).

**Compose with oauth2-proxy:** the same override, with `HUB_AUTH_PROXY_USER_HEADER: X-Auth-Request-User`,
`HUB_AUTH_PROXY_EMAIL_HEADER: X-Auth-Request-Email` and `HUB_AUTH_PROXY_LOGOUT_URL: /oauth2/sign_out`, and
oauth2-proxy run with `--set-xauthrequest` behind your proxy's `auth_request` or `forward_auth`.

**Helm:** `--set auth.mode=proxy --set auth.proxy.user_header=Remote-User --set auth.proxy.auto_create=true`.
`auth.proxy` takes the hub's `auth.proxy` settings as they are. Limit who reaches the Service, for example with a
NetworkPolicy, or use a signed token (below), which the hub checks itself.

**All-in-one:** add `HUB_AUTH_MODE=proxy` and the `HUB_AUTH_PROXY_*` variables to the container, and publish its
port only to your proxy.

### Envoy Gateway (Kubernetes Gateway API)

An Envoy Gateway `SecurityPolicy` can log people in with OIDC in front of the hub. Forward the ID token and let
the hub check it against the provider's keys, so the hub does not have to trust a plain header. The hub's auth
page has a complete `SecurityPolicy`, `HTTPRoute` and hub config for this. Use Envoy Gateway 1.10 or later: 1.8.0
accepts `forwardIDToken` but does not send the header, and the hub then says "The proxy did not say who you are".

With the Helm chart, turn on the routes (`httpRoute.enabled=true`). The chart makes two `HTTPRoute`s: `<release>`
for browsers and `<release>-device` for `/api/device/`. Point the `SecurityPolicy` at `<release>` only: computers
log in to `/api/device/` with their own tokens. Give the hub the token settings in `auth.proxy`:

```yaml
auth:
  mode: proxy
  proxy:
    logout_url: /oauth2/logout
    auto_create: true
    jwt:
      jwks_url: https://id.example.com/keys
      issuer: https://id.example.com
      audiences: [<the SecurityPolicy's clientID>]
      header: X-Id-Token
```

Tested with Envoy Gateway 1.10.0 and Dex as the provider.

## Your own OIDC provider

Any standard provider works. Make a client for the hub, with the redirect address `<public URL>/auth/callback`,
and keep the client ID and secret. Then say who may log in, one or more of:

- an admin makes an account with the person's email under Settings, Users, and the provider says the email is
  verified;
- `auto_create` with `allowed_domains` (or `allowed_groups`): the hub makes an account on the first login. The first
  one it makes is an admin;
- `users`: each person's `sub` claim and their hub user name. To find a `sub`, let the person try to log in; the hub
  refuses and logs the `sub` it saw.

**Compose:** in `compose.override.yaml`, then `docker compose up -d`:

```yaml
services:
  hub:
    environment:
      HUB_AUTH_MODE: oidc
      HUB_OIDC_ISSUER_URL: https://id.example.com/realms/home
      HUB_OIDC_CLIENT_ID: talaria
      HUB_OIDC_CLIENT_SECRET: <the client secret>
      HUB_OIDC_AUTO_CREATE: "true"
      HUB_OIDC_ALLOWED_DOMAINS: example.com
```

**Helm:**

```sh
helm upgrade talaria oci://ghcr.io/usa-reddragon/charts/talaria -n talaria --reuse-values \
  --set auth.mode=oidc \
  --set auth.oidc.issuerUrl=https://id.example.com/realms/home \
  --set auth.oidc.clientId=talaria \
  --set auth.oidc.existingSecret=talaria-oidc \
  --set auth.oidc.settings.auto_create=true \
  --set 'auth.oidc.settings.allowed_domains={example.com}'
```

`talaria-oidc` is a Secret with the key `client-secret`. If your provider's certificate comes from a private
authority, give the hub its root with `--set-file hub.caBundle=root.crt`.

**All-in-one:** the same `HUB_AUTH_MODE` and `HUB_OIDC_*` variables on the container.

Setting up the provider itself (Keycloak, Authentik, Authelia and others) is the provider's own documentation. The
hub's [auth page](../../hub/docs/auth.md) lists what the hub needs from
each.

## Which one

- Just you, or a household: local accounts.
- A small team with no identity provider: local accounts, with an admin who adds people.
- You already run one login for your services: OIDC, or the reverse proxy if you want one login in front of
  everything.
- Several hub replicas (Helm with PostgreSQL): any mode; accounts live in the database the replicas share.
