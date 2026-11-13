# Troubleshooting

Start with what is running and what it says.

```bash
docker compose ps -a                 # every service, and why one stopped
docker compose logs init             # first start: the address and login
docker compose logs hub hermes       # the two parts you talk to
```

On Kubernetes: `kubectl -n talaria get pods`, `kubectl -n talaria logs deploy/talaria-hermes` and `kubectl -n talaria logs deploy/talaria-hub -c hub`.

## The images will not pull

- `pull access denied`: every Talaria image is public and needs no login. Check the image names you set (`TALARIA_HERMES_IMAGE` in compose, `images.*` in Helm) and that the host reaches ghcr.io. See [Images](images.md).
- On Kubernetes, the pod is in `ImagePullBackOff`: the same checks; a pull secret (`imagePullSecrets`) is only needed for an image of your own in a private registry.

## I cannot log in

- You did not see the password: `docker compose logs init` shows it again. Helm: read it from the `<release>-keys` Secret.
- The login page says the address is wrong, or the hub log shows `oidc discovery ... connection refused`: `TALARIA_PUBLIC_URL` does not match how you open Talaria. Use the same address in both, then `docker compose up -d`. The hub itself must be able to open that address, because the built-in login is there.
- A redirect to the wrong host after login: the same cause. The hub accepts requests only for its public URL.
- With your own OIDC provider, the hub refuses you and logs a `sub`: add that `sub` to the user list. See [Login options](auth.md#your-own-oidc-provider).

## Chats do not stream, or stop after a while

Behind your own reverse proxy, turn off response buffering and request timeouts for the hub. Replies stream as server-sent events, and the device connections on `/api/device/` are WebSockets. In nginx that is `proxy_buffering off;`, a long `proxy_read_timeout` and the WebSocket upgrade headers. With a Gateway API route, set `timeouts.request: 0s`.

## The model does not answer

- "Context too small" in the chat: the model's window is under 64,000 tokens. Pick another model, or set `TALARIA_MODEL_CONTEXT` if your service allows more than it reports. Ollama's `qwen3` models have 40K.
- An error with a status like 401 or 404 from the model: the base URL, the model name or the key is wrong. Check `.env`, then `docker compose up -d`. See [Choosing models and providers](models.md).
- A local model is very slow: expected without a GPU. See [Choosing models and providers](models.md#local-models-with-ollama).
- Still the canned reply: the `demo` profile is still on, or the model variables are empty.

## Pages and notifications do not work

Pages (the agent's HTML pages) and Web Push need HTTPS or `localhost`. On a plain `http://<LAN address>` they stay off. Put the hub behind HTTPS: see [Reach it from other computers](compose.md#reach-it-from-other-computers).

## Notifications

- No "Turn on for this device" button: the page is not on HTTPS or `localhost`, or the hub has no Web Push key. The install files make the key on the first start; check `docker compose logs hub` for a line about Web Push.
- The hub logs that it runs without Web Push: the public URL is `http://` and no `notify.vapid_subject` is set. The compose files set it for you.
- A browser stopped getting notices after an upgrade: the key changed. Keep `talaria_secrets` (or the `<release>-keys` Secret) across upgrades and backups.

## A computer will not pair or shows offline

See the `talaria` [troubleshooting](../guides/troubleshooting.md) page. The usual causes: the computer cannot open the hub's public address, the pairing code expired (10 minutes), or the service on the computer is not running. In the hub, Settings, Computers shows when each computer last connected and how the last connection ended.

## Telegram does not answer

- Check the token with `curl https://api.telegram.org/bot<token>/getMe`.
- Only one process can read a bot's messages. Stop any other copy that uses the same token.
- You must link your account first: send `/link` to the bot and confirm on the hub page.

## Memory (Honcho) is empty

Honcho needs a few chats before it writes facts. Check `docker compose logs honcho honcho-deriver`. If the embedding dimensions in `.env` do not match the model, Honcho fails: they cannot change after it stored facts.

## Out of memory or disk

Hermes uses about 275 MB idle and up to about 1 GB during a turn, plus the hub (about 40 MB) and the sandbox (a few MB). The Hermes data grows with use: a few GB after weeks. `docker system df` and `docker volume ls` show what is big. Do not remove `talaria_*` volumes: they hold your chats.

## Start over

This deletes all chats and settings:

```bash
docker compose down -v
```

Helm: `helm uninstall talaria -n talaria`, then delete the PVCs and the `<release>-keys` Secret, which uninstall keeps.

## Report a problem

Open an issue in [talaria](https://github.com/USA-RedDragon/talaria/issues) with the install path, the output of `docker compose ps -a` (or `kubectl get pods`) and the relevant log lines. Remove tokens and passwords from anything you paste.
