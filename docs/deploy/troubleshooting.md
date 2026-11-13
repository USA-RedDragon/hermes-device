# Fix server problems

Admins can start in the hub: **Settings, Diagnostics** shows whether each part is working (the hub, the agent,
the sandbox, your computers, Telegram), runs a health check of the agent, and shows the newest lines of the agent's
log with a filter for warnings and errors. **Copy support info** copies all of it for a bug report.

On the server, start with what is running and what it says.

```bash
docker compose ps -a                 # every service, and why one stopped
docker compose logs init             # first start: the address
docker compose logs hub hermes       # the two parts you talk to
```

On Kubernetes: `kubectl -n talaria get pods`, `kubectl -n talaria logs deploy/talaria-hermes` and `kubectl -n talaria logs deploy/talaria-hub -c hub`.

## The images will not pull

- `pull access denied`: every Talaria image is public and needs no login. Check the image names you set (`TALARIA_HERMES_IMAGE` in compose, `images.*` in Helm) and that the host reaches ghcr.io. See [Images](images.md).
- On Kubernetes, the pod is in `ImagePullBackOff`: the same checks; a pull secret (`imagePullSecrets`) is only needed for an image of your own in a private registry.

## I cannot log in

- The setup page says the hub is already set up, and you did not make the account: someone else opened it first.
  Stop Talaria, and if you do not know who that was, start over with `docker compose down -v` (this deletes
  everything), then keep the port closed to others until you made the account.
- You forgot the password: another admin sets a new one under Settings, Users. With no other admin, restore a
  backup or start over (no shell reset command yet).
- Locked after 5 wrong passwords: wait 15 minutes, or an admin sets a new password.
- A redirect to the wrong host after login: `TALARIA_PUBLIC_URL` does not match how you open Talaria. Use the same
  address in both, then `docker compose up -d`.
- With your own OIDC provider, the hub refuses you and logs a `sub`: make an account with their email, turn on `auto_create`, or list the `sub`. See [Set up logins](auth.md#your-own-oidc-provider).

## Chats do not stream, or stop after a while

Behind your own reverse proxy, turn off response buffering and request timeouts for the hub. Replies stream as server-sent events, and the device connections on `/api/device/` are WebSockets. In nginx that is `proxy_buffering off;`, a long `proxy_read_timeout` and the WebSocket upgrade headers. With a Gateway API route, set `timeouts.request: 0s`.

## The model does not answer

- "Talaria has no model yet" (or, for people who are not admins, "No model is set up yet") in the chat area, or
  the model menu is empty: no model is connected yet. This is the required "Connect a model" step of your install
  guide. An admin clicks **Connect a model** there, or opens Settings, Models ([Choosing models and
  providers](models.md#connect-a-model-from-talaria)). Or run `hermes model` inside Talaria's container (Talaria
  has its own agent; nothing else needs to be installed): [Compose](compose.md#step-4-connect-a-model),
  [Unraid](unraid.md), [TrueNAS](truenas.md), [all-in-one](all-in-one.md), [Helm](helm.md#quick-start-try-it-with-port-forward),
  and [Choosing models and providers](models.md#set-a-model-on-the-server).
- Connect a model is not there: only hub admins see it. The first account (made on the setup page) is one; others
  are made admins in Settings, Users, or named in `auth.admins` (login proxy, OIDC). With `models.admins` set, only
  the `user=backend` pairs it names see it, for those backends.
- A model, server or key in Settings, Models says "Set in the deployment config" or "Set in the deployment", with
  no Edit or Remove: the deployment sets it (its managed Hermes config or the container's environment). Change it
  there ([Models the deployment sets](models.md#models-the-deployment-sets)).
- "Could not reach http://localhost:11434/v1/models." when you test an address: The agent runs in its own container,
  so `localhost` is that container. Use the address of the machine that runs the server, and let the server
  listen on it (Ollama: `OLLAMA_HOST=0.0.0.0:11434`).
- "That API key was rejected": the provider refused the key, and nothing was saved. A key "saved, but not
  checked" is one the agent cannot check before it saves it; if replies then fail with an authentication error, edit
  the key in Settings, Models.
- "The sign-in code ran out": a code works for 15 minutes, and a restart of the agent forgets it. Click **Get a new code**.
- "Could not reach the agent" in Connect a model or Settings, Models: the agent is restarting or down. Try again in a moment.
- "Context too small" in the chat, or Talaria warns that a model's window is under 64,000 tokens: pick another
  model (Settings, Models, or `hermes model`). In Ollama, `qwen3` has 40K and `qwen2.5` 32K.
- An error with a status like 401 or 404 from the model: the address, the model name or the key the agent has is
  wrong. Test it again in Settings, Models, or run `hermes model` again.
- A local model is very slow: expected without a GPU. See [Choosing models and providers](models.md#ollama).
- Still the canned reply: the agent still uses `talaria-demo`. Pick another model in `hermes model`.

## Pages and notifications do not work

Pages (the agent's HTML pages) and Web Push need HTTPS or `localhost`. On a plain `http://<LAN address>` they stay off. Put the hub behind HTTPS: see [Reach it from other computers](compose.md#reach-it-from-other-computers).

## Notifications

- No "Turn on for this device" button: the page is not on HTTPS or `localhost`, or the hub has no Web Push key. The install files make the key on the first start; check `docker compose logs hub` for a line about Web Push.
- The hub logs that it runs without Web Push: the public URL is `http://` and no `notify.vapid_subject` is set. The compose files set it for you.
- A browser stopped getting notices after an upgrade: the key changed. Keep `talaria_secrets` (or the `<release>-keys` Secret) across upgrades and backups.

## A computer will not pair or shows offline

See the `talaria` [troubleshooting](../guides/troubleshooting.md) page. The usual causes: the computer cannot open the hub's public address, the pairing code expired (10 minutes), or the service on the computer is not running. In the hub, Settings, Computers shows when each computer last connected and how the last connection ended.

## Telegram does not answer

- Check the token in Settings, Telegram with **Test**, or with `curl https://api.telegram.org/bot<token>/getMe`.
- Only one process can read a bot's messages. Stop any other copy that uses the same token.
- You must link your account first: send `/link` to the bot and confirm on the hub page.

## Memory (Honcho) is empty

Honcho needs a few chats before it writes facts. Check `docker compose logs honcho honcho-deriver`. If the embedding dimensions in `.env` do not match the model, Honcho fails: they cannot change after it stored facts.

## Out of memory or disk

The agent uses about 275 MB idle and up to about 1 GB during a turn, plus the hub (about 40 MB) and the sandbox (a few MB). The agent's data grows with use: a few GB after weeks. `docker system df` and `docker volume ls` show what is big. Do not remove `talaria_*` volumes: they hold your chats.

## Start over

This deletes all chats and settings:

```bash
docker compose down -v
```

Helm: `helm uninstall talaria -n talaria`, then delete the PVCs and the `<release>-keys` Secret, which uninstall keeps.

## Report a problem

Open an issue in [talaria](https://github.com/USA-RedDragon/talaria/issues) with the install path, the output of `docker compose ps -a` (or `kubectl get pods`) and the relevant log lines. Remove tokens and passwords from anything you paste.
