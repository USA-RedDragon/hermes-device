# Reference

Everything on this page is generated from the files in the repository, so it matches the version you downloaded.

## Compose settings (.env)

Every setting is optional. Copy `deploy/compose/.env.example` to `.env` and change what you need, then run `docker compose up -d`. The `init` service writes the configs again from `.env` on every start and never changes a secret it made.

<!-- docgen:code file=deploy/compose/.env.example -->
```
# Talaria settings for docker compose. Copy this file to .env and change what you need.
# Every setting is optional. Secrets that Talaria needs inside are made for you on the first start.

# --- Where people open Talaria ---
# Port on this host
TALARIA_PORT=8080
# The address in the browser. Empty: http://localhost:<TALARIA_PORT>, for this computer only. Use
# https://<your domain> behind HTTPS, or http://<this host's address>:<TALARIA_PORT> on a LAN. Pages and push
# notifications need HTTPS or localhost.
TALARIA_PUBLIC_URL=
# For automatic HTTPS from Caddy: your domain name (also publish ports 80 and 443, see https://usa-reddragon.github.io/talaria/deploy/compose.html)
# TALARIA_SITE=talaria.example.com
# Where the certificate comes from. Empty: Let's Encrypt (the domain must point at this host, ports 80 and 443
# open). An email address: the same, with that contact. internal: Caddy's own CA, for a LAN or a test; then
# browsers and computers must trust its root (https://usa-reddragon.github.io/talaria/deploy/compose.html)
# TALARIA_TLS=internal

# --- Accounts ---
# There is no admin password here: the first person who opens Talaria makes the admin account on its setup
# page. Other login options (your OIDC provider, or a reverse proxy such as Authelia): https://usa-reddragon.github.io/talaria/deploy/auth.html
# The hub account of the person this Hermes works for, once it exists (optional). Names that person in Honcho
# memory, and owns chats that are on no computer. Empty: works while one person's computers are paired
TALARIA_USER=
# Contact address that push services get with Web Push, such as mailto:you@example.com. Empty: the public URL
# when that is https (push notifications need https anyway)
TALARIA_PUSH_CONTACT=

# --- Model ---
# Optional parts to start: demo (a stand-in model with no API key), ollama, honcho, honcho-embeddings.
# Comma-separated. Remove demo once you set a real model below.
COMPOSE_PROFILES=demo

# Any OpenAI-compatible API. Empty: the demo model.
# OpenRouter:  TALARIA_MODEL_BASE_URL=https://openrouter.ai/api/v1  TALARIA_MODEL=anthropic/claude-sonnet-4.5
# OpenAI:      TALARIA_MODEL_BASE_URL=https://api.openai.com/v1     TALARIA_MODEL=gpt-5-mini
# Ollama:      TALARIA_MODEL_BASE_URL=http://ollama:11434/v1        TALARIA_MODEL=llama3.2:3b  (profile ollama)
#              The model must handle a 64K window: Hermes refuses smaller ones (qwen3 in Ollama has 40K)
TALARIA_MODEL_BASE_URL=
TALARIA_MODEL=
TALARIA_MODEL_API_KEY=
# Context window of the model in tokens. Hermes needs at least 64000
TALARIA_MODEL_CONTEXT=131072
# With profile ollama: a model to download on start, such as llama3.2:3b
TALARIA_OLLAMA_PULL=

# --- Images ---
# Another Hermes image, such as one you built or loaded (the default is pinned by digest)
# TALARIA_HERMES_IMAGE=ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p30

# --- Time zone of schedules and the agent's clock (IANA name, such as Europe/Berlin). Empty: UTC
TZ=

# --- Telegram bot (optional): a token from @BotFather
TALARIA_TELEGRAM_TOKEN=

# --- Long-term memory with Honcho (optional) ---
# Add honcho to COMPOSE_PROFILES and set:
#   TALARIA_HONCHO_URL=http://honcho:8000
# Embeddings: add honcho-embeddings to COMPOSE_PROFILES for a small local model (about 1 GB of RAM), or use a
# hosted API, for example OpenAI:
#   TALARIA_EMBEDDINGS_BASE_URL=https://api.openai.com/v1
#   TALARIA_EMBEDDINGS_MODEL=text-embedding-3-small
#   TALARIA_EMBEDDINGS_API_KEY=sk-...
#   TALARIA_EMBEDDINGS_DIMENSIONS=1536
# Honcho uses the model above for what it learns; TALARIA_HONCHO_MODEL picks another one.
TALARIA_HONCHO_URL=
```
<!-- /docgen:code -->

## Compose services

<!-- docgen:output exec="awk '/^services:/{s=1;next} /^[a-z]/{s=0} s && /^  [a-z][a-z0-9-]*:$/{print $1}' deploy/compose/compose.yaml | tr -d :" -->
```text
init
plugin
hermes-init
hermes
front
hub
sandbox
demo-llm
ollama
ollama-pull
honcho-db
honcho-migrate
honcho
honcho-deriver
honcho-embeddings
```
<!-- /docgen:output -->

## Helm chart

The chart is `deploy/charts/talaria`.

<!-- docgen:code file=deploy/charts/talaria/Chart.yaml -->
```yaml
apiVersion: v2
name: talaria
description: Talaria, a web and Telegram front end for Hermes Agent with paired computers and a sandbox
type: application
version: 0.1.0
# The talaria-hub release this chart was tested with
appVersion: "0.26.54"
home: https://github.com/USA-RedDragon/talaria
sources:
  - https://github.com/USA-RedDragon/talaria
kubeVersion: ">=1.29.0-0"
```
<!-- /docgen:code -->

### Helm values

Only `publicUrl` is required.

<!-- docgen:code file=deploy/charts/talaria/values.yaml -->
```yaml
# Talaria Helm chart values. Only publicUrl is required. Secrets and keys that Talaria needs inside are made on
# the first install and kept in the Secret <release>-talaria-keys, which uninstall leaves in place.

# -- The address people open, such as https://talaria.example.com. With kubectl port-forward on this computer:
# http://localhost:8080 (forward local port 8080, see the notes after install)
publicUrl: ""

# -- IANA time zone of schedules and the agent's clock, such as Europe/Berlin
timeZone: UTC

auth:
  # -- local: the hub's own accounts; the first person who opens Talaria makes the admin account on its setup
  # page. proxy: a reverse proxy that logs people in (Authelia, oauth2-proxy, Envoy Gateway) says who they are.
  # oidc: your OpenID Connect provider. See https://usa-reddragon.github.io/talaria/deploy/auth.html
  mode: local
  # -- The hub account of the person this Hermes works for (optional). Owns chats that are on no computer.
  # Empty: works while one person's computers are paired
  user: ""
  # -- Hub user names that are admins, besides accounts marked admin in Settings > Users
  admins: []
  # -- Addresses the hub reads the client address from X-Forwarded-For for: the front container in the same pod
  trustedProxies: ["127.0.0.1/32"]
  # -- The hub's auth.proxy settings, as they are, for mode proxy, such as {user_header: Remote-User} or a jwt
  # block. Only the proxy may reach the Service then: anyone else could send the header
  proxy: {}
  oidc:
    # -- Issuer URL of your identity provider (mode oidc)
    issuerUrl: ""
    clientId: ""
    # -- Client secret. Or name a Secret with key client-secret in existingSecret
    clientSecret: ""
    existingSecret: ""
    # -- More of the hub's oidc settings, as they are, such as {auto_create: true, allowed_domains: [example.com]}
    settings: {}
  # -- Identities from the provider or proxy and their hub user names, such as [{sub: "...", name: alice}].
  # Optional: an admin can also make accounts in Settings > Users
  users: []

# -- Contact address that push services get with Web Push, such as mailto:you@example.com. Empty: publicUrl
pushContact: ""

model:
  # -- Any OpenAI-compatible API, such as https://openrouter.ai/api/v1. Empty with demo.enabled: the demo model
  baseUrl: ""
  # -- Model name at that API
  name: ""
  # -- API key, or name a Secret with key api-key in existingSecret
  apiKey: ""
  existingSecret: ""
  # -- Context window in tokens; Hermes needs at least 64000
  contextLength: 131072

# -- A stand-in model that needs no API key, to try Talaria. Turn it off once model is set
demo:
  enabled: false

telegram:
  # -- Bot token from @BotFather, or a Secret with key token in existingSecret
  token: ""
  existingSecret: ""

images:
  hub: ghcr.io/usa-reddragon/talaria-hub:v0.26.54@sha256:832bd2db5b0aa144c28687990aca006cfda1a89268a73c1179b68e6fb4ea5504
  # -- Talaria Hermes: Hermes Agent with the Talaria patches (https://usa-reddragon.github.io/talaria/deploy/images.html)
  hermes: ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p30@sha256:4a5d4e2affd196ac92037e2d3e1e3fefbb32ab74f5467f5b324d0e401187cc0a
  device: ghcr.io/usa-reddragon/hermes-device:v0.25.3@sha256:c19e36561ac6b34798eea6464dfbf5f974b44102d30c1e1f789abc88c3a7eeb0
  sandbox: ghcr.io/usa-reddragon/hermes-device-sandbox:v0.25.3@sha256:ec2ffbec87ada250eec859e98467d91bf3cf1e59b65e158c5d3e0faec3cd2ebb
  caddy: docker.io/library/caddy:2.10-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d
  pullPolicy: IfNotPresent
# -- Secrets for private images, such as [{name: ghcr}]
imagePullSecrets: []

hub:
  # -- Replicas. More than 1 needs store postgres
  replicas: 1
  store:
    # -- sqlite (one replica, on the hub's volume) or postgres
    driver: sqlite
    # -- Secret with key dsn (a postgres:// URL) for driver postgres
    existingSecret: ""
  persistence:
    size: 1Gi
    storageClass: ""
  # -- Extra HUB_* environment variables, such as HUB_LOG_LEVEL: debug
  env: {}
  # -- PEM certificates of private authorities the hub trusts besides the usual public ones: for an OIDC provider
  # or a proxy's key set (jwks_url) whose certificates a private CA signs
  caBundle: ""
  resources:
    requests: {cpu: 10m, memory: 64Mi}
    limits: {memory: 1Gi}

hermes:
  persistence:
    size: 20Gi
    storageClass: ""
    # -- ReadWriteOnce: the rolling update starts the new pod on the old pod's node (pod affinity)
    accessMode: ReadWriteOnce
  # -- Zero-downtime rolling updates: the new pod waits as a standby and takes over in about a second, running
  # turns finish in the old pod, and approvals stay answerable. Needs the patched Hermes image
  rolling:
    enabled: true
    # -- Longest a stop waits for running turns plus the handover; keep it above 540 s plus the drain
    terminationGracePeriodSeconds: 780
  # -- Your own Hermes config.yaml settings, merged over Talaria's on every start
  config: {}
  # -- Extra environment variables for Hermes, such as provider keys
  env: {}
  resources:
    requests: {cpu: 100m, memory: 512Mi}
    limits: {memory: 8Gi}

sandbox:
  persistence:
    size: 10Gi
    storageClass: ""
  resources:
    requests: {cpu: 50m, memory: 128Mi}
    limits: {memory: 8Gi}

service:
  type: ClusterIP
  port: 8080

ingress:
  # -- An Ingress for publicUrl's host
  enabled: false
  className: ""
  annotations: {}
  # -- TLS secret name; empty: no tls block
  tlsSecret: ""

httpRoute:
  # -- A Gateway API HTTPRoute for publicUrl's host
  enabled: false
  # -- Gateways to attach to, such as [{name: public, namespace: gateway, sectionName: https}]
  parentRefs: []
```
<!-- /docgen:code -->

## All-in-one image

It reads the same `TALARIA_*` names as compose. The settings of the Unraid template:

<!-- docgen:output exec="grep -o 'Target=\"[A-Z_]*\"' aio/unraid/talaria.xml | sed 's/Target=\"//;s/\"//'" -->
```text

```
<!-- /docgen:output -->

## Images

Every install pulls these, pinned by digest. [Images](images.md) says what each one is.

<!-- docgen:output exec="grep -oE '(ghcr\.io|docker\.io)/[a-z0-9./_-]+:[A-Za-z0-9._-]+' deploy/compose/compose.yaml | sort -u" -->
```text
docker.io/library/caddy:2.10-alpine
docker.io/ollama/ollama:0.40.16
docker.io/pgvector/pgvector:0.8.1-pg17
ghcr.io/huggingface/text-embeddings-inference:cpu-1.9.4
ghcr.io/plastic-labs/honcho:v3.6.0
ghcr.io/usa-reddragon/hermes-device-sandbox:v0.25.3
ghcr.io/usa-reddragon/hermes-device:v0.25.3
ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p30
ghcr.io/usa-reddragon/talaria-hub:v0.26.54
```
<!-- /docgen:output -->
