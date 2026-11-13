# Logging in

The hub has three ways to log people in, set by `auth.mode`:

| Mode | Who says who you are | Needs |
|---|---|---|
| `local` (the default) | The hub: accounts with passwords in its database | Nothing else |
| `proxy` | A reverse proxy in front of the hub, by a header or a signed token | A proxy that logs people in: Authelia, Authentik's proxy outpost, oauth2-proxy, tailscale serve, Cloudflare Access, Envoy Gateway |
| `oidc` | An OpenID Connect provider | Keycloak, Authentik, Authelia, Pocket ID, Google and others |

When `auth.mode` is not set, a hub with `oidc.issuer_url` uses `oidc` (so a config written before the other modes keeps working) and any other hub uses `local`.

Every mode ends in the same session cookie (`session.ttl`, default a week). `session.secret` may stay empty: the hub then makes a key and keeps it in its database, so every replica shares it and a restart keeps people logged in.

## Accounts and backends

A person is a hub user name. A user may use a backend when any of these holds:

- the backend has `everyone: true` (the `hermes.url` shortcut has it),
- `backends[].users` names them, or `backends[].groups` names one of their groups (from the login),
- an admin gave them the backend in Settings > Users.

Admins are the accounts marked admin in Settings > Users and the names in `auth.admins`. Admins add, change, disable and delete accounts and pick each one's backends. An admin cannot disable, demote or delete themselves, and the hub keeps at least one admin. Disabling an account, a new password and deleting it end its sessions on every replica within <!-- docgen:const pkg=internal/web name=accountCheckTTL -->`5s`<!-- /docgen:const -->.

For one person with one Hermes, no `backends[]` list is needed:

```sh
HUB_HERMES_URL=http://hermes:9120
HUB_HERMES_USERNAME=hub
HUB_HERMES_PASSWORD_FILE=/run/secrets/hermes-password
```

This adds a backend with id `hermes` that every hub user may use.

## Local accounts (`auth.mode: local`)

1. Start the hub. While it has no account, it prints a setup token to its log:

   ```text
   level=WARN msg="The hub has no account yet. Open the setup page and give this setup token. ..." url=https://hub.example.com/setup setup_token=...
   ```

   With Docker: `docker compose logs hub | grep setup_token`. To pick the token yourself (for an install with no log access), set `auth.local.setup_token` or `auth.local.setup_token_file`; then the hub prints no token.
2. Open the setup page, give the token, a name and a password. That makes the first account, an admin, with every backend. The setup page works only while the hub has no account, also with several replicas, and a restart makes a new token until then.
3. Add the other people in Settings > Users.

Passwords:

- At least <!-- docgen:const pkg=internal/auth name=MinPassword -->`12`<!-- /docgen:const --> characters and not the account's name; at most <!-- docgen:const pkg=internal/auth name=MaxPasswordBytes -->`1024`<!-- /docgen:const --> bytes.
- Stored as argon2id hashes (19 MiB, 2 passes, OWASP's baseline). The hub runs at most 4 hashes at once.
- `auth.local.max_failures` wrong passwords in a row (default 5) lock the account for `auth.local.lockout` (default 15 minutes). An admin unlocks it sooner by setting a new password.
- Each client address may try at most <!-- docgen:const pkg=internal/web name=loginsPerWindow -->`10`<!-- /docgen:const --> logins or setups in <!-- docgen:const pkg=internal/web name=loginWindow -->`1m0s`<!-- /docgen:const -->. Behind a proxy, set `auth.trusted_proxies` so the hub reads the client address from `X-Forwarded-For`; without it every client counts as the proxy.
- The answer to a failed login is the same for an unknown name, a wrong password and a locked account.
- Each person changes their own password in Settings; that ends their other sessions.

Not built yet: two-factor codes (TOTP) and a command to reset a password from the shell.

## Behind a proxy (`auth.mode: proxy`)

The proxy logs the person in and sends who they are. The hub trusts it only from `auth.trusted_proxies`, or, with `auth.proxy.jwt.jwks_url`, only when the proxy's signed token checks out. Make sure the proxy removes these headers from what clients send.

The hub maps an identity to a user in this order:

1. `users[]`, with the identity as `sub`.
2. The account the identity logged in to before.
3. An account an admin made whose name equals the identity, or whose email equals the proxy's email header, and which no identity has used yet.
4. With `auth.proxy.auto_create`, a new account named after the identity. The first one is an admin when `auth.admins` is empty.

Otherwise the login is refused and the hub logs the identity.

Each request must come with the same identity as the login; when the proxy logs someone else in, the old session stops working. `auth.proxy.logout_url` is where Log out sends the browser after the hub's own logout, such as the proxy's logout page.

### Header settings for common proxies

| Proxy | `user_header` | `name_header` | `email_header` | `groups_header` |
|---|---|---|---|---|
| Authelia | `Remote-User` | `Remote-Name` | `Remote-Email` | `Remote-Groups` |
| Authentik proxy outpost | `X-authentik-username` | `X-authentik-name` | `X-authentik-email` | `X-authentik-groups` |
| oauth2-proxy (`--set-xauthrequest`) | `X-Auth-Request-User` | `X-Auth-Request-Preferred-Username` | `X-Auth-Request-Email` | `X-Auth-Request-Groups` |
| tailscale serve | `Tailscale-User-Login` | `Tailscale-User-Name` | `Tailscale-User-Login` | none |

For tailscale serve, `auth.trusted_proxies` is `127.0.0.1` and `::1`: serve runs on the same machine.

### Signed tokens: Cloudflare Access

```yaml
auth:
  mode: proxy
  proxy:
    jwt:
      jwks_url: https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
      issuer: https://<team>.cloudflareaccess.com
      audiences: [<the application's AUD tag>]
      header: Cf-Access-Jwt-Assertion
      cookie: CF_Authorization
      user_claim: email
```

### Kubernetes Gateway API with Envoy Gateway

An Envoy Gateway `SecurityPolicy` can log people in with OIDC in front of the hub. With Envoy Gateway v1.8 or later, forward the ID token and let the hub check it against the provider's keys, so it does not have to trust a plain header:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: talaria-hub
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: talaria-hub
  oidc:
    provider:
      issuer: https://auth.example.com/realms/home
    clientID: talaria-hub
    clientSecret:
      name: talaria-hub-oidc
    redirectURL: https://hub.example.com/oauth2/callback
    logoutPath: /oauth2/logout
    forwardIDToken:
      header: X-Id-Token
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: talaria-hub
spec:
  parentRefs:
    - name: public
  hostnames: [hub.example.com]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: talaria-hub
          port: 8080
      timeouts:
        request: 0s
```

```yaml
auth:
  mode: proxy
  proxy:
    logout_url: /oauth2/logout
    jwt:
      jwks_url: https://auth.example.com/realms/home/protocol/openid-connect/certs
      issuer: https://auth.example.com/realms/home
      audiences: [talaria-hub]
      header: X-Id-Token
      user_claim: sub
      name_claim: preferred_username
```

Notes:

- The talaria device API (`/api/device/`) must reach the hub without the proxy's login: devices log in with their own tokens. Give `/api/device/` its own HTTPRoute rule without the SecurityPolicy (a policy that targets the route covers all its rules, so put that rule in a second HTTPRoute).
- `timeouts.request: 0s` keeps the event streams open.
- Before v1.8, Envoy Gateway has no `forwardIDToken`. Then use `forwardAccessToken: true` with `header: Authorization` on the hub, when the provider's access tokens are JWTs (Keycloak's are; set `audiences` to what the provider puts in them), or a JWT provider in the SecurityPolicy with `claimToHeaders` (such as `sub` to `X-Auth-User`) and `auth.proxy.user_header: X-Auth-User` with `auth.trusted_proxies` set to the Envoy pods' addresses.

These recipes follow the Envoy Gateway API reference; they have not been run against a live Envoy Gateway yet.

## OpenID Connect (`auth.mode: oidc`)

Give the hub `oidc.issuer_url`, `oidc.client_id` and `oidc.client_secret` (or `client_secret_file`). The redirect URI is `<http.public_url>/auth/callback`. The hub uses PKCE and a nonce, and the `sub` claim is the identity.

People log in when:

1. `users[]` lists their `sub`, or
2. an admin made an account with their email, and the provider says the email is verified (`email_verified`), or
3. `oidc.auto_create` is on and their verified email's domain is in `oidc.allowed_domains` (`*` for any), or they are in one of `oidc.allowed_groups`. The account's name comes from `oidc.display_claim` or the email. The first such account is an admin when `auth.admins` is empty.

### Authentik

1. Create an OAuth2/OpenID provider: client type confidential, redirect URI `https://<public host>/auth/callback` (strict), signing key set, scopes `openid`, `profile` and `email`. The default `profile` scope mapping sends `preferred_username` and `groups`.
2. Create an application with slug `talaria-hub` that uses the provider. The issuer is then `https://<authentik host>/application/o/talaria-hub/`.
3. Bind the users or groups that may log in to the application.
4. Keep the provider's subject mode at "Based on the User's hashed ID" (the default). Put each user's `sub` in `users[]`, or turn on `oidc.auto_create`.

### Keycloak

Create an OpenID Connect client with client authentication on and the redirect URI above. The issuer is `https://<keycloak host>/realms/<realm>`. For groups, add a "Group Membership" mapper with token claim name `groups` and "Full group path" off.
