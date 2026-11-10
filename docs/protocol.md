# hermes-device protocol

Status: phase 1, protocol version 2. Pinned to Hermes Agent v0.22.7.

This document defines every message and endpoint between the device client (`hermes-device`), hermes-hub and the Hermes side (the device plugin in the gateway process, and `acp-host` in the Hermes pod). The hub implements its side from this document. Where this document and code disagree, fix the code or this document in the same change.

The words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

Shared wire names live in Go in package `protocol` and the hub assertion in package `assertion`. Both are public packages of this module, so hermes-hub can import them. The Python plugin repeats the same names in `plugin/device/core.py` and `plugin/device/assertion.py`, and `protocol/contract_test.go` checks that the two agree.

## 1. Parts and legs

```text
device (this PC)                          hermes-hub                         user's Hermes pod
----------------                          ----------                         -----------------
hermes-device run  ==exec leg==========>  /api/device/exec    ==assertion==>  gateway :9120 /api/plugins/device/ws (plugin)
  loopback splice  ==gateway leg=======>  /api/device/gateway ==ticket=====>  gateway :9120 /api/ws (canonical)
    ^ hermes (Ink TUI), started by hermes-device chat
hermes-device acp  ==ACP leg===========>  /api/device/acp     ==assertion==>  acp-host :9130 /acp -> hermes acp (stdio)
hermes-device login, run, chat, acp ===>  /api/device/pair, /pair/token, /whoami, /sessions/{id}/binding (HTTPS)
                                          hub ==dashboard session==>          gateway :9120 /api/plugins/device/* (HTTP)
```

- The device dials only the hub. It never reaches a Hermes pod directly.
- The hub authenticates the device by its device token on every request and upgrade, maps the device to the one backend it was paired with, and checks again on every connect that the device's user still has access to that backend.
- On the exec and ACP legs the hub adds a hub assertion (section 3) to its upgrade request to the backend. On the gateway leg it uses its own gateway credentials (a socket ticket), as it does for its web UI.
- After an upgrade the hub copies WebSocket messages between the two hops unchanged, in order, text as text. It does not parse exec or ACP messages. It closes the other hop when one hop closes, and relays the close code (section 2.3).

## 2. Common rules

### 2.1 Device token

- Format: `hdt_` followed by 43 characters of base64url without padding (32 random bytes). The hub generates it at pairing and stores only its SHA-256.
- The device sends it as `Authorization: Bearer <token>` on every hub HTTP request and WebSocket upgrade.
- The hub answers `401` with `{"error":"unauthorized"}` (HTTP), or refuses the upgrade with HTTP `401`, when the token is unknown or revoked. The client then stops and tells the user to run `hermes-device login`.
- The client keeps the token in the OS keyring (service `hermes-device`, account = the hub URL's host) when one is available, else in a file with mode `0600` (default `<user config dir>/hermes-device/token`).
- The hub records the device's last-seen time and remote address on every authenticated request and upgrade, and shows "reconnected from a new address" when the address changes.

### 2.2 Pings

Every WebSocket hop on every leg (device to hub, hub to backend) sends WebSocket ping control frames from both ends:

| Setting | Value |
|---|---|
| Ping interval | <!-- docgen:const pkg=protocol name=PingInterval print=config -->`25s`<!-- /docgen:const --> |
| Pong deadline | <!-- docgen:const pkg=protocol name=PingTimeout print=config -->`15s`<!-- /docgen:const --> |

A side that gets no pong within the deadline closes the hop with code `4408` (or drops the TCP connection if the close cannot be sent). The exec leg also has an end-to-end application ping (section 4.6), because a healthy device-to-hub hop says nothing about the plugin.

Any HTTP proxy in front of the hub or the backend (for example a Gateway API HTTPRoute) MUST NOT time out these upgrades: give `/api/device/*` its own route rule with request and idle timeouts off. The client reconnects on any drop; phase 1 loses commands in flight on a drop (section 4.7).

### 2.3 Close codes

The hub relays the backend's close code and reason to the device unchanged. Codes the hub itself uses are marked.

| Code | Name | Sent by | Client action |
|---|---|---|---|
| `1000`, `1001` | normal, going away | any | reconnect with backoff |
| <!-- docgen:const pkg=protocol name=CloseUnauthorized code=false -->4401<!-- /docgen:const --> | unauthorized | hub (token), plugin (dev static token) | stop, ask for login |
| <!-- docgen:const pkg=protocol name=CloseAssertionRefused code=false -->4403<!-- /docgen:const --> | assertion refused | plugin, acp-host | reconnect with backoff (a hub or pod misconfiguration) |
| <!-- docgen:const pkg=protocol name=ClosePingTimeout code=false -->4408<!-- /docgen:const --> | ping timeout | any | reconnect |
| <!-- docgen:const pkg=protocol name=CloseReplaced code=false -->4409<!-- /docgen:const --> | replaced | plugin | `run` stops: another daemon with the same token took over |
| <!-- docgen:const pkg=protocol name=CloseUnsupportedVersion code=false -->4426<!-- /docgen:const --> | unsupported version | plugin | stop, tell the user to upgrade |
| <!-- docgen:const pkg=protocol name=CloseUnsupportedPlatform code=false -->4427<!-- /docgen:const --> | unsupported platform | plugin | stop: the device's OS (Windows) is not supported |
| <!-- docgen:const pkg=protocol name=CloseBackendUnavailable code=false -->4502<!-- /docgen:const --> | backend unavailable | hub | reconnect with backoff |
| `1008` | policy | Hermes plugin gate | reconnect with backoff (plugin disabled) |
| `1011` | `hermes acp` exited | acp-host | reconnect with backoff |
| `1013` | try again later | acp-host (process limit, or no gateway) | reconnect with backoff |
| `1002` | protocol error | plugin (no hello in time) | reconnect with backoff |

The hub accepts the device's upgrade only after its own upgrade to the backend succeeded. When the backend refuses or cannot be reached (an HTTP error, a refused connection, a timeout), the hub answers the device's upgrade with HTTP `502` and `{"error": "backend_unavailable"}`, and the client reconnects with backoff. Any HTTP answer other than `401` to an upgrade means reconnect with backoff.

Backoff: start at `reconnect.min-backoff` (default 1s), double after each failed attempt up to `reconnect.max-backoff` (default 30s), reset after a connection that stayed up 30s.

### 2.4 JSON

All messages are UTF-8 JSON objects, one per WebSocket text message. Receivers MUST ignore unknown fields. On the exec leg, receivers MUST ignore messages of an unknown `type`.

## 3. Hub assertion

The hub proves to the backend that one upgrade request comes from one paired device of one user. The hub holds an Ed25519 private key. The pod holds only public keys, so a compromised pod cannot mint assertions for any backend or device. (A compromised pod is still operator access to that user's own Hermes; see section 10.)

### 3.1 Encoding

Header name: `X-Hermes-Device-Assertion` (<!-- docgen:const pkg=assertion name=Header -->`X-Hermes-Device-Assertion`<!-- /docgen:const -->).

```text
value     = "v1." payload "." signature
payload   = base64url(no padding) of the UTF-8 JSON claims object
signature = base64url(no padding) of the 64-byte Ed25519 signature
            over the ASCII bytes of "v1." payload
```

The value is at most <!-- docgen:const pkg=assertion name=MaxEncodedLen code=false -->4096<!-- /docgen:const --> bytes. All lengths in this section count bytes of UTF-8. Verifiers decode the JSON only after the signature verifies.

### 3.2 Claims

| Claim | Type | Meaning |
|---|---|---|
| `typ` | string | Always `hermes-device-assertion` |
| `aud` | string | The backend id. Each pod is configured with its own id (plugin setting `backend_id`, acp-host `acp-host.backend-id`) and refuses any other |
| `leg` | string | `exec` (plugin) or `acp` (acp-host) |
| `htm` | string | HTTP method of the upgrade: `GET` |
| `htp` | string | Exact request path of the upgrade, without query: `/api/plugins/device/ws` or `/acp` |
| `sub` | string | The hub's user id, 1 to 256 characters |
| `device_id` | string | The hub's device id, `[A-Za-z0-9_-]{1,64}`. This is the device's identity everywhere on the backend |
| `device_name` | string | The name the user gave the device at pairing, 1 to 64 characters, for display |
| `iat` | integer | Issue time, Unix seconds |
| `exp` | integer | Expiry, Unix seconds |
| `nonce` | string | At least 16 random bytes, base64url without padding (`[A-Za-z0-9_-]{22,128}`) |

The hub signs a new assertion for every upgrade, with `iat` = now and `exp` = now + 30s or less.

### 3.3 Verification

A verifier MUST refuse the upgrade, by accepting it and closing at once with code `4403` (or by an HTTP `403` before the upgrade), unless all of these hold:

1. The header is present, at most 4096 bytes, starts with `v1.`, and has exactly two `.`-separated parts after it that decode as base64url.
2. The signature verifies with one of the configured public keys.
3. `typ`, `aud`, `leg`, `htm` and `htp` equal the expected values for this endpoint. `htp` is compared to the path the request actually arrived on.
4. `exp >= iat`, `exp - iat <= 30`, `iat <= now + 5`, `now <= exp + 5` (5 seconds of clock skew, <!-- docgen:const pkg=assertion name=ClockSkew print=config -->`5s`<!-- /docgen:const -->).
5. `sub`, `device_id`, `device_name` and `nonce` match their formats above.
6. The nonce was not seen before. Verifiers remember each accepted nonce until `exp + 5` and refuse a repeat. A nonce is recorded only when every other check passes.

The assertion is bound to the upgrade by `aud`, `leg`, `htm`, `htp`, the 30-second lifetime and the single-use nonce. It does not cover `Sec-WebSocket-Key`, because the hub's WebSocket library generates that key itself.

### 3.4 Public keys

Verifiers read one or more keys, in either form, mixed:

- PEM blocks of type `PUBLIC KEY` (SubjectPublicKeyInfo, as `openssl pkey -pubout` writes them).
- Lines of base64 (standard or URL alphabet, padding optional) holding the 32 raw key bytes.

Blank lines and lines starting with `#` are ignored. Several keys allow rotation: the hub signs with the new key once every pod has both.

- Plugin: setting `hub_public_key_file` (a path), else the env var named by `hub_public_key_env` (default `HERMES_DEVICE_HUB_PUBLIC_KEY`).
- acp-host: `acp-host.hub-public-key-file`, else `acp-host.hub-public-key`.

## 4. Exec leg

Device to hub: `GET wss://<hub>/api/device/exec` with the device token. Hub to plugin: `GET ws://<backend>:9120/api/plugins/device/ws` (<!-- docgen:const pkg=protocol name=PluginExecPath -->`/api/plugins/device/ws`<!-- /docgen:const -->) with the assertion (`leg` `exec`). The hub MUST NOT send an `Origin` header and MUST send a `Host` header the gateway accepts (the gateway binds 0.0.0.0, which accepts any Host).

Protocol version: <!-- docgen:const pkg=protocol name=Version code=false -->2<!-- /docgen:const -->.

### 4.1 Handshake

1. The plugin verifies the assertion before it reads any message. On failure it closes with `4403`.
2. The device sends `hello` within <!-- docgen:const pkg=protocol name=HelloTimeout print=config -->`10s`<!-- /docgen:const --> of the upgrade. Otherwise the plugin closes with `1002`.
3. If `hello.version` is not 2 the plugin closes with `4426`.
4. The plugin answers `welcome`. From then on either side may send.
5. If a connection for the same `device_id` exists, the plugin fails its pending commands (exit 255) and closes it with `4409`. Newest wins.

`hello` (device to plugin):

```json
{
  "type": "hello",
  "version": 2,
  "roots": ["/home/alice/src"],
  "tmp_dir": "/home/alice/.cache/hermes-device/tmp",
  "home": "/home/alice",
  "hostname": "desktop",
  "os": "linux",
  "arch": "amd64",
  "shell": "bash",
  "client_version": "v0.2.0"
}
```

| Field | Meaning |
|---|---|
| `roots` | Absolute directories, resolved through symlinks, that commands may run in. The device refuses any `exec` whose `cwd` is outside them |
| `tmp_dir` | Absolute directory Hermes uses on the device for its shell snapshot files |
| `home` | The device user's `$HOME`. The plugin checks writes against the credential denylist relative to it (section 8.6) |
| `hostname`, `os`, `arch`, `shell`, `client_version` | For display and diagnostics. `os` and `arch` use Go's `GOOS` and `GOARCH` names. `os` MUST NOT be `windows`: the plugin closes with `4427` |

`welcome` (plugin to device):

```json
{"type": "welcome", "version": 2, "device_id": "d_8f2c", "name": "desktop", "ping_interval": 25,
 "plugin_version": "0.2.0", "hermes_version": "v0.22.7"}
```

`device_id` and `name` come from the assertion.

Dev mode only (section 8.9): with no hub in between, `hello` also carries `"name"` and `"token"`, there is no assertion, and the plugin checks the token against its static device list. `device_id` is then the configured name.

### 4.2 exec (plugin to device)

```json
{"type": "exec", "id": "4b1e...", "cmd": "source /home/alice/.cache/hermes-device/tmp/hermes-snap-...; ls",
 "cwd": "/home/alice/src/app", "login": false, "timeout": 180, "stdin": "optional"}
```

| Field | Meaning |
|---|---|
| `id` | Request id, unique per connection (32 hex characters) |
| `cmd` | Command text, run as `<shell> -c <cmd>` (`<shell> -l -c <cmd>` when `login` is true), in its own process group |
| `cwd` | Absolute directory on the device. Resolved through symlinks and checked against `roots` before anything runs |
| `timeout` | Seconds. The plugin enforces it and sends `kill`. The device also kills the command at `timeout + 10s` in case the plugin went away |
| `stdin` | Optional text written to the command's stdin, then closed. Without it stdin is empty |

The device answers each `exec` with zero or more `output` and exactly one `exit`.

### 4.3 output (device to plugin)

```json
{"type": "output", "id": "4b1e...", "data": "README.md\nmain.go\n"}
```

Merged stdout and stderr, in order. Each `data` is valid UTF-8: the device never splits a character across messages, and replaces invalid bytes.

### 4.4 exit (device to plugin)

```json
{"type": "exit", "id": "4b1e...", "code": 0}
{"type": "exit", "id": "4b1e...", "code": 130, "killed": true}
{"type": "exit", "id": "4b1e...", "code": 126, "error": "device desktop refused: working directory \"/etc\": outside the allowed roots"}
```

| Code | Meaning |
|---|---|
| command's own | Normal exit. A command ended by a signal reports `128 + signal` |
| <!-- docgen:const pkg=protocol name=ExitRefused code=false -->126<!-- /docgen:const --> with `error` | The device refused (cwd outside the roots, missing cwd) or could not start the command. No process ran |
| <!-- docgen:const pkg=protocol name=ExitKilled code=false -->130<!-- /docgen:const --> with `killed` | Ended by `kill` or the device's own timeout |
| <!-- docgen:const pkg=protocol name=ExitLost code=false -->255<!-- /docgen:const --> | Used by the plugin only, never sent: the device disconnected or was replaced while the command ran |

### 4.5 kill (plugin to device)

```json
{"type": "kill", "id": "4b1e..."}
```

The device sends SIGTERM to the command's process group, then SIGKILL 2 seconds later, and reports `exit` with `killed: true` (unless the command had already exited, in which case its real code). A `kill` for an unknown id is ignored.

### 4.6 ping and pong (either direction)

```json
{"type": "ping", "seq": 17}
{"type": "pong", "seq": 17}
```

The plugin sends `ping` every `ping_interval` seconds; the device answers `pong` with the same `seq`. The device MAY send `ping` too and the plugin answers. A side that receives no message of any kind for 60 seconds closes with `4408`. These pass through the hub like any other message.

### 4.7 Disconnects and resume

When the exec leg drops, the plugin finishes every pending command of that device with exit 255 and a note, and the device kills every command it still runs. Bindings survive: the next connection of the same `device_id` serves them.

`{"type": "resume"}` is reserved for phase 3 (keeping commands alive across a reconnect, with a grace period). Phase 1 peers MUST NOT send it and MUST ignore it.

## 5. Gateway leg and the chat splice

### 5.1 Legs

- Device to hub: `GET wss://<hub>/api/device/gateway` with the device token.
- Hub to backend: the hub opens the gateway's canonical session socket for the device's user, exactly as for its web UI (`/api/ws` on 9120, subprotocols `hermes-gateway-v1` and `hermes-gateway-ticket.<ticket>`), and copies messages both ways unchanged.
- The hub dials the backend as its web UI does, with an `Origin` header and `?native_dial=browser`, because the gateway refuses a ticket dial from a non-loopback peer without an `Origin` (gateway/run_api.py:428-446).
- The hub sends nothing of its own on the device hop. The first message the device sees is the gateway's `gateway.ready` event, with `session_authority: true`.

### 5.2 Loopback splice

`hermes-device run` listens on `gateway.listen` (default `127.0.0.1:0`, loopback only). `hermes-device chat` asks the daemon for a per-run secret (section 5.5) and starts the local Hermes TUI with:

```text
HERMES_TUI_GATEWAY_URL=ws://127.0.0.1:<port>/api/ws?token=<per-run secret>
```

The Ink TUI dials that URL as given, with no subprotocol and no custom headers (ui-tui/src/gatewayClient.ts), so the secret rides in the query. The splice accepts an upgrade only on path `/api/ws`, from a loopback peer, with no `Origin` header, and with a live per-run secret (constant-time compare). Otherwise it answers HTTP `403`. For each accepted TUI socket the splice opens one gateway leg to the hub and copies JSON-RPC messages, applying the rules below to messages from the TUI and to the matching responses.

JSON-RPC ids may be strings (the Ink TUI sends `"r1"`, `"r2"`, ...) or integers. The splice keeps them as raw JSON. Requests the splice sends itself use string ids starting with `hermes-device-`, and the splice consumes their responses. The splice matches JSON member names exactly (`method`, `params`, `id`), as Hermes does. A notification (no `id`) of a denied or rewritten method is dropped. A rewritten method whose `params` is not an object is answered with `4001` `hermes_device_cwd`.

The splice dials the hub leg before it accepts the TUI's upgrade and answers HTTP `502` when that dial fails. It closes TUI sockets with `1000` when their per-run secret is deleted, `1001` when the daemon stops, `1011` when the hub leg drops, and `4408` on a ping timeout.

`hermes-device chat` runs `<chat.hermes> --tui` (plus any arguments after `--`) in the current directory with the terminal attached. It leaves SIGINT to the TUI (the terminal sends it to the TUI's process group), forwards SIGTERM and SIGHUP, deletes its per-run secret when the TUI exits, and exits with the TUI's code (`128 + signal` when a signal ended it).

### 5.3 Rewritten calls

The device directory of a create is the `cwd` the TUI sends, which is the directory `hermes-device chat` ran in. It is a device path.

**`session.create`**

1. Take `params.cwd` (the device directory). If it is missing or not absolute, answer the TUI with an error (5.6) and send nothing.
2. If `params.safe_mode` or `params.ignore_user_config` is true, answer the TUI with an error (5.6, reason `hermes_device_bypass`) and send nothing. Such a session runs in a managed worker with the code-default terminal, outside the plugin, so its commands would run in the pod (gateway/session_local.py `_bypass_policy`).
3. Set `params.row_cwd` to the device directory and `params.cwd` to the placeholder from `whoami` (section 7.3). Remove `params.editor.mcp_servers` if present. Leave every other field, including `request_id`, as sent. The placeholder is cached and refetched after the gateway refuses a create; if whoami fails, the TUI gets the bind error with the hub's code.
4. Forward. Hold the response.
5. On a result, bind `result.session_id` to the device directory (`PUT /api/device/sessions/{id}/binding`, section 7.4). Only when the bind succeeds, pass the response to the TUI.
6. If the bind fails, send `session.mutate` with `{"session_id": <id>, "request_id": "hermes-device-delete-<id>", "expected_revision": result.revision, "expected_generation": result.execution_generation, "operation": "delete", "payload": {}}`, wait for its answer (failure is logged, not retried), then answer the TUI's request with the bind error (5.6).
7. Retries are idempotent: the TUI resends a timed-out create, or one whose socket dropped, with the same `request_id`; the gateway returns the same session for it, and the bind of the same session to the same directory returns the existing binding. The splice caches successful binds by `request_id` and skips the bind call on a repeat that names the same session and directory. The TUI treats a JSON-RPC error answer as final and mints a new `request_id` (ui-tui/src/gatewayClient.ts `createCanonical`), so when the hub leg drops while a create is held the splice closes the TUI's socket (code `1011`) instead of answering it. The TUI then redials and retries the same `request_id`. Binds already running when the leg drops finish; a create whose bind then fails gets no answer.

`/new` in the TUI is a fresh `session.create` from the client, so it goes through the same steps.

**`session.resume`**

Resuming in `hermes-device chat` means "continue this session here". Before forwarding, bind `params.session_id` to the directory `chat` ran in (from the per-run secret, section 5.5). A session already bound to that directory keeps its binding; one bound to another directory of this device moves to this one. If the bind fails (for example `409` `bound_to_other_device`), answer the TUI with the bind error and forward nothing. The bind is skipped when this chat run (the same per-run secret) already bound the session, so a TUI reconnect does not undo a workspace move. A resume with an empty `session_id` (the latest session) is forwarded first, and the session id in its answer is bound before the answer is passed on. Remove `params.editor.mcp_servers` if present.

**`session.mutate` with `operation` `workspace`** (a workspace move)

1. Bind `params.session_id` to `params.payload.cwd` (a device path). On failure, answer the TUI with the bind error and forward nothing.
2. Set `params.payload.cwd` to the placeholder and forward.
3. If the gateway refuses the move, bind the session back to its previous directory when the daemon knows it.

**`session.mutate` with `operation` `branch`**

Remove `params.payload.mcp_servers` if present, then forward. On a result with `branched_session_id`, bind that id to the parent's binding directory (one this daemon bound; for a parent it never bound, skip the bind and log it) before passing the response on. (The plugin also inherits bindings through lineage, section 8.3; the explicit bind covers a child whose lineage row is not written yet.) If the bind fails, pass the response on anyway and log the error: lineage still fails the child closed.

### 5.4 Denied calls

The splice answers these itself with error code `-32601` and message `"<method> is not available through hermes-device"`, and forwards nothing:

| Method | Why |
|---|---|
| `config.set` with any `key` other than `verbose` | Profile-wide settings of the pod. The TUI sets `verbose` after every create. This also refuses `/reasoning`, `/yolo`, `/fast` and display settings from the TUI in phase 1 |
| `session.mutate` with `operation` `editor_mcp` | Spawns editor MCP programs on the pod |
| `vault.*` | Credential store of the pod |
| `model.save_key`, `profiles.create` | Writes credentials and profiles on the pod |
| `shell.exec`, `cli.exec` | Run on the pod, in the placeholder directory |
| `complete.path` | Lists pod directories. The splice answers `{"items": []}` as a result instead of an error |
| `image.attach`, `pdf.attach` with a non-null `path`, `file.attach` with a non-null `path`, `input.detect_drop`, `clipboard.paste` | Read pod paths or the pod clipboard, not the device's |
| `mcp.servers.add`, `mcp.servers.test`, `mcp.servers.remove`, `plugins.manage`, `skills.manage`, `cron.manage`, `cron.submit`, `kanban.run`, `browser.manage`, `display.install`, `display.start`, `a2a.forward`, `worker.*`, `process.kill`, `process.list` | Change, spawn or stop programs on the pod |
| `rollback.restore` | Restores pod checkpoints |
| `connectors.*`, `set_api_key`, `oauth.*`, `diagnostics.share_nous`, `free_tier.provision`, `session.foreign.import` | Change pod credentials or import pod files |
| `prompt.background` | A background agent has no lineage to its parent session (gateway/session_tui_side.py), so it would run on the pod. The plugin also refuses it for bound sessions (section 8.3) |

Every other call passes unchanged, including `slash.exec` and `command.dispatch`, which the TUI needs for its slash commands; the plugin guards what those reach (section 8.3). Phase 3 replaces this list with an allowlist.

This list keeps a well-behaved TUI from acting on the pod by mistake. It is not a security boundary: the splice runs on the device, and anyone holding the device token can open the gateway leg without it (section 10).

### 5.5 Daemon control

`hermes-device run` writes `<state dir>/daemon.json` (mode `0600`, directory `0700`):

```json
{"pid": 4242, "splice": "127.0.0.1:41733", "control_secret": "<43 base64url chars>", "device_id": "d_8f2c", "hub": "https://hub.example.com"}
```

`hermes-device chat` reads it and calls, on the splice address:

- `POST /control/tokens` with `Authorization: Bearer <control_secret>` and `{"cwd": "<absolute device dir chat runs in>"}`: answers `201` with `{"token": "<43 base64url chars>"}`, a new per-run secret tied to that directory (used for `session.resume`, 5.3). A missing or relative `cwd` gets `400`; a daemon that is stopping answers `503`.
- `DELETE /control/tokens/<token>` with the same header when the TUI exits: `204`. Open TUI sockets that used it are closed.

The daemon refuses the control endpoints from non-loopback peers and with a wrong secret (`403`). It removes `daemon.json` on a clean exit.

### 5.6 Errors to the TUI

The splice answers a request it refuses or fails with `{"jsonrpc": "2.0", "id": <same id>, "error": {"code": <code>, "message": <text>, "data": {"reason": <reason>}}}`:

| Case | `code` | `data.reason` |
|---|---|---|
| Denied method | `-32601` | `hermes_device_denied` |
| Missing or relative cwd | `4001` | `hermes_device_cwd` |
| `safe_mode` or `ignore_user_config` create | `4001` | `hermes_device_bypass` |
| Bind failed | `4001` | `hermes_device_bind`, plus `data.hub_error` with the hub's error code (`unauthorized`, or `hub_unreachable` when the hub gave none) |

When the hub leg drops, the splice closes the TUI's socket with code `1011`; it does not answer held requests (5.3 step 7).

## 6. ACP leg

### 6.1 Legs

- Editor to `hermes-device acp`: ACP over stdio, newline-delimited JSON-RPC 2.0.
- Device to hub: `GET wss://<hub>/api/device/acp` with the device token. One WebSocket message per JSON-RPC message.
- Hub to acp-host: `GET ws://<pod>:9130/acp` (<!-- docgen:const pkg=protocol name=ACPHostPath -->`/acp`<!-- /docgen:const -->) with the assertion (`leg` `acp`). Only the hub can reach this port.
- acp-host verifies the assertion, starts one `hermes acp` process for the connection (in the pod, in its own process group, with acp-host's environment and so the pod's `HERMES_HOME`), writes each message to its stdin as one line (re-encoded, so a message never holds a raw newline), and sends each stdout line that is JSON as one message. Other stdout lines are dropped and logged. Its stderr goes to acp-host's log, one line per entry, tagged with the connection. When the WebSocket closes, acp-host closes the child's stdin, sends SIGTERM to the process group after 5 seconds and SIGKILL after 10. When the child exits on its own, acp-host closes the WebSocket with `1011`. At `acp-host.max-processes` children, new connections are closed with `1013`. WebSocket messages over 48 MiB close the connection (`1009`), because `hermes acp` reads stdin with a 50 MiB line limit and stops reading on a longer line.
- A refused assertion is answered by accepting the upgrade and closing at once with `4403`, so the hub can relay the code. `GET /healthz` answers `200`.

`hermes acp` attaches to the running gateway in the same pod (acp_adapter/gateway_server.py), so the sessions live in the gateway, and a restarted gateway is reattached by the same `hermes acp` process. acp-host runs in its own container, so its children outlive gateway restarts.

When no gateway answers, `hermes acp` starts one itself (hermes_cli/gateway_client.py `connect_gateway` calls `ensure_gateway_runtime`, which calls `spawn_unmanaged_gateway` with `start_new_session=True`). Hermes has no setting that turns this off. In the acp-host container that would be a second gateway with no device connections. So acp-host:

- starts a child only while the gateway answers `2xx` on `acp-host.gateway-health-url` (default `http://127.0.0.1:9120/api/health`, a public route), and otherwise closes the connection with `1013`;
- makes itself a child subreaper, and every 2 seconds stops (SIGTERM, then SIGKILL 5 seconds later) every process that left a child's process group, and every orphan reparented to it, then reaps it. A gateway that a child starts during a gateway restart is stopped this way.

acp-host also refuses, with JSON-RPC error `-32602` and message `session is bound to another device`, any `session/load`, `session/resume`, `session/fork` or `session/prompt` whose `sessionId` is bound in `bindings.json` to a device other than the assertion's `device_id`. It folds `params.session_id` (the field name the ACP SDK also accepts) into `params.sessionId` first. When `bindings.json` cannot be read or parsed, these calls get `-32603`.

### 6.2 acp-host rewrites (editor to agent)

| Method | Rewrite |
|---|---|
| `session/new`, `session/load`, `session/resume`, `session/fork` | `params.cwd` becomes the placeholder (`acp-host.placeholder`). `params.mcpServers` becomes `[]` and `params.mcp_servers` is removed (the ACP SDK accepts field names as well as aliases): editor MCP servers would run as programs in the pod. The same values on every call keep `session/load` from triggering a workspace move or an MCP rebind |
| `session/list` | `params.cwd`, when present, must be absolute and is removed: TUI sessions keep their device dir as row cwd, so a placeholder filter would hide them (acp_adapter/catalog.py). The request gets an id of acp-host's own, `hermes-device-list-<n>`, and the response gets the editor's id back, so the response is matched whatever id form the editor used (Python re-encodes ids, and treats `"id": null` as a request). The result is filtered (below) |
| `session/prompt` | Every content block of type `resource_link` is replaced by `{"type": "text", "text": "[hermes-device: link <uri> removed; the bridge inlines device files]"}`. `hermes acp` reads a link as a pod path whatever its form: `file://` in any case, a bare path, or a drive path (acp_adapter/content.py `_path_from_file_uri`) |

Rewritten messages are decoded and encoded again, so a duplicate key keeps its last value, as in Python. acp-host answers these itself and forwards nothing: a message that is not JSON (`-32700`), JSON that is not one object, such as a batch (`-32600`), and `params` of a rewritten method that are not an object, a `prompt` that is not an array, or a relative list `cwd` (`-32602`). A refused notification (no `id` member) gets no answer.

`session/list` results: acp-host reads `$HERMES_HOME/device/bindings.json` (section 8.2; setting `acp-host.bindings-file` overrides the path) read-only, on every list, keeps only rows whose session is bound to the assertion's `device_id` (and, when the request named a cwd, whose binding dir is that cwd or below it), and sets each kept row's `cwd` to its binding dir. A missing file keeps no rows; so does a file that cannot be read or parsed (logged). `nextCursor` passes unchanged, so a filtered page can be empty while `nextCursor` is still set.

Everything else passes unchanged in both directions, including the agent's requests to the editor (`session/request_permission`, `elicitation/create`).

### 6.3 Bridge behavior (`hermes-device acp`)

The bridge runs on the device, so it can read the editor's files and it knows the editor's real cwd.

1. `session/prompt`: each `resource_link` block with a `file://` URI (host empty or `localhost`) of a regular file up to 1 MiB of valid UTF-8 is replaced by a `resource` block with `{"uri": <uri>, "mimeType": "text/plain", "text": <contents>}`. Other links pass, and acp-host replaces them.
2. `session/new`, `session/fork`, `session/load` and `session/resume` need an absolute `params.cwd`; otherwise the bridge answers `-32602` and forwards nothing. `session/new` and `session/fork`: on a result, bind `result.sessionId` to the request's `cwd` (section 7.4) before passing the result to the editor. If the bind fails, answer the editor's request with error `-32603`, message `hermes-device: could not bind the session to <cwd>: <hub error>`. (ACP has no delete; the unbound empty session is not listed, because Hermes lists only sessions with messages.)
3. `session/load` and `session/resume`: bind `params.sessionId` to `params.cwd` before forwarding. A session bound to the same directory keeps its binding. On failure, answer with the error above and forward nothing.
4. Reconnect: when the ACP leg drops, the bridge answers every editor request still waiting with error `-32603`, message `hermes-device: connection to Hermes lost; retry`, and reconnects with backoff. On the new connection it sends the params of the editor's last successful `initialize` and consumes the response, then, for each session the editor opened (a successful new, fork, load or resume), sends `session/load` with `{"sessionId", "cwd", "mcpServers": []}` and drops every `session/update` for that session until the load's response, which it also consumes. Those updates are the replayed history the editor already has. A load that fails is logged and the session is forgotten, and so is a session the editor closed with a successful `session/close`. Editor messages wait until the replay is done, at most 30 seconds; a request still waiting then is answered with the error above.
5. Ids: the bridge sends every editor request to the agent under a string id of its own, `hermes-device-<n>`, and gives the response the editor's id back. Python re-encodes ids (`1e0` comes back as `1.0`, `"\u00e9"` as `"é"`), so this is the only way to match every response. The bridge's own requests use the same form. An editor message over 48 MiB is answered with `-32600`, because acp-host would close the leg.
6. Agent requests (`session/request_permission`, `elicitation/create`) reach the editor with a bridge id `hermes-device-<n>`, and the editor's answer goes back under the agent's id. A new `hermes acp` process numbers its requests from the start again, so this keeps an answer meant for a lost connection from reaching a new request with the same id. Answers to requests of a lost connection are dropped.
7. The bridge stops with a non-zero exit when the hub refuses the token (HTTP `401` on the upgrade or on a bind, or close `4401`) or closes with `4426`. Every other close or dial failure is retried. Stdout carries only JSON-RPC lines; logs go to stderr.

## 7. Hub HTTP API (device side)

Base: the hub URL (`hub.url`, for example `https://hub.example.com`). Bodies are JSON. Errors are `{"error": "<code>", "message": "<text>"}` with an HTTP status.

### 7.1 Pairing

The flow follows the OAuth 2.0 device authorization grant (RFC 8628) in shape. The hub pins the device to one backend at pairing.

**Start**: `POST /api/device/pair`, no auth.

```json
{"name": "desktop", "hostname": "desktop", "os": "linux", "arch": "amd64", "client_version": "v0.2.0"}
```

`name` is a suggestion the user may change on the hub page. Answer `200`:

```json
{"device_code": "<secret, at least 32 bytes base64url>", "user_code": "WDJB-MJHT",
 "verification_uri": "https://hub.example.com/device/pair",
 "expires_in": 600, "interval": 5}
```

The client prints `verification_uri` and `user_code`. On the hub page the user signs in and types the code; the hub never takes the code from the link, so a link someone else sends cannot pair their device to the user's backend. The confirm page shows the requesting device's address, `hostname`, `os` and the request time. The user names the device and confirms. The hub rate-limits both unauthenticated pairing endpoints per address and limits wrong code entries per user.

**Poll**: `POST /api/device/pair/token`, no auth, `{"device_code": "..."}`, no more often than every `interval` seconds.

| Answer | Meaning | Client action |
|---|---|---|
| `200` `{"token", "device_id", "device_name", "backend"}` | Paired | store the token, done |
| `400` `{"error": "authorization_pending"}` | Not confirmed yet | keep polling |
| `400` `{"error": "slow_down"}` | Polling too fast | add 5 seconds to the interval |
| `400` `{"error": "expired_token"}` | Code expired | stop, tell the user to run login again |
| `400` `{"error": "access_denied"}` | User declined | stop |

The token is returned once. The hub forgets the device code after that answer.

### 7.2 Revoke

The user revokes a device on the hub page. The hub then refuses the token (section 2.1) and closes the device's open legs with `4401`.

### 7.3 Whoami

`GET /api/device/whoami` with the device token. Answer `200`:

```json
{"device_id": "d_8f2c", "device_name": "desktop", "user": "alice", "backend": "alice",
 "hermes_version": "v0.22.7", "plugin_version": "0.2.0", "placeholder": "/opt/data/hermes/device-placeholder"}
```

`hermes_version`, `plugin_version` and `placeholder` come from the plugin's `GET /info` (section 9); if the backend is unreachable the hub answers `502` `{"error": "backend_unavailable"}`. `chat` compares `hermes_version` with the first line of the local `hermes --version` (`Hermes Agent v<version> (<date>)`), exactly after removing a leading `v`, and refuses to start on a mismatch (the local TUI and the server must be the same release).

### 7.4 Session binding

`PUT /api/device/sessions/{session_id}/binding` with the device token and `{"dir": "/home/alice/src/app"}`.

The hub forwards to the plugin as `PUT /api/plugins/device/sessions/{session_id}/binding` with `{"device_id": <the token's device id>, "dir": <dir>}`. A device can only bind sessions to itself. The hub passes the plugin's status and body through.

A session whose frozen terminal backend is not `device`, or that runs in a bypass (safe mode) worker, cannot be bound: the plugin answers `409` `not_device_session`.

`DELETE /api/device/sessions/{session_id}/binding` with the device token: the hub forwards it only when the session is bound to the token's device, else `403` `{"error": "forbidden"}`.

Binding object and errors: section 9.

## 8. Plugin (Hermes side)

The plugin is a Hermes user plugin named `device`, installed into `$HERMES_HOME/plugins/device` and enabled with `hermes plugins enable device`.

### 8.1 Settings

`plugins.entries.device.settings` in the profile's `config.yaml`:

| Key | Default | Meaning |
|---|---|---|
| `backend_id` | none (required) | This backend's id, the assertion `aud` |
| `hub_public_key_env` | `HERMES_DEVICE_HUB_PUBLIC_KEY` | Env var holding the hub public keys (section 3.4) |
| `hub_public_key_file` | none | File holding the hub public keys; wins over the env var |
| `placeholder` | none (required) | Server directory used as the cwd of device sessions. Must exist. Must match acp-host's `acp-host.placeholder` |
| `device_write_roots` | `[]` | Device directories the operator added to `HERMES_WRITE_SAFE_ROOT` so file tools can write to devices (section 8.7) |
| `fallback` | `local` | What an unbound, never-bound session runs on: `local` (the pod shell, as without the plugin) or `refuse` |
| `dev_static_tokens` | `false` | Dev mode (section 8.9) |
| `devices` | `{}` | Dev mode only: `<name>: {token_env: <env var>}` or `{token: <at least 16 chars>}` |

`terminal.backend: device` selects the plugin's backend for every new session of the profile, on every surface. `terminal.cwd` stays the pod workspace, which unbound sessions use.

### 8.2 Bindings file

`$HERMES_HOME/device/bindings.json`, written atomically (temp file and rename) by the plugin, read by acp-host:

```json
{
  "version": 2,
  "bindings": {
    "local-3f2a...": {"device_id": "d_8f2c", "dir": "/home/alice/src/app", "generation": "a1b2c3d4e5f6", "bound_at": 1794300000.5}
  },
  "ever_bound": ["local-3f2a...", "local-77c1..."]
}
```

`ever_bound` lists every session id that ever had a binding, including unbound ones. It only grows.

### 8.3 Binding resolution

For each command and file operation, the plugin resolves the session id of the running turn through its lineage. It reads the id only from the `HERMES_SESSION_ID` context variable of `gateway.session_context`, never from the process environment (`get_session_env` falls back to `os.environ`, which agent construction overwrites, tools/delegate_tool_dispatch.py:75-79). When the context variable is unset, the command fails closed.

1. Walk from the session to its root through `parent_session_id` (compression children, delegate subagents, branches and forks all record their parent in the sessions table; agent/compression_facade.py rotates `HERMES_SESSION_ID` mid-session, tools/delegate_tool_dispatch.py gives subagents their own id).
2. The nearest session on that path with a binding gives the binding. Its device and directory serve the command.
3. If no session on the path has a binding but any of them is in `ever_bound`, the command fails closed: it does not run, and the tool gets an error that names the session and says to rebind with `/device`.
4. If the lineage cannot be read completely (no row yet, a store error, a cycle, more than 100 hops), the command fails closed the same way.
5. A session whose cwd (the session cwd context variable that Hermes sets from the frozen policy) is the placeholder, and that has no binding, fails closed. This covers sessions created for a device but not bound yet or no longer bound: an ACP bind failure, a hub-created session between create and bind, a create the splice lost.
6. Side agents with no parent row (`/bg` and other background tasks, gateway/session_tui_side.py and gateway/run_background.py) inherit the binding of the session that started them; the plugin wraps their creation to record that link. If the link cannot be recorded, a background task started from a bound session is refused. Cron jobs and Kanban runs run in the pod only; for a bound session the plugin refuses the tools that create them.
7. Only a session whose whole lineage was read and never had a binding runs on the pod (setting `fallback: local`), or is refused (`fallback: refuse`).
8. A bound session whose device is not connected fails the command with exit 255 and a note; it never falls back.
9. A session that runs in a bypass (safe mode or ignore-user-config) managed worker loads no plugins and gets the code-default terminal, so it can never serve a device. The plugin refuses to bind one (`409` `not_device_session`).

### 8.4 Terminal backend

The backend is a wrapper environment that holds two child environments: a Hermes `LocalEnvironment` for never-bound sessions, created only when first needed, and a device environment that runs commands over the exec leg. Each command goes to the child that section 8.3 picks. `uses_host_filesystem` reports the picked child's value, so the pod's in-process file fast paths are used only for pod-local sessions. The device child starts each binding in the binding's directory, not in `TERMINAL_CWD`, which stays the pod workspace for non-local backends (gateway/session_policy.py:469-471). `is_available()` is always true; it does not depend on any device being configured or connected.

### 8.5 Server-side readers

Some Hermes code reads the server filesystem directly instead of going through the terminal backend. For bound sessions the plugin serves these from the device:

| Reader | Plugin action |
|---|---|
| ACP edit approval, `acp_adapter.edit_approval._read_text_if_exists` | Wrapped: for a bound session it reads the file through the device. If the device cannot be read, it raises, so Hermes denies the edit. Edit approval resolves paths on the pod first (`_required_path` calls `tools.file_tools_paths._resolve_path_for_task`, which expands `~` with the pod home and resolves against the pod filesystem), so the pre_tool_call guard (8.6) first rewrites every file-tool path of a bound session to an absolute device path |
| `@file:`, `@folder:`, `@diff`, `@staged`, `@git:N` context references | Built-in kinds, which plugins cannot register (agent/context_references.py `BUILTIN_PREFIXES`). The plugin wraps the expanders so that for a bound session they read from the device, and also registers `@device:<path>` through `register_context_reference` |
| Checkpoints (`tools.checkpoint_manager.CheckpointManager.ensure_checkpoint`) | Wrapped: skipped for bound sessions, so no checkpoint of the placeholder or of a device path is taken on the pod |

### 8.6 pre_tool_call guard

The guard catches every exception and then blocks the call, because Hermes runs a tool whose pre_tool_call hook raised (model_tools.py:797-799).

For every file tool of a bound session (`read_file`, `write_file`, `patch`, `search_files` and the like), the guard rewrites path arguments to absolute device paths (relative paths against the session's device cwd, `~` against the device `home`) with `{"action": "modify", "args": ...}`. For `write_file` and `patch` (including each file of a V4A patch) it then resolves the target path on the device and blocks the call (`{"action": "block", "message": ...}`) when it hits Hermes's write denylist rebuilt for the device's `home` from `hello` (agent/file_safety.py `build_write_denied_paths`, `build_write_denied_prefixes`, `build_write_approval_paths`), or lies outside the device's roots. The hook also keeps the session's cwd record pointed at the device cwd, so file tools resolve relative paths there, and blocks any tool call of a bound session whose terminal environment is not the device backend (a session created before `terminal.backend: device`).

### 8.7 HERMES_WRITE_SAFE_ROOT

Hermes refuses file writes outside `HERMES_WRITE_SAFE_ROOT` (an `os.pathsep` list), whatever the backend, so device roots that file tools write to must be in it. At load, the plugin removes from `HERMES_WRITE_SAFE_ROOT` (in its own process environment) every `device_write_roots` entry that equals, contains or lies inside a pod-writable path: `/opt/data`, `HERMES_HOME`, the system temp directory, `/tmp`, `/var/tmp`, `/dev/shm`, the placeholder, and `terminal.cwd`. It logs an error for each. Such an entry would let pod-local sessions write there.

Windows devices are not supported: the plugin needs POSIX absolute device paths, and the client refuses to run on Windows.

### 8.8 /device command

`/device` shows the session's binding; `/device <device name or id> <dir>` binds; `/device off` unbinds (the session then fails closed, section 8.3); `/device list` lists connected devices.

### 8.9 Dev mode

With `dev_static_tokens: true` the exec route also accepts version 1 and 2 `hello` messages with `name` and `token` and no assertion, checked against `devices`. It is for local tests without a hub. Never enable it on a pod the hub serves.

## 9. Plugin HTTP routes

Mounted by the dashboard under `/api/plugins/device/` on the gateway listener (9120). They are behind dashboard sign-in, so the hub calls them with its dashboard session for the user. Errors are `{"error": "<code>", "message": "<text>"}`.

| Route | Answer |
|---|---|
| `GET /info` | `200` `{"plugin_version", "hermes_version", "protocol_version": 2, "backend_id", "placeholder"}` |
| `GET /devices` | `200` `{"devices": [Device]}` |
| `GET /devices/{device_id}/dirs?path=<abs>` | `200` `{"path": <resolved>, "entries": [{"name": "src"}]}`: subdirectories only, sorted, at most 500, hidden ones included. `path` defaults to the first root |
| `GET /bindings` | `200` `{"bindings": [Binding]}` |
| `GET /sessions/{session_id}/binding` | `200` Binding plus `"resolved": {"state": "bound" or "never_bound" or "fail_closed", "via": <session id or null>}`; `404` `not_bound` with the same `resolved` |
| `PUT /sessions/{session_id}/binding` | Body `{"device_id", "dir"}`. `200` Binding |
| `DELETE /sessions/{session_id}/binding` | `204`; `404` `not_bound` |
| `GET /ws` (WebSocket) | The exec leg, section 4 |

Device object: `{"device_id", "name", "hostname", "os", "arch", "home", "roots", "client_version", "connected_at"}` (`connected_at` in Unix seconds).

Binding object: `{"session_id", "device_id", "dir", "generation", "bound_at"}`. `dir` is the device's `pwd -P` of the requested directory.

PUT rules:

- The device must be connected (`409` `device_offline`).
- `dir` must be absolute (`400` `bad_dir`), and the device must accept it as a cwd and resolve it with `pwd -P` (`422` `dir_refused`, with the device's message).
- A session bound to a different device is refused (`409` `bound_to_other_device`); unbind first.
- Binding a session to the directory it is already bound to returns the existing binding with its generation unchanged. A different directory on the same device replaces the binding with a new generation.
- `404` `unknown_device` when `device_id` is not connected and was never seen; `400` `bad_request` for a malformed body.

Errors from the dirs route: `409` `device_offline`, `422` `dir_refused`, `400` `bad_dir`.

## 10. Security notes

- The device daemon runs commands only in its own roots, and only on its own user's backend. It can lie about command output.
- A device token is full operator access to its user's own Hermes backend. Through the gateway leg the holder can do what the web UI can, including running commands in the pod (`shell.exec`) and in sessions bound to any of that user's devices; the splice's deny list (5.4) does not stop a holder who dials the gateway leg directly. The same holds for the ACP leg, except that acp-host refuses sessions bound to another device. A token cannot reach another user's backend, mint assertions, or learn the backend's dashboard password. Phase 3 moves the method allowlist and the session.create rewrite into the hub so the hub enforces them.
- A compromised Hermes pod can do anything the user's Hermes can, including running commands on connected devices in their roots. It cannot mint hub assertions, because it holds only public keys.
- The hub's private key is the root of trust for every backend. Keep it in the hub's secret store only.

## 11. Versions

The device client, the plugin and acp-host come from one hermes-device release and speak protocol version 2. The local Hermes that `chat` runs must be the same release as the server's Hermes (`chat` checks `hermes_version`). Upgrade both together.
