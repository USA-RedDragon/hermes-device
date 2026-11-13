# API overview

The hub's HTTP API is the API of its own web page. It is not a separate, versioned public API: the page and the Telegram bot use it, and it changes with the hub. Use it for scripts on your own hub, and read the code (`internal/web`) when this page is not enough. Every path below is relative to the hub's public address, such as `https://hub.example.com`.

## Signing in

Every `/api/` path needs the login session cookie, which the hub sets after a login. The cookie is `hub_session`, or `__Host-hub_session` when `http.public_url` is `https`. There are no API keys or bearer tokens for these paths. For a script, log in once in a browser and copy the cookie from its developer tools, and it works until the session ends (`session.ttl`, a week by default).

Two more rules apply to every request:

- A request that is not a `GET` needs the header `X-CSRF-Token`, with the value of `csrf` from `GET /api/me`.
- The `Origin` header, when present, must be the hub's public address. A request from another site gets `403`.

```sh
COOKIE='__Host-hub_session=...'
curl -fsS -b "$COOKIE" https://hub.example.com/api/me
curl -fsS -b "$COOKIE" https://hub.example.com/api/sessions

CSRF=$(curl -fsS -b "$COOKIE" https://hub.example.com/api/me | jq -r .csrf)
curl -fsS -b "$COOKIE" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"text":"What is in my folder?"}' \
  https://hub.example.com/api/sessions/alice-backend/SESSION_ID/prompt
```

Errors are JSON, `{"error": "..."}`, with the usual status: `401` when the login is missing or expired, `403` for a missing CSRF token or a backend you may not use, `404` for an unknown chat or backend, `409` when the chat is busy, and `503` while a backend restarts.

## What there is

| Area | Paths |
| ---- | ----- |
| Who you are | `GET /api/me` (user, groups, the CSRF token, which features are on), `POST /auth/logout` |
| Backends | `GET /api/backends`, `GET /api/backends/{backend}/models` |
| Chats | `GET /api/sessions`, `POST /api/sessions`, `GET /api/sessions/{backend}/{session}/transcript` |
| One chat | under `/api/sessions/{backend}/{session}/`: `events` (the live stream), `prompt`, `interrupt`, `approvals/{approval}`, `rename`, `archive`, `delete`, `model`, `reasoning`, `draft`, `pin`, `read`, `rewind`, `branch`, `activity` |
| Where a chat runs | `GET /api/computers`, `POST /api/places`, and per chat `place`, `move`, `release` |
| Files and pages | per chat `media`, `page`, `folder`, `file`, `diff`, `changes` |
| Chats that work together | `GET` and `POST /api/backends/{backend}/partners`, `DELETE /api/backends/{backend}/partners/{a}/{b}` |
| Saved prompts | `GET /api/prompts`, `POST /api/prompts`, `PUT` and `DELETE /api/prompts/{name}` |
| Memory | `/api/memory` and `/api/memory/{backend}/notes`, `projects`, `learned` |
| Scheduled jobs | `/api/scheduled` and the jobs routes under it |
| Search and usage | `GET /api/search`, `GET /api/usage/{backend}` |
| Notifications | `/api/notify`, `/api/notify/settings`, `/api/notify/push` and `/api/notify/test` |
| Computers | `GET /api/devices`, `POST /api/devices/{device}/rename` and `/revoke` |

The README describes the behavior of the chat endpoints in detail, with their request and reply fields: see [The hub API for writing and the chat list](../README.md#the-hub-api-for-writing-and-the-chat-list), [Attachments](../README.md#attachments) and [Activity](../README.md#activity).

## The event stream

`GET /api/sessions/{backend}/{session}/events` is a long-lived server-sent event stream: a snapshot of the chat first, then every change as it happens, with a comment line every 15 seconds so idle timeouts above that are safe. Keep the connection open for as long as you follow the chat. A proxy in front of the hub must not buffer it and must not time it out: see reverse proxy notes in the [README](../README.md#deploy).

## Computers talk to a different API

The `talaria` program does not use the cookie. It pairs once and then holds a device token for the paths under `/api/device/`: `pair` and `pair/token` (the pairing flow), `whoami`, `exec`, `acp` and `gateway` (WebSockets), the session binding and `usage`. The protocol is in the talaria repository's [protocol page](https://github.com/USA-RedDragon/talaria/blob/main/docs/protocol.md).

## Health and metrics

| Path | Answers |
| ---- | ------- |
| `GET /healthz` | `ok`, with no login, while the process runs |
| `GET /readyz` | `ok` once the hub has started, and `503` from the moment it starts to shut down. Use it as the readiness probe |
| `GET /metrics` | Prometheus metrics, when `http.metrics.enabled` is on. It is on its own port (loopback by default), not the public one |
