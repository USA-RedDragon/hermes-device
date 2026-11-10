# hermes-device protocol

Status: phase 3, exec leg protocol version 3 (version 2 peers still served). Pinned to Hermes Agent v0.22.7.

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
- After an upgrade on the exec and ACP legs the hub copies WebSocket messages between the two hops unchanged, in order, text as text. It does not parse them. On the gateway leg the hub parses every JSON-RPC message and enforces the rules of section 5. It closes the other hop when one hop closes, and relays the close code (section 2.3).

## 2. Common rules

### 2.1 Device token

- Format: `hdt_` followed by 43 characters of base64url without padding (32 random bytes). The hub generates it at pairing and stores only its SHA-256.
- The device sends it as `Authorization: Bearer <token>` on every hub HTTP request and WebSocket upgrade.
- The hub answers `401` with `{"error":"unauthorized"}` (HTTP), or refuses the upgrade with HTTP `401`, when the token is unknown or revoked, when the device's user may no longer use the pinned backend, or when the backend no longer exists or lost `devices` in the hub's configuration. The client then stops and tells the user to run `hermes-device login`. When the hub cannot check the token or the access (a store or lookup error), it answers `503` with `{"error":"internal_error"}` instead, and the client reconnects with backoff.
- The client keeps the token in the OS keyring (service `hermes-device`, account = the hub URL's host) when one is available, else in a file with mode `0600` (default `<user config dir>/hermes-device/token`).
- The hub records the device's last-seen time and remote address on every authenticated request and upgrade, and shows "reconnected from a new address" when the address changes.

### 2.2 Pings

Every WebSocket hop on every leg (device to hub, hub to backend) sends WebSocket ping control frames from both ends:

| Setting | Value |
|---|---|
| Ping interval | <!-- docgen:const pkg=protocol name=PingInterval print=config -->`25s`<!-- /docgen:const --> |
| Pong deadline | <!-- docgen:const pkg=protocol name=PingTimeout print=config -->`15s`<!-- /docgen:const --> |

A side that gets no pong within the deadline closes the hop with code `4408` (or drops the TCP connection if the close cannot be sent). The exec leg also has an end-to-end application ping (section 4.6), because a healthy device-to-hub hop says nothing about the plugin.

Any HTTP proxy in front of the hub or the backend (for example a Gateway API HTTPRoute) MUST NOT time out these upgrades: give `/api/device/*` its own route rule with request and idle timeouts off. The client reconnects on any drop. On a version 3 exec leg, commands in flight survive a drop for a grace period (section 4.7).

### 2.3 Close codes

The hub relays the backend's close code and reason to the device unchanged. Codes the hub itself uses are marked.

| Code | Name | Sent by | Client action |
|---|---|---|---|
| `1000`, `1001` | normal, going away | any | reconnect with backoff |
| <!-- docgen:const pkg=protocol name=CloseUnauthorized code=false -->4401<!-- /docgen:const --> | unauthorized | hub (token), plugin (dev static token) | stop, ask for login |
| <!-- docgen:const pkg=protocol name=CloseAssertionRefused code=false -->4403<!-- /docgen:const --> | assertion refused | plugin, acp-host | reconnect with backoff (a hub or pod misconfiguration) |
| <!-- docgen:const pkg=protocol name=ClosePingTimeout code=false -->4408<!-- /docgen:const --> | ping timeout | any | reconnect |
| <!-- docgen:const pkg=protocol name=CloseReplaced code=false -->4409<!-- /docgen:const --> | replaced | plugin | `run` stops: another daemon with the same token took over |
| <!-- docgen:const pkg=protocol name=CloseUnsupportedVersion code=false -->4426<!-- /docgen:const --> | unsupported version | plugin, device | stop, show the close reason (it names both sides' versions) and tell the user to upgrade |
| <!-- docgen:const pkg=protocol name=CloseUnsupportedPlatform code=false -->4427<!-- /docgen:const --> | unsupported platform | plugin | stop: the device's OS (Windows) is not supported |
| <!-- docgen:const pkg=protocol name=CloseBackendUnavailable code=false -->4502<!-- /docgen:const --> | backend unavailable | hub (also when the backend hop drops without a close frame) | reconnect with backoff |
| `1008` | policy | Hermes plugin gate | reconnect with backoff (plugin disabled) |
| `1011` | `hermes acp` exited | acp-host | reconnect with backoff |
| `1013` | try again later | acp-host (process limit, or no gateway) | reconnect with backoff |
| `1002` | protocol error | plugin (no hello in time), device (no resume in time) | reconnect with backoff |
| <!-- docgen:const pkg=protocol name=CloseServiceRestart code=false -->1012<!-- /docgen:const --> | service restart | plugin (uvicorn, when the gateway stops) | reconnect with backoff; on the exec leg the device kills its commands at once (section 4.7) |

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

Protocol version: <!-- docgen:const pkg=protocol name=Version code=false -->3<!-- /docgen:const -->. The oldest version this release still speaks: <!-- docgen:const pkg=protocol name=MinVersion code=false -->2<!-- /docgen:const -->. Version 3 adds resume (section 4.7) and the output `offset`; everything else is as in version 2. Capabilities (section 4.1) add optional features without a version bump: browser control (section 4.8).

### 4.1 Handshake

1. The plugin verifies the assertion before it reads any message. On failure it closes with `4403`.
2. The device sends `hello` within <!-- docgen:const pkg=protocol name=HelloTimeout print=config -->`10s`<!-- /docgen:const --> of the upgrade. Otherwise the plugin closes with `1002`.
3. Version negotiation. `hello.version` is the oldest version the device speaks and `hello.max_version` the newest (absent means equal to `version`). The plugin picks the newest version that both sides speak. A version 2 plugin reads only `version`, so a device that speaks 2 to 3 sends `"version": 2, "max_version": 3` and a version 2 plugin serves it as version 2. When no version is common, the plugin closes with `4426` and a reason that names both ranges and both releases, for example `hermes-device v0.4.0 speaks protocol 4 to 5; plugin 0.3.0 speaks 2 to 3; upgrade them together`. The hub relays the code and the reason (it cuts reasons to 120 bytes; the plugin keeps them shorter), and the client stops and shows the reason. A device that gets a `welcome.version` outside its own range closes with `4426` and a reason of the same form, and stops.
4. The plugin answers `welcome` with the chosen version. On version 3 it sends `resume` (section 4.7) right after `welcome`, before any other message. From then on either side may send.
5. Capabilities. `hello.capabilities` lists the optional features the device serves; today only `browser` (<!-- docgen:const pkg=protocol name=CapabilityBrowser code=false -->browser<!-- /docgen:const -->, section 4.8). The plugin puts the ones it accepts in `welcome.capabilities` and leaves the field out when it accepts none. Each side uses a feature only when `welcome.capabilities` names it, and ignores unknown names. A plugin that does not know capabilities ignores the field and sends none back, so the device does not use them.
6. If a connection for the same `device_id` exists, the plugin closes it with `4409`. Newest wins. On version 3 its pending commands move to the new connection and are listed in the `resume`; on version 2 they fail with exit 255.

`hello` (device to plugin):

```json
{
  "type": "hello",
  "version": 2,
  "max_version": 3,
  "roots": ["/home/alice/src"],
  "tmp_dir": "/home/alice/.cache/hermes-device/tmp",
  "home": "/home/alice",
  "hostname": "desktop",
  "os": "linux",
  "arch": "amd64",
  "shell": "bash",
  "client_version": "v0.3.0"
}
```

| Field | Meaning |
|---|---|
| `version`, `max_version` | The oldest and the newest protocol version the device speaks (step 3) |
| `roots` | Absolute directories, resolved through symlinks, that commands may run in. The device refuses any `exec` whose `cwd` is outside them |
| `tmp_dir` | Absolute directory Hermes uses on the device for its shell snapshot files |
| `home` | The device user's `$HOME`. The plugin checks writes against the credential denylist relative to it (section 8.6) |
| `capabilities` | Optional features the device serves (step 5). Absent means none |
| `hostname`, `os`, `arch`, `shell`, `client_version` | For display and diagnostics. `os` and `arch` use Go's `GOOS` and `GOARCH` names. `os` MUST NOT be `windows`: the plugin closes with `4427` |

`welcome` (plugin to device):

```json
{"type": "welcome", "version": 3, "device_id": "d_8f2c", "name": "desktop", "ping_interval": 25,
 "plugin_version": "0.3.0", "hermes_version": "v0.22.7", "grace_seconds": 120}
```

`device_id` and `name` come from the assertion. `version` is the chosen version. `grace_seconds` (version 3 only) is the plugin's resume grace (section 4.7). `capabilities`, when present, lists the device's capabilities the plugin accepts (step 5), for example `["browser"]`. `client_version` in `hello` and `plugin_version` and `hermes_version` in `welcome` carry the release versions both ways, for display and for the `4426` reasons.

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
{"type": "output", "id": "4b1e...", "data": "README.md\nmain.go\n", "offset": 0}
```

Merged stdout and stderr, in order. Each `data` is valid UTF-8: the device never splits a character across messages, and replaces each invalid byte with U+FFFD. `offset` is the byte offset of the first byte of `data` (UTF-8, after the replacement) in the command's whole output. A version 2 plugin ignores it.

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
| <!-- docgen:const pkg=protocol name=ExitLost code=false -->255<!-- /docgen:const --> | Set by the plugin when the device disconnected or was replaced while the command ran and no resume brought the command back (section 4.7). The device sends it, with `error`, only in answer to a `resume` that names a command it has no record of |

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

The plugin sends `ping` every `ping_interval` seconds; the device answers `pong` with the same `seq`. The device sends `ping` too and the plugin answers. A side that receives no message of any kind for 60 seconds closes with `4408`. These pass through the hub like any other message.

The plugin MUST handle messages in the order they arrive and answer a `ping` only after it handled every message before it. On version 3 the device uses this as an acknowledgement: the hub relays in order, so a `pong` tells the device that the plugin has every `output` and `exit` the device wrote before that `ping`.

### 4.7 Disconnects and resume

Bindings always survive a drop: the next connection of the same `device_id` serves them. What happens to commands in flight depends on the version.

**Version 2.** When the exec leg drops, the plugin finishes every pending command of that device with exit 255 and a note, and the device kills every command it still runs.

**Version 3.** Both sides keep commands for a grace period, so that a hub restart or a network drop does not fail them.

The plugin side:

1. When a version 3 leg drops, the plugin keeps the device's pending commands for `grace_seconds` (plugin setting, default <!-- docgen:const pkg=protocol name=DefaultGrace print=config -->`2m0s`<!-- /docgen:const -->, at most 3600; 0 turns resume off). The tool calls that wait on them keep waiting. New commands for the device fail at once, as for any device that is not connected.
2. When the device connects again within the grace with version 3, the plugin moves those commands to the new connection and sends, right after `welcome`:

   ```json
   {"type": "resume", "commands": [{"id": "4b1e...", "offset": 1834}]}
   ```

   `commands` lists every command the plugin still waits for on this device (it may be empty), each with the number of output bytes it already has. A version 3 plugin always sends `resume`, also when the list is empty.
3. After the grace, the commands finish with exit 255 and a note. A reconnect with version 2 also fails them.
4. A killed command is never listed in a `resume`, so the device kills it on the resume. This covers a `kill` sent into a connection that was already dead and a `kill` while the command waits for a resume; in the second case the plugin finishes it at once with exit 130.
5. When an `output` arrives with an `offset` below the bytes the plugin has, it drops the bytes it already has. When it arrives above, the plugin adds a note to the output that says how many bytes were lost.
6. A leg that ends with `1012`, or whose handler the server cancels, is not parked: its pending commands fail at once with exit 255. uvicorn ends every socket with `1012` when the gateway stops, and the device kills its commands on `1012`, so nothing would resume them. Commands already parked for other devices keep their grace; in a stopping gateway they end with the process.

The device side:

1. The device keeps each command's output that the plugin has not acknowledged (section 4.6), up to `device.resume-buffer` bytes per command (default 1 MiB). When a command writes more before an acknowledgement, its oldest output goes first. It keeps a finished command's `exit` until it is acknowledged.
2. When a version 3 leg drops for any reason other than a stop code of section 2.3 or `1012`, the device keeps its commands running for the welcome's `grace_seconds`, capped by `device.max-grace` (default 10 minutes). After that it kills them and forgets them. On `1012` it kills them at once.
3. A version 3 device waits up to <!-- docgen:const pkg=protocol name=HelloTimeout print=config -->`10s`<!-- /docgen:const --> for the `resume` after `welcome`, answering pings meanwhile. Any other message, or no `resume` in time, closes the leg with `1002`.
4. On `resume` the device kills and forgets every command it holds that the list does not name. For each named command it knows, it sends the buffered `output` messages that end after the given `offset` (with their original offsets) and then the `exit` if the command ended. For each named command it does not know, it sends `exit` with code 255 and an `error` that says the device has no record of it.
5. A device that connects with version 2 to a plugin kills every command it still holds from an earlier version 3 leg.

What a restart does:

| Restart | Result |
|---|---|
| hub (close `1001`, then upgrades refused with `502` until it is back) | Commands keep running and resume when the device reconnects within the grace |
| network drop, ping timeout (`4408`) | Same |
| Hermes gateway, also a rollout to a new pod | The plugin's pending commands live in the gateway process's memory, so they cannot survive it. Hermes stops the running turns before it stops the listener (gateway/run_runtime.py), the plugin fails every command still pending on a connected device at once (point 6 above), and the device, which sees `1012`, kills its commands at once. If the device misses the `1012` (for example a pod that was killed), its next connection gets an empty `resume` from the new plugin process and it kills them then; if it cannot connect at all, it kills them after the grace. Nothing waits for the grace in either case |
| `hermes-device run` | The daemon kills its commands when it stops. The plugin's `resume` to the new daemon names them, the daemon answers exit 255 for each, and the tool calls end at once |

### 4.8 Browser control (capability `browser`)

On a leg where `welcome.capabilities` names `browser`, three more message types run on the exec leg. docs/browser.md section 4 defines their fields, ops and codes.

| Type | Direction | Meaning |
|---|---|---|
| `browser` | plugin to device | Run one browser operation: `{"type", "id", "op", "args", "timeout"}`. `id` is unique per connection; `kill` with the same `id` cancels it |
| `browser_result` | device to plugin | Exactly one answer per `browser`: `{"type", "id", "ok", "result"}` or `{"type", "id", "ok": false, "code", "error"}`, `code` one of `not_connected`, `refused`, `timeout`, `cancelled`, `failed` |
| `browser_status` | device to plugin | Whether the browser extension is connected: `{"type", "connected", "browser", "extension_version"}`. Sent after the `welcome` (and the `resume`) and on every change |

Browser requests are never resumed: a drop cancels them on the device, and the plugin fails them with `not_connected` at once. A device without the capability never gets `browser`; a plugin that did not accept it never gets `browser_status`.

## 5. Gateway leg and the chat splice

### 5.1 Legs

- Device to hub: `GET wss://<hub>/api/device/gateway` with the device token.
- Hub to backend: the hub opens the gateway's canonical session socket for the device's user, exactly as for its web UI (`/api/ws` on 9120, subprotocols `hermes-gateway-v1` and `hermes-gateway-ticket.<ticket>`).
- The hub parses every message of the device and applies the rules of 5.3 to 5.6, whether or not the device runs the splice. It answers a refused request itself and forwards nothing. It forwards every other device request under its own id `hermes-hub-d<n>` and gives the device's id back on the answer, so the answer matches whatever form the gateway gives the id in. Notifications that pass the rules are forwarded with the same param rewrites as requests (5.4), under no id. The hub also sends its own requests, with ids `hermes-hub-<n>` (5.3), and consumes their answers. It refuses a device request whose id starts with `hermes-hub-` (5.6) and drops any other device message with such an id. It acts on the held answers of 5.3 before the device sees them. Binary messages are dropped.
- A device message without a `method` member passes only as an answer to one of the gateway's requests: its `id` must be a string that starts with `srq-` (gateway/session_server_requests.py `is_response_frame`), and it must have exactly one of `result` and `error`. The hub rebuilds it from `jsonrpc`, `id` and that one member, so the gateway reads no other member, and drops every other method-less message.
- The hub dials the backend as its web UI does, with an `Origin` header and `?native_dial=browser`, because the gateway refuses a ticket dial from a non-loopback peer without an `Origin` (gateway/run_api.py:428-446).
- The hub sends no message of its own to the device except answers to the device's own requests. The first message the device sees is the gateway's `gateway.ready` event, with `session_authority: true`.

### 5.2 Loopback splice

`hermes-device run` listens on `gateway.listen` (default `127.0.0.1:0`, loopback only). `hermes-device chat` asks the daemon for a per-run secret (section 5.5) and starts the local Hermes TUI with:

```text
HERMES_TUI_GATEWAY_URL=ws://127.0.0.1:<port>/api/ws?token=<per-run secret>
```

The Ink TUI dials that URL as given, with no subprotocol and no custom headers (ui-tui/src/gatewayClient.ts), so the secret rides in the query. The splice accepts an upgrade only on path `/api/ws`, from a loopback peer, with no `Origin` header, and with a live per-run secret (constant-time compare). Otherwise it answers HTTP `403`. For each accepted TUI socket the splice opens one gateway leg to the hub and copies JSON-RPC messages, applying the rules below to messages from the TUI and to the matching responses.

JSON-RPC ids may be strings (the Ink TUI sends `"r1"`, `"r2"`, ...) or integers. The splice keeps them as raw JSON. Requests the splice sends itself use string ids starting with `hermes-device-`, and the splice consumes their responses. The splice matches JSON member names exactly (`method`, `params`, `id`), as Hermes does. A notification (no `id`) of a denied or rewritten method is dropped. A rewritten method whose `params` is not an object is answered with `4001` `hermes_device_cwd`.

The splice dials the hub leg before it accepts the TUI's upgrade and answers HTTP `502` when that dial fails. It closes TUI sockets with `1000` when their per-run secret is deleted, `1001` when the daemon stops, `1011` when the hub leg drops, and `4408` on a ping timeout.

`hermes-device chat` runs `<chat.hermes> --tui` (plus any arguments after `--`) in the current directory with the terminal attached. It leaves SIGINT to the TUI (the terminal sends it to the TUI's process group), forwards SIGTERM and SIGHUP, deletes its per-run secret when the TUI exits, and exits with the TUI's code (`128 + signal` when a signal ended it).

The local Hermes launcher resolves `-c`/`--continue [name]` and `-r`/`--resume <id, title or latest>` in its own local session store, which does not hold the server's sessions, and hands the Ink TUI the result in `HERMES_TUI_RESUME` (hermes_cli/main.py `_resolve_continue_arg`, hermes_cli/main_tui_launch.py); with no local match, bare `-c` exits before the TUI starts. So `chat` takes these flags out of the arguments, up to a `--`, in the forms argparse reads: `-c [NAME]`, `-cNAME`, `-c=NAME`, `--continue[=NAME]` and its prefixes down to `--co`, `-r ID`, `-rID`, `--resume[=]ID` and its prefixes down to `--res`. As in argparse, `-c` takes the next argument as its name unless it starts with `-`; a `--resume` with no value, or with one that starts with `-`, stays for the local Hermes to report. `--resume` wins over `-c`, and an empty value resumes nothing, as in Hermes. `chat` then looks the session up with `POST /control/lookup` (5.5: bare `-c` and `--resume latest` are the latest session, any other value is a name) and starts the TUI with `--resume <id>` first. When the lookup finds no session for a name, `chat` passes the name as `--resume <name>`, and the splice resumes it as sent (5.3), so a session that the lookup cannot find by id (such as a messaging session moved to this device) still resumes. When it finds no latest session, `chat` fails before it starts the TUI. `--create-if-missing` is not supported: it has no effect. Each lookup opens a gateway leg, so it adds a round trip to the start.

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

The hub repeats the create rewrite, the hold, the bind and the delete for every `session.create` it receives, whether the device sent it raw or the splice already rewrote it:

1. The hub refuses a create with a `title`, `base_url`, `api_key` or `accept_hooks` member (5.4) and removes `cwd_explicit`. The session checks below run next.
2. A create with `safe_mode` or `ignore_user_config` true is answered with `hermes_device_bypass`.
3. The hub reads the placeholder from the plugin's `GET /info` (cached for 1 minute, dropped when the gateway refuses a create). When `params.cwd` equals the placeholder, the device directory is `params.row_cwd` (a create the splice already rewrote). Otherwise it is `params.cwd`. If that directory is not absolute, or is the placeholder, the hub answers with `hermes_device_cwd`.
4. The hub sets `params.cwd` to the placeholder and `params.row_cwd` to the device directory, removes `params.editor.mcp_servers`, forwards the create and holds the answer.
5. On a result with a `session_id`, the hub first asks the plugin how it resolves the session through its lineage (`GET /sessions/{id}/binding`), and binds only a session that resolves to this device, or to no device with none in its lineage ever bound (the bind API rules of 7.4). Then it binds the session to the directory through the plugin and passes the answer on only when the bind succeeds. It remembers a successful bind for 1 hour under the device id and the `request_id`, and skips the bind on a repeat of the same session and directory. It also remembers for 1 hour that this device created the session (7.4). An error answer, or a result without `session_id`, passes unchanged.
6. If the create answered with a session that resolves to another device, the hub sends `session.detach` with the answer's `session_id` and `subscription_id`, so the leg gets no more of its events, and answers the device's create with `hermes_device_other_device`. It deletes nothing.
7. If the bind fails otherwise, the hub asks the plugin again how the session resolves. Only a session that is `never_bound` (no binding and none ever in its lineage) is deleted: the hub sends `session.mutate` with `{"session_id": <id>, "request_id": "hermes-hub-delete-<id>", "expected_revision": result.revision, "expected_generation": result.execution_generation, "operation": "delete", "payload": {}}`, waits up to 15 seconds for its answer (failure is logged, not retried), then answers the device's create with `hermes_device_bind` (5.6). A session that is or was bound, or whose binding cannot be read, is kept: the hub detaches its subscription as in step 6 and answers with `hermes_device_bind`. If the gateway leg is lost first, the device gets no answer.

The hub also repeats the rest of the rewrites of this section on the gateway leg: it removes `params.editor.mcp_servers` from `session.resume` and `params.payload.mcp_servers` from a `branch`, and for a `workspace` move it binds the new directory first, sets `params.payload.cwd` to the placeholder, and binds the previous directory back when the gateway refuses the move.

Session checks. For every request (and every notification) whose `params.session_id` is a non-empty string, the hub asks the plugin for the session's binding (through lineage) and refuses the request unless it is bound to this device (`hermes_device_not_bound` when it is not bound, `hermes_device_other_device` when it is bound to another device). A `params.parent_session_id` bound to another device is refused the same way. A lookup that succeeds is trusted for 10 seconds. When the lookup fails, the device gets `hermes_device_bind`. A notification that fails a check is dropped. The hub binds every session its own create rewrite creates, so the check passes for them.

**`session.resume`**

Resuming in `hermes-device chat` means "continue this session here". The hub resumes only by an explicit `session_id`, so the splice first finds the session id:

1. A resume with a `latest` member, or with no `title` member and an empty or missing `session_id`, is a resume of the latest session: a session bound to this device, closed ones first (lookup below).
2. A resume with a `title` member is a resume of the session with that title.
3. Otherwise `params.session_id` may be a session id or a title. The Ink TUI sends a title there: `/resume <title>` sends it as `session_id`, and so does a `--resume <title>` that the local Hermes launcher could not match in its own session store (hermes_cli/main.py `_resolve_chat_session_args`, ui-tui/src/app/useSessionLifecycle.ts). The splice looks the name up and resumes the id it finds; when nothing matches, it resumes `params.session_id` as sent.

The splice skips the lookup for a `session_id` this chat run already bound. When a lookup of 1 or 2 finds nothing, it answers the TUI with `hermes_device_not_found` (5.6) and forwards nothing. When a lookup fails, it answers with the bind error. It then removes `latest`, `title` and `workspace` from the params and sets `params.session_id` to the id.

Lookups use the splice's own requests on the TUI's gateway leg (ids `hermes-device-<n>`), or on a leg of their own for `POST /control/lookup` (ids `hermes-device-lookup-<n>`, 5.5). They run off the TUI's reader, so the TUI's pongs are still read, and each call waits at most 10 seconds. Both methods are allowed by the hub:

- A name: `session.list` with `{"title": <name>}`. The gateway resolves it as `session.resume` would, without attaching: a session id first, then a title (the latest `"<title> #N"` continuation), projected to the compression tip (gateway/session_local_title.py `resolve_titled_session`). The splice takes the first row's `resolved_id` (else `session_id`, else `id`).
- The latest session: `session.list` with `{"scope": "stored", "limit": 200}`, then `session.list` with `{"limit": 200}`. The stored listing holds the closed local sessions (most recently active first), messaging conversations and legacy local rows (gateway/session_listing.py). The live listing holds the open sessions, newest start first (gateway/session_controls.py `list_sessions`). Neither gives a last-active time that orders both, so the splice takes stored rows first: a session closed in a TUI is the usual target of `-c`, and an open session may be in use in another terminal, whose commands would then run in this directory. The device cannot read bindings, so the splice asks the hub about each row in that order, at most 200 sessions, since the listings hold the user's sessions of every surface: it sends `session.info` with `{"session_id": <id>, "hermes_device_probe": true}`. The hub checks the `session_id` of every request (below) and refuses one that is not bound to this device with `hermes_device_not_bound` or `hermes_device_other_device`; the splice then tries the next row. Any other answer means the hub let the request through, so the session is this device's (the extra member makes the gateway answer `invalid_params` at once). A `hermes_device_bind` refusal (the hub could not check) fails the lookup. The latest session is not limited to the current directory, and the resume moves it here (below).

Then, before forwarding, bind the session to the directory `chat` ran in (from the per-run secret, section 5.5). A session already bound to that directory keeps its binding; one bound to another directory of this device moves to this one. If the bind fails (for example `409` `bound_to_other_device`), answer the TUI with the bind error and forward nothing. The bind is skipped when this chat run (the same per-run secret) already bound the session, so a TUI reconnect does not undo a workspace move. Remove `params.editor.mcp_servers` if present.

The hub enforces this on the leg. It refuses a resume by `title` or `latest`, or with an empty or missing `session_id`, with `-32601` (5.4): the gateway would attach the session it picks before the hub could check it. That is why the splice looks the session up first. It also refuses an `api_key` member. It refuses a resume of a `session_id` that is not bound to this device (session checks, above), and it checks every resume answer, since the gateway may answer with another session (a compression tip, or the messaging session that took the conversation over): when the answer's `session_id` is not bound to this device, it detaches the subscription (`session.detach`, with the answer's `subscription_id`) and answers the device with the same refusal. The hub binds nothing on a resume.

**`session.mutate` with `operation` `workspace`** (a workspace move)

1. Bind `params.session_id` to `params.payload.cwd` (a device path). On failure, answer the TUI with the bind error and forward nothing.
2. Set `params.payload.cwd` to the placeholder and forward.
3. If the gateway refuses the move, bind the session back to its previous directory when the daemon knows it.

**`session.mutate` with `operation` `branch`**

Remove `params.payload.mcp_servers` if present, then forward. On a result with `branched_session_id`, bind that id to the parent's binding directory (one this daemon bound; for a parent it never bound, skip the bind and log it) before passing the response on. (The plugin also inherits bindings through lineage, section 8.3; the explicit bind covers a child whose lineage row is not written yet.) If the bind fails, pass the response on anyway and log the error: lineage still fails the child closed.

### 5.4 Allowed and denied calls

The hub lets through only an allowlist (`gatewayRules` in hermes-hub internal/device/rpc.go): the methods the Hermes v0.22.7 Ink TUI and classic CLI send for chatting. It refuses every other method, and the requests below, with error code `-32601`, reason `hermes_device_denied` and message `<method> is not available through hermes-device: <why>`, and forwards nothing. A refused notification is dropped. The session checks of 5.3 apply to every method that passes.

| Methods | Rule |
|---|---|
| `gateway.ping`, `runtime.describe`, `setup.status`, `session.detach`, `session.list`, `session.active_list`, `session.most_recent`, `session.info`, `session.stats`, `session.export`, `session.context_breakdown`, `session.control.read`, `session.interrupt`, `terminal.resize`, `prompt.receipt`, `prompt.cancel`, `prompt.resolve_unknown`, `approval.respond`, `clarify.respond`, `clarify.lock`, `command.observe`, `commands.catalog`, `complete.slash`, `plugins.list`, `tools.show`, `subagent.list`, `subagent.tail`, `subagent.steer`, `subagent.interrupt`, `image.attach_bytes`, `image.detach`, `learning.detail`, `pet.info.meta`, `pet.gallery`, `i18n.catalog`, `system.battery`, `shared_metrics.slash_command`, `shared_metrics.startup_latency` | Pass unchanged |
| `prompt.submit`, `prompt.replace_queued`, `session.steer`, `session.redirect`, `prompt.btw` | Pass unless `text` is a slash command that is not allowed (below) |
| `session.create`, `session.resume` | Rewritten and held (5.3). Refused with a `title`, `base_url`, `api_key` or `accept_hooks` member (create), or with `title`, `latest`, `api_key`, or an empty or missing `session_id` (resume). `cwd_explicit` is removed from a create |
| `session.mutate` | Only the operations `rename`, `archive`, `sidebar`, `model`, `reasoning`, `workspace`, `edit_policy`, `delete`, `rewind`, `reset`, `branch` and `compress`. `workspace` and `branch` are rewritten (5.3). `editor_mcp` (spawns editor MCP programs on the pod) and `import` (reads other agents' histories from pod files) are refused |
| `complete.path` | Answered `{"items": []}` as a result by the hub and the splice: it lists pod directories |
| `config.set` | Only `key` `busy`, `verbose`, `show_reasoning`, `service_tier`, `fast` or `yolo` with no `scope` member, and `reasoning` with `scope` absent or `session` and a `value` that is exactly an effort level or `reset`. Every other key writes profile-wide state of the pod (display keys write `config.yaml`) |
| `config.get` | Refused with a `cwd` member or with `key` `project`: both run git in a pod directory |
| `slash.exec`, `command.dispatch` | Only the slash-command allowlist below. A `/model` gets `--session` put first |
| `file.attach` | Refused with a non-null `path` (a pod path) or a `staged_upload` member (copies a browser upload out of the pod) |
| `model.options` | `refresh` is removed: it refetches every provider's model list for the whole profile |
| `learning.frames`, `pet.cells` | Size params are clamped (`learning.frames`: `cols` 20 to 400, `rows` 10 to 200, `frames` 1 to 120; `pet.cells`: `cols` 4 to 200), because Hermes renders whatever size it is asked for. A size that is neither null nor a number is refused with `-32602` |

The methods the TUI or CLI send that the hub refuses, with the reason in the message:

| Methods | Why |
|---|---|
| `shell.exec`, `process.list`, `process.stop`, `process.kill`, `reload.mcp`, `browser.manage`, `kanban.run` | Run programs in the pod, not on the device |
| `session.save`, `spawn_tree.save`, `image.attach`, `clipboard.paste`, `input.detect_drop`, `paste.collapse`, `rollback.list`, `rollback.diff`, `rollback.restore`, `rollback.session_diff` | Read or write files of the pod |
| `tools.configure`, `plugins.manage`, `skills.manage`, `skills.reload`, `reload.env`, `delegation.pause`, `pet.select`, `learning.edit`, `learning.delete`, `session.automation`, `onboarding.start`, `onboarding.state`, `projects.tree` | Change settings or data of the whole profile |
| `model.save_key`, `model.disconnect`, `connection.respond`, `connectors.*` (the TUI's methods), `billing.*`, `subscription.*`, `free_tier.status` | Act on credentials or the account of the profile |
| `wake.*`, `voice.toggle`, `voice.record`, `display.observe` | Use the pod's own audio or display |
| `session.prune`, `delegation.status`, `spawn_tree.list`, `spawn_tree.load` | Act on or read many sessions at once (`delegation.status` and the spawn tree methods read every session's subagents and spawn trees) |
| `prompt.background` | A background agent has no lineage to its parent session (gateway/session_tui_side.py), so it would run on the pod. The plugin also refuses it for bound sessions (section 8.3) |
| `profiles.describe`, `profiles.list` | Read or change other profiles |
| `session.activate`, `session.title`, `session.close`, `session.delete`, `session.branch`, `session.compress`, `session.undo`, `session.status`, `session.usage`, `session.archive` | Legacy methods the gateway refuses on the hub's connection anyway |
| `setup.runtime_check`, `shared_metrics.update_run` | Not part of chatting |
| every other method | Not in the allowlist |

The hub also refuses, for every method:

- a request with a `profile` member that is not null, of any type: the gateway would route it to a sibling profile that may not run device sessions. Error `-32601`, message `hermes-device serves only the device's own Hermes profile`, reason `hermes_device_denied`;
- a request with a `request_id` param that starts with `hermes-hub-`, the prefix of the hub's own requests: error `-32600`.

The slash-command allowlist is `new`, `clear`, `redraw`, `history`, `retry`, `undo`, `title`, `compress`, `stop`, `status`, `context`, `usage`, `help`, `commands`, `version`, `copy`, `queue`, `steer`, `btw`, `branch`, `fast`, `yolo`, `reasoning`, `model`, `goal`, `plan`, `review` and `whoami`: commands that act on the session only, or only read. `verbose` is not in it (it writes the profile's `config.yaml`), nor `diff` (it runs git in the pod) or `refine` (it reviews into the profile's memory and skills). The hub splits a `slash.exec` command as Hermes does (strip, remove leading `/`, name up to the first space, the rest is the argument) and takes `name` and `arg` of `command.dispatch` the same way. Then:

- An argument that Hermes' flag parsers may read as `--global` is refused: the hub drops quotes and backslashes, turns figure, en, em and bar dashes into `--`, lowers the case, and refuses an argument that then contains `--global`.
- `/reasoning` takes no argument, or exactly an effort level or `reset`.
- `/usage` takes no argument: `/usage reset` redeems an account credit.
- `/model` with `--refresh` is refused. Every other `/model` gets `--session` put first (`slash.exec` command `/model --session <rest>`, `command.dispatch` `arg` `--session <arg>`), where no value flag can take it as its value: without `--session`, Hermes writes the pick to the profile's `config.yaml` when the profile has no default model or sets `model.persist_switch_by_default` (hermes_cli/model_switch.py `resolve_persist_behavior`).

Prompt text. `prompt.submit` and `prompt.replace_queued` admit their `text` as a message event, which runs a slash command (gateway/run_inbound.py), and the other text methods hand it to the model; the hub checks the `text` of all five as Hermes reads a command (MessageEvent `get_command`): it strips leading whitespace (Python's `str.isspace`), then every leading line that starts with `@` (a superset of the attachment refs Hermes strips, gateway/platforms/event.py `_ATTACHMENT_REF_RE`), then leading whitespace again. Text that then starts with `/` names a command: the name is the lower-cased word up to the first whitespace, without a `@bot` suffix, and the rest without leading whitespace is the argument. A name that is empty or holds `/` is plain text. A name that is not plain ASCII is refused. An alias counts as its command (`reset` as `new`, `fork` as `branch`, `compact` as `compress`, `q` as `queue`, `s` as `steer`, `ctx` as `context`, `v` as `version`, from hermes_cli/commands.py). The command must pass the slash rules above, and `/model` typed as text is refused, since its value flags can take a `--session` as their value (hermes_cli/model_switch.py `parse_model_flags_detailed`); the TUI sends `/model` through `slash.exec`.

The splice applies a shorter deny list first, so the TUI gets these refusals without a round trip, with the same error code and message `<method> is not available through hermes-device`: `model.save_key`, `profiles.create`, `shell.exec`, `cli.exec`, `image.attach`, `input.detect_drop`, `clipboard.paste`, `mcp.servers.add`, `mcp.servers.test`, `mcp.servers.remove`, `plugins.manage`, `skills.manage`, `cron.manage`, `cron.submit`, `kanban.run`, `browser.manage`, `display.install`, `display.start`, `a2a.forward`, `process.kill`, `process.list`, `rollback.restore`, `set_api_key`, `diagnostics.share_nous`, `free_tier.provision`, `session.foreign.import`, `prompt.background`, `vault.*`, `worker.*`, `connectors.*`, `oauth.*`, a `config.set` that the hub refuses (the same rule), `pdf.attach` and `file.attach` with a non-null `path`, and `session.mutate` with `operation` `editor_mcp`. Everything else is left to the hub. The plugin guards what the allowed calls reach (section 8.3).

The hub enforces these rules on the gateway leg, so a holder of the device token who dials the gateway leg without the splice meets them too (section 10).

### 5.5 Daemon control

`hermes-device run` writes `<state dir>/daemon.json` (mode `0600`, directory `0700`):

```json
{"pid": 4242, "splice": "127.0.0.1:41733", "control_secret": "<43 base64url chars>", "device_id": "d_8f2c", "hub": "https://hub.example.com"}
```

`hermes-device chat` reads it and calls, on the splice address:

- `POST /control/tokens` with `Authorization: Bearer <control_secret>` and `{"cwd": "<absolute device dir chat runs in>"}`: answers `201` with `{"token": "<43 base64url chars>"}`, a new per-run secret tied to that directory (used for `session.resume`, 5.3). A missing or relative `cwd` gets `400`; a daemon that is stopping answers `503`.
- `DELETE /control/tokens/<token>` with the same header when the TUI exits: `204`. Open TUI sockets that used it are closed.
- `POST /control/lookup` with the same header and `{"session": "<id or title>"}` or `{"latest": true}`: the daemon opens a gateway leg of its own, looks the session up as for a resume (5.3, at most 2 minutes), and closes the leg. It binds and attaches nothing. Answers `200` `{"session_id": "<id>"}`, `404` `{"error": "not_found"}`, `502` `{"error": <hub error code, else hub_unreachable>, "message": <text>}` when the lookup fails, and `400` for a body that is not JSON or has neither or both of `session` (non-empty) and `latest` (true). Other members are ignored.

The daemon refuses the control endpoints from non-loopback peers and with a wrong secret (`403`). It removes `daemon.json` on a clean exit.

### 5.6 Errors to the TUI

The splice and the hub answer a request they refuse or fail with `{"jsonrpc": "2.0", "id": <same id>, "error": {"code": <code>, "message": <text>, "data": {"reason": <reason>}}}`:

| Case | `code` | `data.reason` |
|---|---|---|
| Denied method or params (5.4). The hub's message adds the reason: `<method> is not available through hermes-device: <why>` | `-32601` | `hermes_device_denied` |
| Missing or relative cwd | `4001` | `hermes_device_cwd` |
| `safe_mode` or `ignore_user_config` create | `4001` | `hermes_device_bypass` |
| Bind failed, or the splice's session lookup failed (5.3) | `4001` | `hermes_device_bind`, plus `data.hub_error` with the hub's error code (`unauthorized`, or `hub_unreachable` when the hub gave none). When the hub itself fails a bind or a binding lookup, `data.hub_error` is the plugin's error code, or `backend_unavailable` when the plugin gave none |
| Session bound to another device (hub only) | `4001` | `hermes_device_other_device` |
| Session not bound to this device (hub only) | `4001` | `hermes_device_not_bound` |
| No session to resume: no session bound to this device for a latest resume, or none with the title (splice only) | `4001` | `hermes_device_not_found` |
| Message is not one JSON object, such as a batch (hub only) | `-32600` | none, `id` is `null` |
| Request id that is not a string or a number (hub only) | `-32600` | none, `id` is `null` |
| Request id, or `request_id` param, that starts with `hermes-hub-` (hub only) | `-32600` | none |
| `params` that is not an object (hub only) | `-32602` | none |
| `session_id` or `parent_session_id` that is not a string (hub only; a `session.resume` gets the `-32601` of 5.4 first) | `-32602` | none |
| Size param of `learning.frames` or `pet.cells` that is neither null nor a number (hub only) | `-32602` | none |

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

acp-host also refuses, with JSON-RPC error `-32602`, any editor request or notification whose `params.sessionId` (or `params.session_id`) is not bound in `bindings.json` to the assertion's `device_id`: message `session is bound to another device` when the session's binding names another device, and `session is not bound to this device` when the session has no binding of its own. Only the session's own entry counts, because acp-host cannot read the lineage: a compression child or branch of a session bound to another device has no entry, so it is refused too. The bridge binds every session before it uses it (6.3), and the plugin refuses that bind for a session of another device's lineage (section 9 PUT rules), so editor sessions always have an entry. acp-host checks the params after the pinning of 6.2, so it sees the id Hermes will use. When `bindings.json` cannot be read or parsed, these calls get `-32603`.

The hub reads every message on the ACP leg too (`acp.go` in hermes-hub), so acp-host's checks are not the only ones:

- A device may send only `initialize`, `authenticate`, `session/new`, `session/list`, `session/load`, `session/resume`, `session/fork`, `session/prompt`, `session/set_mode`, `session/set_model` and `session/set_config_option` as requests, and `session/cancel` as a notification: the methods of the ACP SDK that Hermes v0.22.7's gateway ACP agent implements (acp_adapter/gateway_server.py). Any other request gets `-32601` with reason `hermes_device_denied` and message `<method> is not available through hermes-hub`; any other notification is dropped. `session/close` is refused this way.
- The hub pins the params per method: they may have only the members the ACP SDK (v0.9.0, acp/schema.py) defines for that method, under the alias or the field name (the members kept in the 6.2 table, plus `sessionId` and `session_id` for the methods with a session). Any other member is refused with `-32602`, and so is a `field_meta` member at any depth. A top-level `_meta` is refused in this way: the SDK merges it into the handler's arguments, where it would replace the session id or cwd the hub checked. (acp-host removes it instead, 6.2.) Params that are not an object get `-32602`.
- Every method except `initialize`, `authenticate`, `session/new` and `session/list` must name its session in `sessionId` or `session_id` (both are checked when both are sent, each a non-empty string). The session must be bound to this device, through its lineage, or have been created by `session/new` or `session/fork` on this leg and not be bound to another device since. Otherwise the answer is `-32602` with reason `hermes_device_other_device` or `hermes_device_not_bound`, or `-32603` with reason `hermes_device_bind` when the binding cannot be read.
- As acp-host does (6.2), the hub sets `cwd` to the plugin's placeholder, sets `mcpServers` to `[]` and removes `mcp_servers` on `session/new`, `session/load`, `session/resume` and `session/fork`, replaces `resource_link` prompt blocks with a text note, and keeps only the sessions bound to this device in `session/list` answers (no rows when the bindings cannot be read). It refuses a `session/prompt` with any `text` member, at any depth, that is a slash command the gateway leg's prompt text rule refuses (5.4), with `-32602` and reason `hermes_device_denied`: `hermes acp` sends those to the gateway as `slash.exec` over its own connection, which the hub does not see.
- From acp-host, the hub relays `session/update` (a notification), `session/request_permission` and `elicitation/create` to the device, and answers any other agent request, such as `fs/read_text_file` or `terminal/create`, with `-32601` itself: they would act on the device outside the device's own rules. Other agent notifications are dropped.
- The hub forwards each device request under an id of its own (`hermes-hub-a<n>`), gives the answer the device's id back, and drops answers to ids it did not send. A device request with an id that starts with `hermes-hub-` is refused with `-32600`.

### 6.2 acp-host rewrites (editor to agent)

Params pinning comes first, for every editor message with a `method`. The ACP SDK in Hermes (agent-client-protocol 0.9.0) validates params against the method's request model and calls the handler with the model's fields as keyword arguments, then merges `params._meta` into them (acp/router.py `_make_func`, acp/utils.py `model_to_kwargs`). The SDK also accepts each field name in place of its alias (`populate_by_name`), so `field_meta` works as `_meta`, and `session_id` as `sessionId`. A `_meta` member named like a handler argument replaces it: `{"sessionId": <own>, "_meta": {"session_id": <other>}}` would run on the other session after the session check, and `"_meta": {"cwd": ...}` would replace the placeholder. The SDK merges only the top-level `_meta`; a `_meta` inside a content block or capability stays on that object, which Hermes does not read. So:

- For a method the SDK routes to Hermes, acp-host keeps only the members of its request model, each under its alias. A field name folds into the alias; when both are present the alias wins, as in the SDK. Everything else, including `_meta` and `field_meta`, is removed. Params that are not an object are refused (`-32602`).

  | Method | Members kept |
  |---|---|
  | `initialize` | `protocolVersion`, `clientCapabilities`, `clientInfo` |
  | `authenticate` | `methodId` |
  | `session/new` | `cwd`, `mcpServers` |
  | `session/load`, `session/resume`, `session/fork` | `sessionId`, `cwd`, `mcpServers` |
  | `session/list` | `cursor`, `cwd` |
  | `session/close`, `session/cancel` | `sessionId` |
  | `session/set_mode` | `sessionId`, `modeId` |
  | `session/set_model` | `sessionId`, `modelId` |
  | `session/set_config_option` | `sessionId`, `configId`, `type`, `value` |
  | `session/prompt` | `sessionId`, `prompt`, `messageId` |

- Any other method loses the `_meta` and `field_meta` members of object params; its other params pass. The SDK answers such methods with method not found today (Hermes registers no extension methods); this covers a method a later SDK adds.

The session check of 6.1 then runs on the pinned params. acp-host removes `_meta` instead of refusing the message; the hub in front of it refuses a top-level `_meta` and a `field_meta` at any depth (6.1), so through the hub an editor must not send them.

| Method | Rewrite |
|---|---|
| `session/new`, `session/load`, `session/resume`, `session/fork` | `params.cwd` becomes the placeholder (`acp-host.placeholder`). `params.mcpServers` becomes `[]`: editor MCP servers would run as programs in the pod. The same values on every call keep `session/load` from triggering a workspace move or an MCP rebind |
| `session/list` | `params.cwd`, when present, must be absolute and is removed: TUI sessions keep their device dir as row cwd, so a placeholder filter would hide them (acp_adapter/catalog.py). The request gets an id of acp-host's own, `hermes-device-list-<n>`, and the response gets the editor's id back, so the response is matched whatever id form the editor used (Python re-encodes ids, and treats `"id": null` as a request). The result is filtered (below) |
| `session/prompt` | Every content block of type `resource_link` is replaced by `{"type": "text", "text": "[hermes-device: link <uri> removed; the bridge inlines device files]"}`. `hermes acp` reads a link as a pod path whatever its form: `file://` in any case, a bare path, or a drive path (acp_adapter/content.py `_path_from_file_uri`). Then a prompt that would run a slash command the rules below refuse is refused (`-32602`, message `/<name> is not available through hermes-device`) |

Slash commands in prompts: `hermes acp` runs a prompt whose text starts with a registered command as that command, and sends every command it does not answer itself to the gateway's `slash.exec` over its own connection (acp_adapter/gateway_server.py `_slash_command` and `_run_slash`), which the hub does not see. So acp-host applies the gateway leg's prompt text rule (5.4) to prompts, as the hub does on the ACP leg: it joins the string `text` members of the prompt's blocks with newlines (after the link rewrite), strips leading whitespace as Python does, then every leading line that starts with `@`, then leading whitespace again, and when the result starts with `/`, takes the word up to the first whitespace and the rest, stripped, as the argument. The name is the word lower-cased, without a `@bot` suffix. A name that is empty or holds `/` is prompt text, as in Hermes. Any other word must be ASCII, and the name, with an alias counted as its command, must be in the 5.4 allowlist without `model`, or be `discard` or `tools` (which `hermes acp` answers for the session itself). The hub's own ACP check (6.1) does not allow `discard` and `tools`, and it checks every `text` member on its own, so through the hub those two are refused too. The argument must not read as `--global` in any of the forms of 5.4, `/reasoning` takes no argument, an effort level or `reset`, and `/usage` takes no argument. Hermes builds the text from more blocks, but the others only add text that starts with `[`, so this refuses every command Hermes would run, and a few prompts that Hermes would send as text. `/device` is not in the list: through ACP it would let a device move its session to another device (8.8).

Rewritten messages are decoded and encoded again, so a duplicate key keeps its last value, as in Python. acp-host answers these itself and forwards nothing: a message that is not JSON (`-32700`), JSON that is not one object, such as a batch (`-32600`), and `params` of a pinned method that are not an object, a `prompt` that is not an array, or a relative list `cwd` (`-32602`). A refused notification (no `id` member) gets no answer.

`session/list` results: acp-host reads `$HERMES_HOME/device/bindings.json` (section 8.2; setting `acp-host.bindings-file` overrides the path) read-only, on every list, keeps only rows whose session is bound to the assertion's `device_id` (and, when the request named a cwd, whose binding dir is that cwd or below it), and sets each kept row's `cwd` to its binding dir. A missing file keeps no rows; so does a file that cannot be read or parsed (logged). `nextCursor` passes unchanged, so a filtered page can be empty while `nextCursor` is still set.

Everything else passes unchanged in both directions, including the agent's requests to the editor (`session/request_permission`, `elicitation/create`) and the editor's answers to them.

### 6.3 Bridge behavior (`hermes-device acp`)

The bridge runs on the device, so it can read the editor's files and it knows the editor's real cwd.

1. `session/prompt`: each `resource_link` block with a `file://` URI (host empty or `localhost`) of a regular file up to 1 MiB of valid UTF-8 is replaced by a `resource` block with `{"uri": <uri>, "mimeType": "text/plain", "text": <contents>}`. Other links pass, and acp-host replaces them.
2. `session/new`, `session/fork`, `session/load` and `session/resume` need an absolute `params.cwd`; otherwise the bridge answers `-32602` and forwards nothing. `session/new` and `session/fork`: on a result, bind `result.sessionId` to the request's `cwd` (section 7.4) before passing the result to the editor. If the bind fails, answer the editor's request with error `-32603`, message `hermes-device: could not bind the session to <cwd>: <hub error>`. (ACP has no delete; the unbound empty session is not listed, because Hermes lists only sessions with messages.)
3. `session/load` and `session/resume`: bind `params.sessionId` to `params.cwd` before forwarding. A session bound to the same directory keeps its binding. On failure, answer with the error above and forward nothing.
4. Reconnect: when the ACP leg drops, the bridge answers every editor request still waiting with error `-32603`, message `hermes-device: connection to Hermes lost; retry`, and reconnects with backoff. On the new connection it sends the params of the editor's last successful `initialize` and consumes the response, then, for each session the editor opened (a successful new, fork, load or resume), sends `session/load` with `{"sessionId", "cwd", "mcpServers": []}` and drops every `session/update` for that session until the load's response, which it also consumes. Those updates are the replayed history the editor already has. A load that fails is logged and the session is forgotten, and so is a session the editor closed with a successful `session/close`. Editor messages wait until the replay is done, at most 30 seconds; a request still waiting then is answered with the error above.
5. Ids: the bridge sends every editor request to the agent under a string id of its own, `hermes-device-<n>`, and gives the response the editor's id back. Python re-encodes ids (`1e0` comes back as `1.0`, `"\u00e9"` as `"é"`), so this is the only way to match every response. The bridge's own requests use the same form. An editor message over 48 MiB is answered with `-32600`, because acp-host would close the leg.
6. Agent requests (`session/request_permission`, `elicitation/create`) reach the editor with a bridge id `hermes-device-<n>`, and the editor's answer goes back under the agent's id. A new `hermes acp` process numbers its requests from the start again, so this keeps an answer meant for a lost connection from reaching a new request with the same id. Answers to requests of a lost connection are dropped.
7. The bridge stops with a non-zero exit when the hub refuses the token (HTTP `401` on the upgrade or on a bind, or close `4401`) or closes with `4426`. Every other close or dial failure is retried with backoff: a close with `1011` (the `hermes acp` child exited, as when acp-host restarts) or `1001` (hub shutdown), a connection that ends without a close, and an upgrade answered with a 5xx status. Each reconnect replays as in point 4. Stdout carries only JSON-RPC lines; logs go to stderr.

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

The client prints `verification_uri` and `user_code`. On the hub page the user signs in and types the code; the hub never takes the code from the link, so a link someone else sends cannot pair their device to the user's backend. The confirm page shows the requesting device's address, `hostname`, `os` and the request time. The user names the device and confirms. The hub rate-limits both unauthenticated pairing endpoints per address and limits wrong code entries per user. A rate-limited start answers `429` `{"error": "rate_limited"}`. A rate-limited poll answers `400` `{"error": "slow_down"}`.

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

The user revokes a device on the hub page. The hub then refuses the token (section 2.1) and closes the device's open legs with `4401`. On the hub replica that served the revoke it does so at once. Other replicas close the legs they hold at their next check, within `device.recheck_interval` (default 10 seconds). The same check closes a leg when the device's user may no longer use its backend.

### 7.3 Whoami

`GET /api/device/whoami` with the device token. Answer `200`:

```json
{"device_id": "d_8f2c", "device_name": "desktop", "user": "alice", "backend": "alice",
 "hermes_version": "v0.22.7", "plugin_version": "0.2.0", "placeholder": "/opt/data/hermes/device-placeholder"}
```

Service devices (section 8.10) may call whoami too. Their `user` is `service`.

`hermes_version`, `plugin_version` and `placeholder` come from the plugin's `GET /info` (section 9); if the backend is unreachable the hub answers `502` `{"error": "backend_unavailable"}`. `chat` compares `hermes_version` with the first line of the local `hermes --version` (`Hermes Agent v<version> (<date>)`), exactly after removing a leading `v`, and refuses to start on a mismatch (the local TUI and the server must be the same release).

### 7.4 Session binding

`PUT /api/device/sessions/{session_id}/binding` with the device token and `{"dir": "/home/alice/src/app"}`.

Before it forwards a bind, the hub asks the plugin how it resolves the session through its lineage (`GET /sessions/{session_id}/binding`, section 9) and refuses the bind itself:

- `409` `{"error": "bound_to_other_device"}` for a session that resolves to another device, including one that only inherits that binding from an ancestor (a compression child or branch of another device's session);
- `409` `{"error": "was_bound"}` for a session that resolves fail-closed: it or its lineage was bound before and is not now, or the plugin cannot read its lineage. The exception is a session that a leg of the same hub replica created for this device in the last hour (a `session.create` the hub bound, a branch child of a `session.mutate` `branch`, or a session of ACP `session/new` or `session/fork`), whose lineage row may not be written yet. So a device that unbinds its own session cannot bind it again;
- `502` `{"error": "backend_unavailable"}` when the plugin cannot be reached.

So the hub forwards only a bind of a session that resolves to this device, or to no device with no session in its lineage ever bound. It forwards to the plugin as `PUT /api/plugins/device/sessions/{session_id}/binding` with `{"device_id": <the token's device id>, "dir": <dir>}`; the plugin applies its own rules (section 9 PUT rules), and the hub passes the plugin's status and body through.

A session whose frozen terminal backend is not `device`, or that runs in a bypass (safe mode) worker, cannot be bound: the plugin answers `409` `not_device_session`.

`DELETE /api/device/sessions/{session_id}/binding` with the device token: the hub forwards it only when the session's own binding belongs to the token's device, else `403` `{"error": "forbidden"}`. A binding the session only inherits through lineage does not count.

Service devices (section 8.10) get `403` `{"error": "forbidden"}` on the bind and unbind API, on the gateway leg and on the ACP leg. They may use only the exec leg and whoami.

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
| `fallback` | `local` | What an unbound, never-bound session runs on: `local` (the pod shell, as without the plugin), `refuse`, or `device` (the sandbox device, section 8.10). `device` without `fallback_device` acts as `refuse` |
| `fallback_device` | none | `fallback: device` only: the sandbox device, by device id (preferred) or by the unique name of a connected device |
| `grace_seconds` | `120` | How long the pending commands of a dropped version 3 exec leg wait for the device to reconnect and resume them (section 4.7). At most 3600. `0` turns resume off |
| `fallback_root` | `/work` | `fallback: device` only: absolute directory on the sandbox device that holds one working directory per session lineage. Must be inside the sandbox's roots |
| `browser` | `true` | `false` hides the `device-browser` toolset (docs/browser.md section 3) |
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
7. Only a session whose whole lineage was read and never had a binding runs on the pod (setting `fallback: local`), is refused (`fallback: refuse`), or runs on the sandbox device (`fallback: device`, section 8.10). Rule 5 applies first, so a placeholder session never goes to the sandbox.
8. A bound session whose device is not connected fails the command with exit 255 and a note; it never falls back. A never-bound session whose sandbox device is not connected fails the same way (section 8.10).
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

`/device <device> <dir>` is the owner's explicit action, so it binds with operator rules, which are wider than the PUT rules of section 9:

1. A session whose own binding names another device moves to the named device. No `/device off` is needed first.
2. A session that is not bound now binds, whether or not it or its lineage was ever bound (after `/device off`, or a session whose lineage cannot be read).
3. A session that only inherits a binding of another device from an ancestor (a compression child or branch) is refused with `bound_to_other_device`: binding it would carve it out of that device's lineage. Run `/device off` in the session first (it removes the ancestor's binding), then bind.

Devices cannot run `/device`: the hub refuses it on the gateway leg (5.4) and acp-host refuses it in ACP prompts (6.2).

### 8.9 Dev mode

With `dev_static_tokens: true` the exec route also accepts version 1 to 3 `hello` messages with `name` and `token` and no assertion, checked against `devices`. It is for local tests without a hub. Never enable it on a pod the hub serves.

### 8.10 Sandbox fallback

With `fallback: device`, a session that rule 7 of section 8.3 finds never bound runs on the sandbox device `fallback_device`, a service device that the hub knows from its own config and that authenticates on the exec leg only. The sandbox is a separate pod (image `hermes-device-sandbox`, one per person, in its own namespace) that runs `hermes-device run --no-splice`.

1. The plugin gives the session an implicit binding: device = the connected device whose id is `fallback_device`, else, only when no device with that id connected since the gateway started, the one connected device with that name; dir = `<fallback_root>/<name>`, where `<name>` is the id of the session's lineage root (the session at the top of its `parent_session_id` chain) when it matches `[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}`, else a cleaned prefix of the id, `~`, and the first 12 hex digits of its SHA-256 (a name that passes unchanged never holds `~`, so two ids never share a directory). Every session of one lineage (compression children, subagents, branches, side agents) shares the directory.
2. The implicit binding is computed for each resolution and never stored. It is not in `bindings.json`, not in `ever_bound`, and not returned by `GET /bindings`, so acp-host does not filter on it and section 9's PUT works on the session as on any never-bound session.
3. Everything that treats a bound session specially treats it the same way: the terminal backend runs its commands on the sandbox in that directory, `uses_host_filesystem` is false, the guard (8.6) rewrites file tool paths and checks writes against the denylist rebuilt for the sandbox's `home` and its roots, edit approval and context references read from the sandbox, checkpoints are skipped, and the tools of `POD_ONLY_TOOLS` and bypass (safe mode) turns are refused. As for a bound session whose terminal backend is not `device`, the guard blocks every tool call of a never-bound session whose frozen backend is not `device` (a session created before `terminal.backend: device`).

   One difference: the session's server cwd stays `terminal.cwd`, not the placeholder (rule 5 of 8.3 would fail it closed). Hermes builds the system prompt in the gateway and reads project context files (`AGENTS.md`, `.hermes.md` and the like) and the git state from that server directory (agent/system_prompt.py). The agent cannot choose these paths, but keep `terminal.cwd` an empty directory, so the prompt holds no pod files.
4. Before the first command whose cwd is in that directory, on each connection of the sandbox, the plugin runs `mkdir -p -- <dir>` on the sandbox, with cwd the sandbox root that holds `fallback_root`, so `fallback_root` itself may be missing. If that fails, the command fails with exit 126 (255 when the sandbox dropped) and a note, and nothing else runs. When the sandbox refuses a command in that directory (for example because the agent deleted it), that command fails and the next one creates the directory again.
5. The command fails, and never runs on the pod, when the sandbox device is not connected, when the name matches more than one connected device, when `fallback_root` is not an absolute path, and when `fallback_root` is not inside the sandbox's roots from `hello`. The terminal gets exit 1 and a message that names the sandbox; a command whose sandbox drops while it is sent gets exit 255. File tools are blocked with the same message.
6. An explicit bind (`/device <device> <dir>`, the splice, the ACP bridge or the PUT route) wins, and the PUT rules of section 9 allow it, since the implicit binding is not stored: the session then runs on that device, and is in `ever_bound` like any bound session. After `/device off` it fails closed (rule 3); it does not return to the sandbox.
7. `/device` without arguments names the sandbox and the directory; `/device off` answers that no device was bound.

Name the sandbox by its device id. A name lookup takes any connected device of that name, including a PC the user named the same, and an id falls back to a name lookup until the sandbox has connected once since the gateway started. Either way the device must also have `fallback_root` inside its roots.

The plugin also checks `fallback_root` against the pod-writable paths of section 8.7. Add it to `device_write_roots` and to `HERMES_WRITE_SAFE_ROOT` when that variable is set, or file tools cannot write there.

The bindings file format does not change (version 2).

## 9. Plugin HTTP routes

Mounted by the dashboard under `/api/plugins/device/` on the gateway listener (9120). They are behind dashboard sign-in, so the hub calls them with its dashboard session for the user. Errors are `{"error": "<code>", "message": "<text>"}`.

| Route | Answer |
|---|---|
| `GET /info` | `200` `{"plugin_version", "hermes_version", "protocol_version": 3, "backend_id", "placeholder"}`. `protocol_version` is the newest exec leg version the plugin speaks |
| `GET /devices` | `200` `{"devices": [Device]}` |
| `GET /devices/{device_id}/dirs?path=<abs>` | `200` `{"path": <resolved>, "entries": [{"name": "src"}]}`: subdirectories only, sorted, at most 500, hidden ones included. `path` defaults to the first root |
| `GET /bindings` | `200` `{"bindings": [Binding]}` |
| `GET /sessions/{session_id}/binding` | `200` Binding plus `"resolved": {"state": "bound" or "never_bound" or "fail_closed", "via": <session id or null>}`; `404` `not_bound` with the same `resolved`. With `fallback: device`, a `never_bound` `resolved` also has `"sandbox": {"device_id", "dir"}` for the session's implicit sandbox binding (8.10), or `"sandbox": null` when the sandbox cannot serve it |
| `PUT /sessions/{session_id}/binding` | Body `{"device_id", "dir"}`. `200` Binding |
| `DELETE /sessions/{session_id}/binding` | `204`; `404` `not_bound` |
| `GET /ws` (WebSocket) | The exec leg, section 4 |

Device object: `{"device_id", "name", "hostname", "os", "arch", "home", "roots", "client_version", "connected_at", "capabilities"}` (`connected_at` in Unix seconds; `capabilities` as negotiated, section 4.1). A device with the `browser` capability also has `"browser": {"connected", "browser", "extension_version"}` from its last `browser_status`.

Binding object: `{"session_id", "device_id", "dir", "generation", "bound_at"}`. `dir` is the device's `pwd -P` of the requested directory.

PUT rules:

- The device must be connected (`409` `device_offline`).
- `dir` must be absolute (`400` `bad_dir`), and the device must accept it as a cwd and resolve it with `pwd -P` (`422` `dir_refused`, with the device's message).
- The plugin resolves the session through its lineage (8.3) first. A session whose own binding, or the binding it inherits from an ancestor, names a different device is refused (`409` `bound_to_other_device`). So a device cannot take a compression child or branch of another device's session, which before this rule would have shadowed the ancestor's binding (the nearest binding wins).
- A session whose lineage resolves fail-closed is refused (`409` `not_bindable`, the message gives the reason): one that was bound and is not now (after `/device off`), one whose lineage cannot be read (no row in the session store yet, a store error, a cycle, too many hops), and an orphaned side agent. No device can claim these; the owner binds them with `/device` (8.8). A session the device itself bound and then had unbound is refused the same way, because `ever_bound` does not record the device.
- So a device may bind: a session that resolves to itself (its own binding, or one it inherits, such as a branch of its own session, which the splice binds after a `branch`), and a session that is unbound and was never bound anywhere in its lineage (a new session of a create, or a web chat). With `fallback: device` the implicit sandbox binding does not count (8.10), so such a session counts as never bound.
- The plugin checks these rules before it asks the device to resolve `dir`, and again under its lock before it writes, so a bind that lands meanwhile is seen.
- Binding a session to the directory it is already bound to returns the existing binding with its generation unchanged. A different directory on the same device replaces the binding with a new generation.
- `404` `unknown_device` when `device_id` is not connected and was never seen; `400` `bad_request` for a malformed body.
- `409` `not_device_session` for a session whose live frozen policy is a bypass (safe mode, ignore-user-config) policy or names a terminal backend other than `device`. The plugin can check this only for sessions loaded in the gateway; others are allowed.
- `503` `state_unavailable` when `bindings.json` cannot be read (every unbound session then fails closed), and `500` `state_write_failed` when it cannot be written.

Errors from the dirs route: `409` `device_offline` for any device that is not connected, `422` `dir_refused`, `400` `bad_dir`.

`/device <device> <dir>` uses wider rules (8.8). `GET /sessions/{id}/binding` returns the binding that serves the session, which may belong to an ancestor (`resolved.via` names it). `DELETE` removes only the session's own binding; `/device off` removes the binding that serves the current session.

## 10. Security notes

- The device daemon runs commands only in its own roots, and only on its own user's backend. It can lie about command output.
- The hub enforces the allowlist and the refused params of 5.4, the create rewrite and the session checks of 5.3 on the gateway leg, so a holder of a device token who dials the gateway leg directly meets them. A method that a new Hermes release adds is refused until the hub's allowlist names it. The hub also parses the ACP leg (6.1): it allows only the ACP methods Hermes implements, pins their params and refuses `_meta` and `field_meta`, refuses sessions that are not bound to the device, refuses slash commands outside the allowlist in prompts, rewrites cwd and MCP servers, and answers the agent's `fs/*` and `terminal/*` requests itself. Behind it, acp-host pins the params too, refuses sessions that are not bound to the device itself, applies the same slash rule to prompts (also allowing `/discard` and `/tools`, which the hub refuses) and rewrites cwd and MCP servers, but it removes `_meta` instead of refusing it and passes other methods and the agent's requests (6.1, 6.2). A token cannot reach another user's backend, mint assertions, or learn the backend's dashboard password.
- What a device token still allows on the gateway leg: running commands in the sessions bound to that device, and the calls of the 5.4 allowlist, which act on those sessions or only read. Some of them read beyond the device's own sessions: `session.list`, `session.active_list` and `session.most_recent` are not filtered by device, so they show the titles and other list fields of all of the user's sessions. This is intended: they are the user's own sessions, shown to the user's own devices, and the splice uses `session.list` to find a session by title or the latest one (5.3). Every call that names a session must name one of the device's sessions. Sessions that are bound to no device are refused on the gateway leg (`hermes_device_not_bound`) and on the ACP leg (`session is not bound to this device`). A device can still bind a never-bound session to itself with the PUT route (7.4), then use it. A compromised device can also lie about command output.
- A device token can bind only sessions that resolve to its device or were never bound in their lineage (7.4, and the plugin's own rules in section 9). It cannot take a session of another device's lineage (`409` `bound_to_other_device`), or one whose binding was removed (`409` `was_bound`). It can still claim any never-bound session of the user, such as a web chat, and then run commands in it on itself. When a create's bind fails, the hub deletes the new session only when it is never bound, so a create cannot delete a session that is or was bound.
- A compromised Hermes pod can do anything the user's Hermes can, including running commands on connected devices in their roots. It cannot mint hub assertions, because it holds only public keys.
- The hub's private key is the root of trust for every backend. Keep it in the hub's secret store only.
- With `fallback: local`, the commands of never-bound sessions run in the gateway container and can read its secrets, environment, state database and processes. `fallback: device` (8.10) moves them to a sandbox pod that holds only its own device token and the `/work` volume. The sandbox's commands run as the same user as its daemon, so they can read its token file and open an exec leg of their own, which replaces the daemon's connection (`4409`). That token is a service device token, which the hub accepts on the exec leg only, so it reaches no gateway or ACP leg. `run` removes `HERMES_DEVICE_HUB_TOKEN` from the environment of the commands it runs, but they can still read it from `/proc` of the daemon, so an environment variable or a flag is no better hidden than the file. All of one person's sessions share the `/work` volume, so one session can read another's directory. Limit the sandbox's network to the internet and the hub.

## 11. Versions

The device client, the plugin and acp-host come from one hermes-device release. Release 0.3.0 speaks exec leg protocol versions 2 and 3 and negotiates the newest both sides speak (section 4.1), so a 0.3.0 device works with a 0.2.0 plugin and the other way round, without resume. Upgrade the plugin first: devices then get resume as soon as they upgrade. A side that shares no version with its peer closes with `4426` and a reason that names both versions. The ACP and gateway legs did not change. The local Hermes that `chat` runs must be the same release as the server's Hermes (`chat` checks `hermes_version`).

| Peers | Exec leg |
|---|---|
| device 0.3.0, plugin 0.3.0 | version 3, with resume |
| device 0.3.0, plugin 0.2.0 | version 2 (the device's `hello.version` is 2) |
| device 0.2.0, plugin 0.3.0 | version 2 (`hello` has no `max_version`) |

Browser control needs a device and a plugin that both know the `browser` capability. With an older peer on either side the capability is not negotiated: the leg works as before, and the plugin's browser tools report that the device does not offer browser control.

hermes-hub v0.5.0 refuses a `session.resume` by title or `latest` or with an empty `session_id` (5.4). The Ink TUI never sends those: it resumes by `session_id`, which may hold a title (5.3). With a device client of release 0.4.1 or older, `hermes-device chat -- -c` and `--resume <title>` fail with any hub, because the local Hermes launcher resolves them in its own session store (5.2), and a title sent as `session_id` fails the splice's bind. Later releases look the session up on the server and resume it by id (5.2, 5.3). The acp-host of those releases applies the hub v0.5.0 slash rules to prompts (6.2).

Plugin 0.4.1 and the acp-host of the same release add refusals and change no wire format. The plugin refuses a device bind of a session of another device's lineage or of a fail-closed lineage (section 9), and `/device` can move a session between devices (8.8). acp-host pins ACP params, removes `_meta`, refuses sessions that are not bound to the device itself, and applies the slash-command allowlist to prompts (6.1, 6.2). Editors and the bridge need no change: the bridge binds every session before it uses it.
