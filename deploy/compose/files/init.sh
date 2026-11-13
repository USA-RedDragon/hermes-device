#!/bin/sh
# Talaria first-run step. Runs as root in a small container before the other services.
#
# 1. Makes every secret and key that is missing, once, in the secrets volume.
# 2. Writes the hub's config file from those secrets and the settings in .env, on every start, so a change in
#    .env takes effect.
# 3. Gives each data volume to the user that its service runs as.
# 4. Prints the address to open.
#
# There is no password here: the hub has its own accounts, and the first person who opens it makes the admin
# account on its setup page. It never changes a secret that exists. To start over, remove the volumes:
#   docker compose down -v
set -eu

# Paths and owners. The defaults fit compose.yaml; the all-in-one image sets its own
S=${TALARIA_SECRETS:-/secrets}
HUB_DATA=${TALARIA_HUB_DATA:-/hub-data}
WORK=${TALARIA_WORK:-/work}
HERMES_URL=${TALARIA_HERMES_URL:-http://hermes:9120}
# The acp-host that runs next to Hermes, for editors (talaria acp). Empty: the hub has no editor bridge
ACP_URL=${TALARIA_ACP_URL-http://hermes:9130}
HUB_UID=${TALARIA_HUB_UID:-65534}
HERMES_UID=${TALARIA_HERMES_UID:-10000}
SANDBOX_UID=${TALARIA_SANDBOX_UID:-568}
SEARXNG_UID=${TALARIA_SEARXNG_UID:-977}
umask 077
mkdir -p "$S/generated" "$S/hub" "$S/hermes" "$S/sandbox" "$S/searxng"

rand() { head -c "$1" /dev/urandom | base64 | tr -d '\n=' | tr '+/' '-_'; }

# gen NAME COMMAND...: writes the output of COMMAND to generated/NAME once
gen() {
  name=$1; shift
  [ -s "$S/generated/$name" ] && return 0
  "$@" > "$S/generated/$name.tmp"
  mv "$S/generated/$name.tmp" "$S/generated/$name"
  echo "init: made $name"
}

# Ed25519 seed (32 bytes, base64) that signs the hub's device assertions
gen hub-signing-seed sh -c 'head -c 32 /dev/urandom | base64 | tr -d "\n"'
# Web Push (VAPID) P-256 private key, raw 32 bytes in base64url
gen vapid-key sh -c 'head -c 32 /dev/urandom | base64 | tr -d "\n=" | tr "+/" "-_"'
gen session-secret rand 48
# Password the hub signs in to Hermes with (Hermes dashboard basic auth, user "hub")
gen hermes-password rand 32
gen hermes-session-secret rand 48
# Token of the sandbox, a service device of the hub
token() { printf 'hdt_%s' "$(rand 33)"; }
gen sandbox-token token
# Key that SearXNG (the agent's web search) signs its cookies with
gen searxng-secret rand 48

g() { cat "$S/generated/$1"; }

PUBLIC_URL=${TALARIA_PUBLIC_URL:-http://localhost:8080}
PUBLIC_URL=${PUBLIC_URL%/}
# The hub account of the person this Hermes works for (optional): the Honcho peer name, and the owner of chats
# that the device plugin cannot tie to a computer
USER_NAME=${TALARIA_USER:-}

# --- hub: backends can only be set in its config file. Login settings (auth.mode and the rest) come from the
# hub's own HUB_AUTH_* and HUB_OIDC_* variables, see https://usa-reddragon.github.io/talaria/deploy/auth.html ---
cat > "$S/hub/config.yaml.tmp" <<EOF
auth:
  # Caddy (front) runs in the hub's network namespace: read the client's address from X-Forwarded-For
  trusted_proxies: [127.0.0.1/32]
session:
  secret: $(g session-secret)
device:
  signing_key: $(g hub-signing-seed)
notify:
  vapid_private_key: $(g vapid-key)
EOF
if [ -n "${TALARIA_PUSH_CONTACT:-}" ]; then
  # Push services want a mailto: or https: contact; the default is the public URL when that is https
  printf '  vapid_subject: %s\n' "$TALARIA_PUSH_CONTACT" >> "$S/hub/config.yaml.tmp"
fi
cat >> "$S/hub/config.yaml.tmp" <<EOF
backends:
  - id: main
    name: ${TALARIA_BACKEND_NAME:-Talaria}
    url: $HERMES_URL
    username: hub
    password: $(g hermes-password)
    devices: true
    media_roots: [/opt/data/cache]
    service_devices:
      - id: sandbox
        name: Sandbox
        token: $(g sandbox-token)
EOF
if [ -n "$ACP_URL" ]; then
  printf '    acp_url: %s\n' "$ACP_URL" >> "$S/hub/config.yaml.tmp"
fi
if [ -n "${TALARIA_HONCHO_URL:-}" ]; then
  # Shows what Honcho learned on the hub's Memory page. The compose Honcho runs without auth
  cat >> "$S/hub/config.yaml.tmp" <<EOF
    honcho:
      url: $TALARIA_HONCHO_URL
      workspace: talaria
      user_peer: ${USER_NAME:-user}
      ai_peer: hermes
      api_key: ${TALARIA_HONCHO_API_KEY:-none}
EOF
fi
if [ -n "${TALARIA_TELEGRAM_TOKEN:-}" ]; then
  printf 'telegram:\n  enabled: true\n  token: %s\n' "$TALARIA_TELEGRAM_TOKEN" >> "$S/hub/config.yaml.tmp"
fi
mv "$S/hub/config.yaml.tmp" "$S/hub/config.yaml"

# --- Hermes: what its own init step needs ---
cp "$S/generated/hub-signing-seed" "$S/hermes/hub-signing-key"
cp "$S/generated/hermes-password" "$S/hermes/password"
cp "$S/generated/hermes-session-secret" "$S/hermes/session-secret"
printf '%s' "$USER_NAME" > "$S/hermes/user"

# --- sandbox ---
cp "$S/generated/sandbox-token" "$S/sandbox/token"

# --- searxng ---
cp "$S/generated/searxng-secret" "$S/searxng/secret"

# Each service reads only its own part
mkdir -p "$HUB_DATA" "$WORK"
chown -R "$HUB_UID:$HUB_UID" "$S/hub" "$HUB_DATA"
# Caddy (compose only) runs unprivileged, as the hub's user; its certificates live here
if [ -d "${TALARIA_CADDY_DATA:-/caddy-data}" ]; then chown -R "$HUB_UID:$HUB_UID" "${TALARIA_CADDY_DATA:-/caddy-data}"; fi
chown -R "$HERMES_UID:$HERMES_UID" "$S/hermes"
chown -R "$SANDBOX_UID:$SANDBOX_UID" "$S/sandbox"
chown -R "$SEARXNG_UID:$SEARXNG_UID" "$S/searxng"
chown "$SANDBOX_UID:$SANDBOX_UID" "$WORK"
# The Hermes home, so its init step can run before the Hermes image's own start-up script
if [ -n "${TALARIA_HERMES_DATA:-/hermes-data}" ] && [ -d "${TALARIA_HERMES_DATA:-/hermes-data}" ]; then
  H=${TALARIA_HERMES_DATA:-/hermes-data}
  [ "$(stat -c %u "$H")" = "$HERMES_UID" ] || chown -R "$HERMES_UID:$HERMES_UID" "$H"
fi
chmod 711 "$S"
chmod 700 "$S/generated" "$S/hub" "$S/hermes" "$S/sandbox" "$S/searxng"
chmod 400 "$S/sandbox/token"

cat <<EOF

==================================================================
 Talaria is starting.

 Open:  $PUBLIC_URL

 The first person who opens it makes the admin account. Do that
 now, before Talaria is reachable from the internet.

 Show this again: ${TALARIA_SHOW_AGAIN:-docker compose logs init}
==================================================================

EOF
