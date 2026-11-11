# talaria protocol

Status: phase 3, exec leg protocol version 3 (version 2 peers still served). Pinned to Hermes Agent v0.22.7.

This document defines every message and endpoint between the device client (`talaria`), hermes-hub and the Hermes side (the device plugin in the gateway process, and `acp-host` in the Hermes pod). The hub implements its side from this document. Where this document and code disagree, fix the code or this document in the same change.

The words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

Shared wire names live in Go in package `protocol` and the hub assertion in package `assertion`. Both are public packages of this module, so hermes-hub can import them. The Python plugin repeats the same names in `plugin/device/core.py` and `plugin/device/assertion.py`, and `protocol/contract_test.go` checks that the two agree.

## 1. Parts and legs

```text
device (this PC)                          hermes-hub                         user's Hermes pod
----------------                          ----------                         -----------------
talaria run  ==exec leg==========>  /api/device/exec    ==assertion==>  gateway :9120 /api/plugins/device/ws (plugin)
  loopback splice  ==gateway leg=======>  /api/device/gateway ==ticket=====>  gateway :9120 /api/ws (canonical)
    ^ hermes (Ink TUI), started by talaria chat
talaria acp  ==ACP leg===========>  /api/device/acp     ==assertion==>  acp-host :9130 /acp -> hermes acp (stdio)
talaria login, run, chat, acp ===>  /api/device/pair, /pair/token, /whoami, /sessions/{id}/binding (HTTPS)
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
- The hub answers `401` with `{"error":"unauthorized"}` (HTTP), or refuses the upgrade with HTTP `401`, when the token is unknown or revoked, when the device's user may no longer use the pinned backend, or when the backend no longer exists or lost `devices` in the hub's configuration. The client then stops and tells the user to run `talaria login`. When the hub cannot check the token or the access (a store or lookup error), it answers `503` with `{"error":"internal_error"}` instead, and the client reconnects with backoff.
- The client keeps the token in the OS keyring (service `talaria`, account = the hub URL's host) when one is available, else in a file with mode `0600` (default `<user config dir>/talaria/token`).
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

Protocol version: <!-- docgen:const pkg=protocol name=Version code=false -->3<!-- /docgen:const -->. The oldest version this release still speaks: <!-- docgen:const pkg=protocol name=MinVersion code=false -->2<!-- /docgen:const -->. Version 3 adds resume (section 4.7) and the output `offset`; everything else is as in version 2. Capabilities (section 4.1) add optional features without a version bump: browser control (section 4.8) and MCP servers on the device (section 4.9).

### 4.1 Handshake

1. The plugin verifies the assertion before it reads any message. On failure it closes with `4403`.
2. The device sends `hello` within <!-- docgen:const pkg=protocol name=HelloTimeout print=config -->`10s`<!-- /docgen:const --> of the upgrade. Otherwise the plugin closes with `1002`.
3. Version negotiation. `hello.version` is the oldest version the device speaks and `hello.max_version` the newest (absent means equal to `version`). The plugin picks the newest version that both sides speak. A version 2 plugin reads only `version`, so a device that speaks 2 to 3 sends `"version": 2, "max_version": 3` and a version 2 plugin serves it as version 2. When no version is common, the plugin closes with `4426` and a reason that names both ranges and both releases, for example `talaria v0.4.0 speaks protocol 4 to 5; plugin 0.3.0 speaks 2 to 3; upgrade them together`. The hub relays the code and the reason (it cuts reasons to 120 bytes; the plugin keeps them shorter), and the client stops and shows the reason. A device that gets a `welcome.version` outside its own range closes with `4426` and a reason of the same form, and stops.
4. The plugin answers `welcome` with the chosen version. On version 3 it sends `resume` (section 4.7) right after `welcome`, before any other message. From then on either side may send.
5. Capabilities. `hello.capabilities` lists the optional features the device serves: `browser` (<!-- docgen:const pkg=protocol name=CapabilityBrowser code=false -->browser<!-- /docgen:const -->, section 4.8) and `mcp` (<!-- docgen:const pkg=protocol name=CapabilityMCP code=false -->mcp<!-- /docgen:const -->, section 4.9). The plugin puts the ones it accepts in `welcome.capabilities` and leaves the field out when it accepts none. Each side uses a feature only when `welcome.capabilities` names it, and ignores unknown names. A plugin that does not know capabilities ignores the field and sends none back, so the device does not use them.
6. If a connection for the same `device_id` exists, the plugin closes it with `4409`. Newest wins. On version 3 its pending commands move to the new connection and are listed in the `resume`; on version 2 they fail with exit 255.

`hello` (device to plugin):

```json
{
  "type": "hello",
  "version": 2,
  "max_version": 3,
  "roots": ["/home/alice/src"],
  "tmp_dir": "/home/alice/.cache/talaria/tmp",
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
| `mcp_servers` | With `mcp` in `capabilities`: the names of the MCP servers in the device config (section 4.9). Absent otherwise |
| `hostname`, `os`, `arch`, `shell`, `client_version` | For display and diagnostics. `os` and `arch` use Go's `GOOS` and `GOARCH` names. `os` MUST NOT be `windows`: the plugin closes with `4427` |

`welcome` (plugin to device):

```json
{"type": "welcome", "version": 3, "device_id": "d_8f2c", "name": "desktop", "ping_interval": 25,
 "plugin_version": "0.3.0", "hermes_version": "v0.22.7", "grace_seconds": 120}
```

`device_id` and `name` come from the assertion. `version` is the chosen version. `grace_seconds` (version 3 only) is the plugin's resume grace (section 4.7). `capabilities`, when present, lists the device's capabilities the plugin accepts (step 5), for example `["browser"]`. `client_version` in `hello` and `plugin_version` and `hermes_version` in `welcome` carry the release versions both ways, for display and for the `4426` reasons.

`hermes_image`, when present, is the container image the server's Hermes runs, such as `registry.example.com/hermes-agent:v0.23.1@sha256:<64 hex>`, at most <!-- docgen:const pkg=protocol name=MaxHermesImage code=false -->512<!-- /docgen:const --> bytes. The plugin builds it from the gateway's environment: `HERMES_IMAGE_REPOSITORY`, then `:` and `HERMES_IMAGE_TAG`, or `@` and the tag when the tag is only a `sha256:` digest. The tag may carry the digest (`v0.23.1@sha256:<hex>`). The plugin leaves the field out when either variable is unset. `hermes_base_image`, when present, is the upstream image that `hermes_image` builds on, from `HERMES_BASE_IMAGE`, at most the same length and pinned by digest (the plugin leaves out a value without `@sha256:`). A device that cannot fetch `hermes_image`, for example a private patched image, fetches and runs this one's build instead (5.7). Set both from the same values as the pod's image, so the reference names exactly what runs. The hub relays the welcome unchanged. `chat` on the device runs the same build when the reference has a digest (section 5.7); without one it uses its configured launcher.

Dev mode only (section 8.9): with no hub in between, `hello` also carries `"name"` and `"token"`, there is no assertion, and the plugin checks the token against its static device list. `device_id` is then the configured name.

### 4.2 exec (plugin to device)

```json
{"type": "exec", "id": "4b1e...", "cmd": "source /home/alice/.cache/talaria/tmp/hermes-snap-...; ls",
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
| `talaria run` | The daemon kills its commands when it stops. The plugin's `resume` to the new daemon names them, the daemon answers exit 255 for each, and the tool calls end at once |

### 4.8 Browser control (capability `browser`)

On a leg where `welcome.capabilities` names `browser`, more message types run on the exec leg. docs/browser.md section 4 defines their fields, ops and codes.

| Type | Direction | Meaning |
|---|---|---|
| `browser` | plugin to device | Run one browser operation: `{"type", "id", "op", "args", "timeout"}`. `id` is unique per connection; `kill` with the same `id` cancels it. `op` is one of `tabs_list`, `tab_open`, `tab_close`, `navigate`, `back`, `read_text`, `snapshot`, `inspect`, `click`, `type`, `press`, `hover`, `scroll`, `wait`, `eval`, `screenshot`, `login`, `check` |
| `browser_start` | plugin to device | Start Firefox when the extension is not connected, and wait until it is connected: `{"type", "id", "timeout"}`. Answered by one `browser_result` with the same `id` (`result`: `{"started"}`); `kill` cancels it. The hub forwards it like any other message (docs/browser.md section 5.1) |
| `browser_result` | device to plugin | Exactly one answer per `browser`: `{"type", "id", "ok", "result"}` or `{"type", "id", "ok": false, "code", "error"}`, `code` one of `not_connected`, `refused`, `timeout`, `cancelled`, `failed` |
| `browser_status` | device to plugin | Whether the browser extension is connected: `{"type", "connected", "browser", "extension_version", "can_start"}` (`can_start`: the device answers `browser_start`). Sent after the `welcome` (and the `resume`) and on every change |

The browser is authorized by the device, as commands are: the extension talks only to the local daemon, the daemon's socket is private to its OS user, and the daemon is paired to its owner's hub account. The plugin sends browser messages only on the exec leg of the session's browser device, which must belong to the session's hub user (docs/browser.md section 3.2), so they reach only devices that user owns. docs/browser.md section 13 describes the trust model. Devices from v0.7.0 to v0.15.0 also sent `browser_auth` and refused browser requests until the hub answered it; the current hub does not answer it, so update those devices.

Browser requests are never resumed: a drop cancels them on the device, and the plugin fails them with `not_connected` at once. A device without the capability never gets `browser`; a plugin that did not accept it never gets `browser_status`.

### 4.9 MCP servers (capability `mcp`)

A device may run MCP servers that its own config lists (`mcp.servers`, README "MCP servers"), so that the agent can use tools that must run on the device, such as a reverse engineering server that reads local binaries and drives local programs. The device is the MCP client: it runs each server as a child process and speaks MCP over the server's stdin and stdout (newline-delimited JSON-RPC 2.0, the MCP stdio transport, revision `2025-06-18`). The exec leg carries one request and one answer per tool operation, not the MCP messages themselves, so the plugin and the hub never speak MCP and the device uses only the tools part of it.

The device offers `mcp` in `hello.capabilities` when its config lists at least one server, and lists their names in `hello.mcp_servers`. A name is 1 to 32 characters of `[a-z0-9_-]` that starts with a letter or digit. The plugin accepts the capability only when at least one name is valid, and keeps only the valid names. Changing the list needs a restart of `run`.

| Type | Direction | Meaning |
|---|---|---|
| `mcp` | plugin to device | Run one operation: `{"type", "id", "op", "server", "tool", "arguments", "timeout"}`. `id` is unique per connection; `kill` with the same `id` cancels it |
| `mcp_result` | device to plugin | Exactly one answer per `mcp`: `{"type", "id", "ok": true, "result"}` or `{"type", "id", "ok": false, "code", "error"}`. The device writes `<`, `>` and `&` unescaped and refuses a message over 16 MiB plus 64 KiB with `too_large` |

```json
{"type": "mcp", "id": "9c1f...", "op": "call", "server": "rea", "tool": "list_strings", "arguments": {"limit": 50}, "timeout": 600}
{"type": "mcp_result", "id": "9c1f...", "ok": true, "result": {"content": [{"type": "text", "text": "..."}]}}
{"type": "mcp_result", "id": "9c1f...", "ok": false, "code": "unknown_server", "error": "no MCP server named \"ghidra\" in this device's config"}
```

| `op` | Fields | `result` |
|---|---|---|
| `list` | none | `{"servers": [{"name", "running", "server", "last_error", "retry_in"}]}`: every configured server, whether it runs, its `serverInfo` name and version, the reason it last failed, and the seconds before it may start again |
| `tools` | `server` | `{"tools": [...]}`: the server's `tools/list`, every page joined, each tool as the server sent it. The device keeps the list until the server sends `notifications/tools/list_changed` or exits |
| `call` | `server`, `tool` (1 to 128 bytes), `arguments` (a JSON object; absent or `null` means `{}`) | The server's `CallToolResult` unchanged. A tool error is a result with `isError: true`, not a failed `mcp_result` |

`timeout` is in seconds. Without one the device uses <!-- docgen:const pkg=protocol name=MCPDefaultTimeout print=config -->`2m0s`<!-- /docgen:const -->, and it caps every request at <!-- docgen:const pkg=protocol name=MCPMaxTimeout print=config -->`1h0m0s`<!-- /docgen:const -->. When a request ends by its timeout or a `kill`, the device sends the server `notifications/cancelled` for it and answers `timeout` or `cancelled`; the server keeps running.

| `code` | Meaning |
|---|---|
| <!-- docgen:const pkg=protocol name=MCPUnknownServer code=false -->unknown_server<!-- /docgen:const --> | The device config lists no server of that name |
| <!-- docgen:const pkg=protocol name=MCPUnavailable code=false -->unavailable<!-- /docgen:const --> | The server did not start, its initialize failed, it is waiting before it may start again (below), or the daemon is stopping |
| <!-- docgen:const pkg=protocol name=MCPBusy code=false -->busy<!-- /docgen:const --> | The server already has 8 requests in flight, or the device has 16 on all its servers together |
| <!-- docgen:const pkg=protocol name=MCPTooLarge code=false -->too_large<!-- /docgen:const --> | The server's answer, or its whole tool list, is over <!-- docgen:const pkg=protocol name=MaxMCPResult code=false -->16777216<!-- /docgen:const --> bytes |
| <!-- docgen:const pkg=protocol name=MCPTimeout code=false -->timeout<!-- /docgen:const -->, <!-- docgen:const pkg=protocol name=MCPCancelled code=false -->cancelled<!-- /docgen:const --> | The request's timeout passed, or a `kill` or a dropped leg ended it |
| <!-- docgen:const pkg=protocol name=MCPFailed code=false -->failed<!-- /docgen:const --> | The server answered with a JSON-RPC error, or exited during the request |
| <!-- docgen:const pkg=protocol name=MCPRefused code=false -->refused<!-- /docgen:const --> | The request is not valid (unknown `op`, a tool name that is empty or too long, `arguments` that are not an object, a message over <!-- docgen:const pkg=protocol name=MaxMCPRequest code=false -->4194304<!-- /docgen:const --> bytes, an `id` already in flight), or the leg did not negotiate `mcp` |

The plugin also uses `not_connected` for a request it could not send, or one whose device disconnected before the answer.

Server lifecycle on the device:

- The daemon starts nothing at startup. A server starts on the first `tools` or `call` for it: the device runs `command` with `args`, in `dir` (default `$HOME`), in its own process group, with the daemon's environment without `TALARIA_HUB_TOKEN`, plus the server's `env`. It sends `initialize` (no client capabilities) and `notifications/initialized`, and waits up to 120 seconds for the answer.
- The server's stderr goes to the daemon log, one line per record, cut to 2000 bytes, at most 50 lines a second; the device logs how many lines it left out. Its stdout must carry only JSON-RPC. A line over 16 MiB is not read. When the answer's `id` is its first member after `jsonrpc` or its last member (the TypeScript MCP SDK writes `{"result", "jsonrpc", "id"}`), that request fails with `too_large` and the server keeps running; otherwise the device kills the server.
- The device answers the server's `ping` requests and refuses every other server request (`roots/list`, `sampling/createMessage`, `elicitation/create`) with `-32601`. One goroutine writes to the server's stdin, with at most 64 messages waiting. A server that lets more pile up (it sends requests and does not read the answers) or does not take a write within 30 seconds is killed.
- The start runs on its own: a request whose timeout passes while the server starts fails with `timeout`, and the start goes on for the next request.
- A server with no request in flight stops after its own `mcp.servers.<name>.idle-timeout`, or else after `mcp.idle-timeout` (default 1 hour). `0` keeps it running until `run` stops. It loses its state, such as opened binaries; the next request starts it again.
- A server that exits on its own fails its requests in flight with `failed`. The next request starts it again, after a wait: 1 second after the first failure, doubling up to 60 seconds while it keeps failing (a start that fails, or an exit within 30 seconds of the start); a server that ran 30 seconds or longer gets the 1 second wait again. Meanwhile requests fail with `unavailable` and the server's last stderr line.
- To stop a server, the device closes its stdin, then sends SIGTERM and SIGKILL to its process group, 3 seconds apart. `run` stops every server before it exits. When a server's main process exits, the device sends SIGKILL to the rest of its process group, and then no further signal, since the group id may be reused. A server is not stopped if `run` itself is killed with SIGKILL; under systemd, the unit's cgroup ends it.

MCP requests are never resumed: a dropped leg cancels the requests in flight on the device (it sends `notifications/cancelled`), and the plugin fails them with `not_connected` at once. The servers keep running across a drop and a reconnect. A device without the capability never gets `mcp`; it answers one with `refused` and starts no server. A plugin that did not accept the capability ignores `mcp_result`.

The hub copies `mcp` and `mcp_result` unchanged, as every exec leg message. Their sizes are within its 64 MiB message limit. The plugin decides which session may use which device's servers (section 8.11), and the device decides which servers can run.

## 5. Gateway leg and the chat splice

### 5.1 Legs

- Device to hub: `GET wss://<hub>/api/device/gateway` with the device token.
- Hub to backend: the hub opens the gateway's canonical session socket for the device's user, exactly as for its web UI (`/api/ws` on 9120, subprotocols `hermes-gateway-v1` and `hermes-gateway-ticket.<ticket>`).
- The hub parses every message of the device and applies the rules of 5.3 to 5.6, whether or not the device runs the splice. It answers a refused request itself and forwards nothing. It forwards every other device request under its own id `hermes-hub-d<n>` and gives the device's id back on the answer, so the answer matches whatever form the gateway gives the id in. Notifications that pass the rules are forwarded with the same param rewrites as requests (5.4), under no id. The hub also sends its own requests, with ids `hermes-hub-<n>` (5.3), and consumes their answers. It refuses a device request whose id starts with `hermes-hub-` (5.6) and drops any other device message with such an id. It acts on the held answers of 5.3 before the device sees them. Binary messages are dropped.
- A device message without a `method` member passes only as an answer to one of the gateway's requests: its `id` must be a string that starts with `srq-` (gateway/session_server_requests.py `is_response_frame`), and it must have exactly one of `result` and `error`. The hub rebuilds it from `jsonrpc`, `id` and that one member, so the gateway reads no other member, and drops every other method-less message.
- The hub dials the backend as its web UI does, with an `Origin` header and `?native_dial=browser`, because the gateway refuses a ticket dial from a non-loopback peer without an `Origin` (gateway/run_api.py:428-446).
- The hub sends no message of its own to the device except answers to the device's own requests. The first message the device sees is the gateway's `gateway.ready` event, with `session_authority: true`.

### 5.2 Loopback splice

`talaria run` listens on `gateway.listen` (default `127.0.0.1:0`, loopback only). `talaria chat` asks the daemon for a per-run secret (section 5.5) and starts the local Hermes TUI with:

```text
HERMES_TUI_GATEWAY_URL=ws://127.0.0.1:<port>/api/ws?token=<per-run secret>
```

The Ink TUI dials that URL as given, with no subprotocol and no custom headers (ui-tui/src/gatewayClient.ts), so the secret rides in the query. The splice accepts an upgrade only on path `/api/ws`, from a loopback peer, with no `Origin` header, and with a live per-run secret (constant-time compare). Otherwise it answers HTTP `403`. For each accepted TUI socket the splice opens one gateway leg to the hub and copies JSON-RPC messages, applying the rules below to messages from the TUI and to the matching responses.

JSON-RPC ids may be strings (the Ink TUI sends `"r1"`, `"r2"`, ...) or integers. The splice keeps them as raw JSON. Requests the splice sends itself use string ids starting with `talaria-`, and the splice consumes their responses. The splice matches JSON member names exactly (`method`, `params`, `id`), as Hermes does. A notification (no `id`) of a denied or rewritten method is dropped. A rewritten method whose `params` is not an object is answered with `4001` `hermes_device_cwd`.

The splice dials the hub leg before it accepts the TUI's upgrade and answers HTTP `502` when that dial fails. It closes TUI sockets with `1000` when their per-run secret is deleted, `1001` when the daemon stops, `1011` when the hub leg drops, and `4408` on a ping timeout.

`talaria chat` runs `<chat.hermes> --tui` (plus any arguments after `--`) in the current directory with the terminal attached. It leaves SIGINT to the TUI (the terminal sends it to the TUI's process group), forwards SIGTERM and SIGHUP, deletes its per-run secret when the TUI exits, and exits with the TUI's code (`128 + signal` when a signal ended it).

The local Hermes launcher resolves `-c`/`--continue [name]` and `-r`/`--resume <id, title or latest>` in its own local session store, which does not hold the server's sessions, and hands the Ink TUI the result in `HERMES_TUI_RESUME` (hermes_cli/main.py `_resolve_continue_arg`, hermes_cli/main_tui_launch.py); with no local match, bare `-c` exits before the TUI starts. So `chat` takes these flags out of the arguments, up to a `--`, in the forms argparse reads: `-c [NAME]`, `-cNAME`, `-c=NAME`, `--continue[=NAME]` and its prefixes down to `--co`, `-r ID`, `-rID`, `--resume[=]ID` and its prefixes down to `--res`. As in argparse, `-c` takes the next argument as its name unless it starts with `-`; a `--resume` with no value, or with one that starts with `-`, stays for the local Hermes to report. `--resume` wins over `-c`, and an empty value resumes nothing, as in Hermes. `chat` then looks the session up with `POST /control/lookup` (5.5: bare `-c` and `--resume latest` are the latest session, any other value is a name) and starts the TUI with `--resume <id>` first. When the lookup finds no session for a name, `chat` passes the name as `--resume <name>`, and the splice resumes it as sent (5.3), so a session that the lookup cannot find by id (such as a messaging session moved to this device) still resumes. When it finds no latest session, `chat` fails before it starts the TUI. `--create-if-missing` is not supported: it has no effect. Each lookup opens a gateway leg, so it adds a round trip to the start.

### 5.3 Rewritten calls

The device directory of a create is the `cwd` the TUI sends, which is the directory `talaria chat` ran in. It is a device path.

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

Resuming in `talaria chat` means "continue this session here". The hub resumes only by an explicit `session_id`, so the splice first finds the session id:

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

The hub lets through only an allowlist (`gatewayRules` in hermes-hub internal/device/rpc.go): the methods the Hermes v0.22.7 Ink TUI and classic CLI send for chatting. It refuses every other method, and the requests below, with error code `-32601`, reason `hermes_device_denied` and message `<method> is not available through talaria: <why>`, and forwards nothing. A refused notification is dropped. The session checks of 5.3 apply to every method that passes.

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

- a request with a `profile` member that is not null, of any type: the gateway would route it to a sibling profile that may not run device sessions. Error `-32601`, message `talaria serves only the device's own Hermes profile`, reason `hermes_device_denied`;
- a request with a `request_id` param that starts with `hermes-hub-`, the prefix of the hub's own requests: error `-32600`.

The slash-command allowlist is `new`, `clear`, `redraw`, `history`, `retry`, `undo`, `title`, `compress`, `stop`, `status`, `context`, `usage`, `help`, `commands`, `version`, `copy`, `queue`, `steer`, `btw`, `branch`, `fast`, `yolo`, `reasoning`, `model`, `goal`, `plan`, `review` and `whoami`: commands that act on the session only, or only read. `verbose` is not in it (it writes the profile's `config.yaml`), nor `diff` (it runs git in the pod) or `refine` (it reviews into the profile's memory and skills). The hub splits a `slash.exec` command as Hermes does (strip, remove leading `/`, name up to the first space, the rest is the argument) and takes `name` and `arg` of `command.dispatch` the same way. Then:

- An argument that Hermes' flag parsers may read as `--global` is refused: the hub drops quotes and backslashes, turns figure, en, em and bar dashes into `--`, lowers the case, and refuses an argument that then contains `--global`.
- `/reasoning` takes no argument, or exactly an effort level or `reset`.
- `/usage` takes no argument: `/usage reset` redeems an account credit.
- `/model` with `--refresh` is refused. Every other `/model` gets `--session` put first (`slash.exec` command `/model --session <rest>`, `command.dispatch` `arg` `--session <arg>`), where no value flag can take it as its value: without `--session`, Hermes writes the pick to the profile's `config.yaml` when the profile has no default model or sets `model.persist_switch_by_default` (hermes_cli/model_switch.py `resolve_persist_behavior`).

Prompt text. `prompt.submit` and `prompt.replace_queued` admit their `text` as a message event, which runs a slash command (gateway/run_inbound.py), and the other text methods hand it to the model; the hub checks the `text` of all five as Hermes reads a command (MessageEvent `get_command`): it strips leading whitespace (Python's `str.isspace`), then every leading line that starts with `@` (a superset of the attachment refs Hermes strips, gateway/platforms/event.py `_ATTACHMENT_REF_RE`), then leading whitespace again. Text that then starts with `/` names a command: the name is the lower-cased word up to the first whitespace, without a `@bot` suffix, and the rest without leading whitespace is the argument. A name that is empty or holds `/` is plain text. A name that is not plain ASCII is refused. An alias counts as its command (`reset` as `new`, `fork` as `branch`, `compact` as `compress`, `q` as `queue`, `s` as `steer`, `ctx` as `context`, `v` as `version`, from hermes_cli/commands.py). The command must pass the slash rules above, and `/model` typed as text is refused, since its value flags can take a `--session` as their value (hermes_cli/model_switch.py `parse_model_flags_detailed`); the TUI sends `/model` through `slash.exec`.

The splice applies a shorter deny list first, so the TUI gets these refusals without a round trip, with the same error code and message `<method> is not available through talaria`: `model.save_key`, `profiles.create`, `shell.exec`, `cli.exec`, `image.attach`, `input.detect_drop`, `clipboard.paste`, `mcp.servers.add`, `mcp.servers.test`, `mcp.servers.remove`, `plugins.manage`, `skills.manage`, `cron.manage`, `cron.submit`, `kanban.run`, `browser.manage`, `display.install`, `display.start`, `a2a.forward`, `process.kill`, `rollback.restore`, `set_api_key`, `diagnostics.share_nous`, `free_tier.provision`, `session.foreign.import`, `prompt.background`, `vault.*`, `worker.*`, `connectors.*`, `oauth.*`, a `config.set` that the hub refuses (the same rule), `pdf.attach` and `file.attach` with a non-null `path`, and `session.mutate` with `operation` `editor_mcp`. It answers `process.list` itself with `{"processes": []}` and `wake.start` with `{"started": false, "reason": "unavailable"}` and a hint: the hub refuses both, and the Hermes TUI shows those refusals as errors (a `-32601` as a version mismatch between the TUI and the backend). Everything else is left to the hub. The plugin guards what the allowed calls reach (section 8.3).

The hub enforces these rules on the gateway leg, so a holder of the device token who dials the gateway leg without the splice meets them too (section 10).

Attachments from the TUI. The v0.22.7 Ink TUI on a remote gateway sends a pasted or dropped image as `image.attach_bytes` (the gateway stages it in the profile's `cache/images`) and names the staged path in the `attachments` of the next `prompt.submit` (ui-tui/src/lib/imageAttachments.ts `stageImagePath`); both pass. A dropped file that is not an image stays a path in the prompt text. That path is a path on the device, which is where the session's file tools look, so no upload is needed. The TUI never sends `file.attach`, and the gateway does not serve `pdf.attach` on this connection.

### 5.5 Daemon control

`talaria run` writes `<state dir>/daemon.json` (mode `0600`, directory `0700`):

```json
{"pid": 4242, "splice": "127.0.0.1:41733", "control_secret": "<43 base64url chars>", "device_id": "d_8f2c", "hub": "https://hub.example.com",
 "hermes_version": "v0.23.1", "hermes_image": "registry.example.com/hermes-agent:v0.23.1@sha256:<64 hex>"}
```

`hermes_version`, `hermes_image` and `hermes_base_image` come from the latest exec leg welcome (section 4.1) and are absent until the first one, or when the plugin does not report them. `chat` uses `hermes_image` for the managed Hermes (5.7).

`talaria chat` reads it and calls, on the splice address:

- `POST /control/tokens` with `Authorization: Bearer <control_secret>` and `{"cwd": "<absolute device dir chat runs in>"}`: answers `201` with `{"token": "<43 base64url chars>"}`, a new per-run secret tied to that directory (used for `session.resume`, 5.3). A missing or relative `cwd` gets `400`; a daemon that is stopping answers `503`.
- `DELETE /control/tokens/<token>` with the same header when the TUI exits: `204`. Open TUI sockets that used it are closed.
- `POST /control/lookup` with the same header and `{"session": "<id or title>"}` or `{"latest": true}`: the daemon opens a gateway leg of its own, looks the session up as for a resume (5.3, at most 2 minutes), and closes the leg. It binds and attaches nothing. Answers `200` `{"session_id": "<id>"}`, `404` `{"error": "not_found"}`, `502` `{"error": <hub error code, else hub_unreachable>, "message": <text>}` when the lookup fails, and `400` for a body that is not JSON or has neither or both of `session` (non-empty) and `latest` (true). Other members are ignored.

`talaria setup` calls, with the same header:

- `GET /control/browser`: answers `200` with `{"enabled", "connected", "version", "browser"}`. `enabled` is false, and the other members are empty, when `browser.enabled` is off. `connected` is true while the Firefox extension is connected to the daemon, with its `version` and `browser`. `talaria setup` uses it to wait for the extension.

The daemon refuses the control endpoints from non-loopback peers and with a wrong secret (`403`). It removes `daemon.json` on a clean exit.

### 5.6 Errors to the TUI

The splice and the hub answer a request they refuse or fail with `{"jsonrpc": "2.0", "id": <same id>, "error": {"code": <code>, "message": <text>, "data": {"reason": <reason>}}}`:

| Case | `code` | `data.reason` |
|---|---|---|
| Denied method or params (5.4). The hub's message adds the reason: `<method> is not available through talaria: <why>` | `-32601` | `hermes_device_denied` |
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

### 5.7 Managed Hermes

On Linux with `chat.managed` (the default), `chat` runs the Hermes build that `welcome.hermes_image` names (section 4.1), so the TUI and the server are the same build.

1. `run` writes the `hermes_version` and `hermes_image` of each welcome into `daemon.json` (5.5), and ignores an image longer than the limit of section 4.1. When the image is pinned by digest, it fetches the build in the background unless it is already on disk, and retries with backoff (1 minute, doubling, at most 1 hour) until it is. When that fetch fails and the welcome has `hermes_base_image`, it fetches that build instead. A welcome with the same image does not cut the backoff short. It then removes the builds other than the current one and the most recently used other one, skipping builds in use.
2. A fetch pulls the image by digest only: a reference without `@sha256:` is refused. It takes an anonymous token when the registry answers with a `Bearer` challenge, picks `linux/<GOARCH>` from an image index, and checks the index, the manifest, the config and every layer against their digests and sizes while they stream. Layers must be gzip or plain tar; whiteouts apply as in the OCI image spec. Only `opt/hermes` and `usr/local/bin` of the image are kept, without the browser and ffmpeg in `opt/hermes/tools`; a hard link to a file outside them is skipped. No entry may land outside the build directory: paths with `..`, whiteouts of `.` or `..`, and paths that pass through a symlink out of the tree fail the pull. A request fails when no data arrives for 2 minutes; there is no limit on the total time. When the registry fails before the layers and the repository path starts with a well-known public registry (`<mirror>/docker.io/<repository>`; also `ghcr.io`, `quay.io`, `gcr.io`, `registry.k8s.io`, `public.ecr.aws`, `mcr.microsoft.com`, `registry.gitlab.com` and `nvcr.io`), the fetch pulls the same digest from that registry instead.
3. A verified tree is written to disk (sync) and then moved into `builds/<digest>` of the managed directory. Absolute symlinks become relative ones inside the tree, and an absolute `home` in a `pyvenv.cfg` moves under the tree, so the image's Python and Node run from there.
4. `chat` reads `hermes_image` and `hermes_base_image` from `daemon.json`. It runs the build of `hermes_image`, or of `hermes_base_image` when only that one is on the device or `hermes_image` cannot be fetched. It fetches the build when it is missing, and holds a shared lock on `locks/<digest>.lock` (also passed to the TUI process) while the TUI runs. Prune and refresh take that lock exclusively and skip a build they cannot lock. It starts `<build>/opt/hermes/.venv/bin/hermes --tui` with `HOME`, `HERMES_HOME`, `XDG_*_HOME` and `HERMES_MANAGED_DIR` in the managed home, the image's `HERMES_*`, `PYTHON*` and `PLAYWRIGHT_*` variables with their paths moved into the tree, `HERMES_NODE` set to the bundled Node, a `PATH` that starts with the build's `usr/local/bin` and venv `bin` and leaves out the user's directories under `HOME`, and none of the user's variables that point at other Python, Node or Hermes installs or libraries. It does not compare versions.
5. Without an image in `daemon.json`, with an image that is not pinned by digest or that has no build for `linux/<GOARCH>`, with `chat.managed` false or on macOS, `chat` runs `chat.hermes` after the version check of section 7.3. `run` does not retry such an image.

## 6. ACP leg

### 6.1 Legs

- Editor to `talaria acp`: ACP over stdio, newline-delimited JSON-RPC 2.0.
- Device to hub: `GET wss://<hub>/api/device/acp` with the device token. One WebSocket message per JSON-RPC message.
- Hub to acp-host: `GET ws://<pod>:9130/acp` (<!-- docgen:const pkg=protocol name=ACPHostPath -->`/acp`<!-- /docgen:const -->) with the assertion (`leg` `acp`). Only the hub can reach this port.
- acp-host verifies the assertion, starts one `hermes acp` process for the connection (in the pod, in its own process group, with acp-host's environment and so the pod's `HERMES_HOME`), writes each message to its stdin as one line (re-encoded, so a message never holds a raw newline), and sends each stdout line that is JSON as one message. Other stdout lines are dropped and logged. Its stderr goes to acp-host's log, one line per entry, tagged with the connection. When the WebSocket closes, acp-host closes the child's stdin, sends SIGTERM to the process group after 5 seconds and SIGKILL after 10. When the child exits on its own, acp-host closes the WebSocket with `1011`. At `acp-host.max-processes` children, new connections are closed with `1013`. WebSocket messages over 48 MiB close the connection (`1009`), because `hermes acp` reads stdin with a 50 MiB line limit and stops reading on a longer line.
- A refused assertion is answered by accepting the upgrade and closing at once with `4403`, so the hub can relay the code. `GET /healthz` answers `200`.

`hermes acp` attaches to the running gateway in the same pod (acp_adapter/gateway_server.py), so the sessions live in the gateway, and a restarted gateway is reattached by the same `hermes acp` process. acp-host runs in its own container, so its children outlive gateway restarts.

When no gateway answers, `hermes acp` starts one itself (hermes_cli/gateway_client.py `connect_gateway` calls `ensure_gateway_runtime`, which calls `spawn_unmanaged_gateway` with `start_new_session=True`). Hermes has no setting that turns this off. In the acp-host container that would be a second gateway with no device connections. So acp-host:

- starts a child only while the gateway answers `2xx` on `acp-host.gateway-health-url` (default `http://127.0.0.1:9120/api/health`, a public route), and otherwise closes the connection with `1013`;
- makes itself a child subreaper, and every 2 seconds stops (SIGTERM, then SIGKILL 5 seconds later) every process that left a child's process group, and every orphan reparented to it, then reaps it. A gateway that a child starts during a gateway restart is stopped this way.

acp-host also refuses, with JSON-RPC error `-32602`, any editor request or notification whose `params.sessionId` (or `params.session_id`) is not bound in `bindings.json` to the assertion's `device_id`: message `session is bound to another device` when the session's binding names another device, and `session is not bound to this device` when the session has no binding of its own. Only the session's own entry counts, because acp-host cannot read the lineage: a compression child or branch of a session bound to another device has no entry, so it is refused too. The bridge binds every session it creates, and every session it opens from another place once the user asks for the move (6.3), and the plugin refuses that bind for a session of another device's lineage (section 9 PUT rules), so editor sessions that run have an entry. acp-host checks the params after the pinning of 6.2, so it sees the id Hermes will use. When `bindings.json` cannot be read or parsed, these calls get `-32603`.

The hub reads every message on the ACP leg too (`acp.go` in hermes-hub), so acp-host's checks are not the only ones:

- A device may send only `initialize`, `authenticate`, `session/new`, `session/list`, `session/load`, `session/resume`, `session/fork`, `session/prompt`, `session/set_mode`, `session/set_model` and `session/set_config_option` as requests, and `session/cancel` as a notification: the methods of the ACP SDK that Hermes v0.22.7's gateway ACP agent implements (acp_adapter/gateway_server.py). Any other request gets `-32601` with reason `hermes_device_denied` and message `<method> is not available through hermes-hub`; any other notification is dropped. `session/close` is refused this way, so the bridge answers it itself (6.3).
- The hub pins the params per method: they may have only the members the ACP SDK (v0.9.0, acp/schema.py) defines for that method, under the alias or the field name (the members kept in the 6.2 table, plus `sessionId` and `session_id` for the methods with a session). Any other member is refused with `-32602`, and so is a `field_meta` member at any depth. A top-level `_meta` is refused in this way: the SDK merges it into the handler's arguments, where it would replace the session id or cwd the hub checked. (acp-host removes it instead, 6.2, and the bridge removes it before it reaches the hub, 6.3.) Params that are not an object get `-32602`.
- Every method except `initialize`, `authenticate`, `session/new` and `session/list` must name its session in `sessionId` or `session_id` (both are checked when both are sent, each a non-empty string). The session must be bound to this device, through its lineage, or have been created by `session/new` or `session/fork` on this leg and not be bound to another device since. Otherwise the answer is `-32602` with reason `hermes_device_other_device` or `hermes_device_not_bound`, or `-32603` with reason `hermes_device_bind` when the binding cannot be read.
- As acp-host does (6.2), the hub sets `cwd` to the plugin's placeholder, sets `mcpServers` to `[]` and removes `mcp_servers` on `session/new`, `session/load`, `session/resume` and `session/fork`, replaces `resource_link` prompt blocks with a text note, and keeps only the sessions bound to this device in `session/list` answers (no rows when the bindings cannot be read). It refuses a `session/prompt` with any `text` member, at any depth, that is a slash command the gateway leg's prompt text rule refuses (5.4) and is not `/discard` or `/tools` (6.2), with `-32602` and reason `hermes_device_denied`: `hermes acp` sends those to the gateway as `slash.exec` over its own connection, which the hub does not see.
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

The session check of 6.1 then runs on the pinned params. acp-host removes `_meta` instead of refusing the message; the hub in front of it refuses a top-level `_meta` and a `field_meta` at any depth (6.1), so the bridge removes a top-level `_meta` before it forwards a message (6.3). A `_meta` inside a member, such as a content block, passes. The bridge does not remove `field_meta`: ACP clients send the alias `_meta`, so only a client that sends field names would send it, and the hub refuses it.

| Method | Rewrite |
|---|---|
| `session/new`, `session/load`, `session/resume`, `session/fork` | `params.cwd` becomes the placeholder (`acp-host.placeholder`). `params.mcpServers` becomes `[]`: editor MCP servers would run as programs in the pod. The same values on every call keep `session/load` from triggering a workspace move or an MCP rebind |
| `session/list` | `params.cwd`, when present, must be absolute and is removed: TUI sessions keep their device dir as row cwd, so a placeholder filter would hide them (acp_adapter/catalog.py). The request gets an id of acp-host's own, `hermes-device-list-<n>`, and the response gets the editor's id back, so the response is matched whatever id form the editor used (Python re-encodes ids, and treats `"id": null` as a request). The result is filtered (below) |
| `session/load`, `session/resume` | Besides the rewrite above, the request gets an id of acp-host's own, `hermes-device-load-<n>`, and the response gets the editor's id back, as for `session/list`. When the result is an object and `bindings.json` binds the session to the assertion's `device_id`, acp-host adds `"_meta": {"deviceBinding": {"dir": <binding dir>}}` to it (<!-- docgen:const pkg=protocol name=ACPBindingMeta -->`deviceBinding`<!-- /docgen:const -->), and keeps the other `_meta` members Hermes sent. The bridge reads it to tell whether the editor's folder is the session's folder (6.3) |
| `session/prompt` | Every content block of type `resource_link` is replaced by `{"type": "text", "text": "[talaria: link <uri> removed; the bridge inlines device files]"}`. `hermes acp` reads a link as a pod path whatever its form: `file://` in any case, a bare path, or a drive path (acp_adapter/content.py `_path_from_file_uri`). Then a prompt that would run a slash command the rules below refuse is refused (`-32602`, message `/<name> is not available through talaria`) |

Slash commands in prompts: `hermes acp` runs a prompt whose text starts with a registered command as that command, and sends every command it does not answer itself to the gateway's `slash.exec` over its own connection (acp_adapter/gateway_server.py `_slash_command` and `_run_slash`), which the hub does not see. So acp-host applies the gateway leg's prompt text rule (5.4) to prompts, as the hub does on the ACP leg: it joins the string `text` members of the prompt's blocks with newlines (after the link rewrite), strips leading whitespace as Python does, then every leading line that starts with `@`, then leading whitespace again, and when the result starts with `/`, takes the word up to the first whitespace and the rest, stripped, as the argument. The name is the word lower-cased, without a `@bot` suffix. A name that is empty or holds `/` is prompt text, as in Hermes. Any other word must be ASCII, and the name, with an alias counted as its command, must be in the 5.4 allowlist without `model`, or be `discard` or `tools` (which `hermes acp` answers for the session itself). The hub's own ACP check (6.1) allows them too, and it checks every `text` member on its own. `/discard <id>` is the only way an editor clears a turn that a gateway restart lost: until then `hermes acp` refuses every other prompt of the session (acp_adapter/gateway_server.py `prompt`). The argument must not read as `--global` in any of the forms of 5.4, `/reasoning` takes no argument, an effort level or `reset`, and `/usage` takes no argument. Hermes builds the text from more blocks, but the others only add text that starts with `[`, so this refuses every command Hermes would run, and a few prompts that Hermes would send as text. `/device` is not in the list: through ACP it would let a device move its session to another device (8.8).

Rewritten messages are decoded and encoded again, so a duplicate key keeps its last value, as in Python. acp-host answers these itself and forwards nothing: a message that is not JSON (`-32700`), JSON that is not one object, such as a batch (`-32600`), and `params` of a pinned method that are not an object, a `prompt` that is not an array, or a relative list `cwd` (`-32602`). A refused notification (no `id` member) gets no answer.

`session/list` results: acp-host reads `$HERMES_HOME/device/bindings.json` (section 8.2; setting `acp-host.bindings-file` overrides the path) read-only, on every list, and keeps two kinds of rows. A row whose session is bound to the assertion's `device_id` (and, when the request named a cwd, whose binding dir is that cwd or below it) gets its binding dir as its `cwd`. When the request named a cwd, a row whose session was never bound (no entry in `bindings` and not in `ever_bound`) gets that cwd as its `cwd`: these are the chats that Hermes's `sessions.acp_list_sources` adds, such as hub, Desktop, TUI and CLI chats that run in the sandbox or on the server. Opening one follows 6.3 point 3: it opens with a notice and runs from the editor only after `/move-here`. Every other row is dropped: rows bound to another device, rows bound to this device outside the cwd, and rows that were bound before and are not now. A missing file counts as no bindings, so it keeps only never-bound rows; a file that cannot be read or parsed keeps no rows (logged). `nextCursor` passes unchanged, so a filtered page can be empty while `nextCursor` is still set.

Everything else passes unchanged in both directions, including the agent's requests to the editor (`session/request_permission`, `elicitation/create`) and the editor's answers to them.

### 6.3 Bridge behavior (`talaria acp`)

The bridge runs on the device, so it can read the editor's files and it knows the editor's real cwd.

1. `session/prompt`: each `resource_link` block with a `file://` URI (host empty or `localhost`) of a regular file up to 1 MiB of valid UTF-8 is replaced by a `resource` block with `{"uri": <uri>, "mimeType": "text/plain", "text": <contents>}`. Other links pass, and acp-host replaces them.
2. `session/new`, `session/fork`, `session/load` and `session/resume` need an absolute `params.cwd`; otherwise the bridge answers `-32602` and forwards nothing. `session/new` and `session/fork`: on a result, bind `result.sessionId` to the request's `cwd` (section 7.4) before passing the result to the editor. If the bind fails, answer the editor's request with error `-32603`, message `talaria: could not bind the session to <cwd>: <hub error>`. (ACP has no delete; the unbound empty session is not listed, because Hermes lists only sessions with messages.)
3. `session/load` and `session/resume`: the bridge does not bind. It forwards the request, so the hub and acp-host check the session's binding (6.1), and answers by where the session runs:
   - Bound to this device, and the binding dir from acp-host (6.2, `_meta.deviceBinding.dir`) is `params.cwd` or its real path (symlinks resolved): the result passes, without the `deviceBinding` member. This is a chat opened in its own folder.
   - Bound to this device and another folder, or the result has no `deviceBinding` (an older acp-host): the result passes the same way, with the replayed history, after an `agent_message_chunk` notice that names the bound folder (when known), says the chat was not moved, and tells the user to send `/move-here` (<!-- docgen:const pkg=internal/acpbridge name=MoveCommand -->`/move-here`<!-- /docgen:const -->) to move it. The session stays bound where it is.
   - Refused as not bound to this device (reason `hermes_device_not_bound`, or acp-host's `session is not bound to this device`): a chat that runs in the sandbox or on the server (a web or Telegram chat, 8.10), or one that was bound before, or one bound only through an earlier part of its lineage. The bridge answers the request itself with `{}`, after an `available_commands_update` that offers `/move-here` and a notice that the chat was not moved. Hermes has not loaded the session, so the editor shows no history yet.
   - Refused as bound to another device (reason `hermes_device_other_device`, or acp-host's `session is bound to another device`): the bridge answers with the same error code and the message `This chat is bound to another computer, so it was not opened here. Use it on that computer, or move it with /device in the chat.` The plugin refuses a device bind of another device's session (section 9 PUT rules), so the editor cannot move it.
   - Any other answer passes unchanged.

   A session opened in another place does not run from the editor until the user moves it. The bridge answers every `session/prompt` of it itself, with a notice that the message was not sent and `{"stopReason": "end_turn"}`, and forwards nothing. A prompt whose text blocks, joined and trimmed, are `/move-here` (any case) binds the session to the editor's `cwd` (section 7.4), with the notice `Could not move this chat to <cwd>: <hub error>` and `end_turn` when the bind fails; the session stays where it was. A session Hermes already loaded is then answered with a "Moved" notice and `end_turn`. A session the bridge answered itself is loaded then (`session/load` with `{"sessionId", "cwd", "mcpServers": []}`), its replayed history reaches the editor, and the prompt is answered after the load with the notice and `end_turn`. If that load fails or the leg drops, the session counts as bound to another folder of this device: a reconnect loads it, and `/move-here` again finishes the move. A `/move-here` sent as a notification does nothing. `/move-here` in a session that is already in the editor's folder gets a notice and `end_turn`. While a session is not moved, the bridge adds `/move-here` to each `available_commands_update` Hermes sends for it; the editor may keep showing it after a move until Hermes sends its next list.

4. Reconnect: when the ACP leg drops, the bridge answers every editor request still waiting with error `-32603`, message `talaria: connection to Hermes lost; retry`, and reconnects with backoff. On the new connection it sends the params of the editor's last successful `initialize` and consumes the response, then, for each session the editor opened (a successful new, fork, load or resume), sends `session/load` with `{"sessionId", "cwd", "mcpServers": []}` and drops every `session/update` for that session until the load's response, which it also consumes. It does not bind: a session bound here or moved by the user loads as before, with no notice, and one in another folder stays as it was (point 3). When the answer's `deviceBinding` names another folder for a session that was in the editor's folder (the chat was moved, for example with `/device`, while the leg was down), the bridge sends the point 3 notice and treats the session as one in another folder. A session that the bridge answered itself and the user did not move is not loaded; it stays open for `/move-here`. Those updates are the replayed history the editor already has. A load that fails is logged and the session is forgotten, and so is a session the editor closed with a successful `session/close`. Editor messages wait until the replay is done, at most 30 seconds; a request still waiting then is answered with the error above.
5. Ids: the bridge sends every editor request to the agent under a string id of its own, `hermes-device-<n>`, and gives the response the editor's id back. Python re-encodes ids (`1e0` comes back as `1.0`, `"\u00e9"` as `"é"`), so this is the only way to match every response. The bridge's own requests use the same form. An editor message over 48 MiB is answered with `-32600`, because acp-host would close the leg.
6. Agent requests (`session/request_permission`, `elicitation/create`) reach the editor with a bridge id `hermes-device-<n>`, and the editor's answer goes back under the agent's id. A new `hermes acp` process numbers its requests from the start again, so this keeps an answer meant for a lost connection from reaching a new request with the same id. Answers to requests of a lost connection are dropped.
7. The bridge stops with a non-zero exit when the hub refuses the token (HTTP `401` on the upgrade or on a bind, or close `4401`) or closes with `4426`. Every other close or dial failure is retried with backoff: a close with `1011` (the `hermes acp` child exited, as when acp-host restarts) or `1001` (hub shutdown), a connection that ends without a close, and an upgrade answered with a 5xx status. Each reconnect replays as in point 4. Stdout carries only JSON-RPC lines; logs go to stderr.
8. `_meta`: the bridge removes a top-level `_meta` member from the params of every editor request and notification before it forwards the message, because the hub refuses it (6.1). Several ACP clients attach `_meta` to every request. Members inside params, and their own `_meta`, pass unchanged. Editor answers to agent requests pass unchanged.
9. `session/close`: the hub refuses it (Hermes does not implement it), so the bridge answers it itself and forwards nothing. A `sessionId` that is not a non-empty string gets `-32602`. Otherwise the answer is the ACP `CloseSessionResponse`, `{}`. When the editor opened the session (point 4), the bridge forgets it, so a reconnect does not load it again, and sends `session/cancel` with `{"sessionId"}` on the current connection when it is ready: ACP asks the agent to cancel the session's work on close. Hermes then cancels a prompt still running in the session, and the prompt's answer reaches the editor as usual. A close of a session that is not open gets `{}` too and sends nothing.
10. `initialize`: on a successful result, the bridge sets `agentCapabilities.sessionCapabilities.close` to `{}` (ACP's unstable session close capability), since it answers `session/close` (point 9). It keeps every other member, and a `close` the agent already sent. A result whose `agentCapabilities` or `sessionCapabilities` is not an object passes unchanged. Hermes v0.22.7 advertises `loadSession`, image prompts and the `fork`, `list` and `resume` session capabilities; the hub allows all of them.

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

`hermes_version`, `plugin_version` and `placeholder` come from the plugin's `GET /info` (section 9); if the backend is unreachable the hub answers `502` `{"error": "backend_unavailable"}`. When `chat` runs `chat.hermes` instead of the managed build (5.7), it compares `hermes_version` with the first line of the local `hermes --version` (`Hermes Agent v<version> (<date>)`), exactly after removing a leading `v`, and refuses to start on a mismatch (the local TUI and the server must be the same release).

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
| `user` | none | The hub user name of the one person who uses this backend. Sessions that no computer serves act for this user when they pick a browser device (docs/browser.md section 3.2). Leave it unset on a backend that more than one hub user may use |
| `browser` | `true` | `false` hides the `device-browser` toolset (docs/browser.md section 3) |
| `mcp` | `true` | `false` hides the `device-mcp` toolset and turns its tools off (section 8.11) |
| `mcp_approval` | `write` | When `mcp_device_call` asks the user first: `write` for tools whose server does not mark them `readOnlyHint: true`, `always` for every tool, `never` |
| `agent_messages` | `true` | `false` hides the `device-agents` toolset (section 12) |
| `mcp_call_timeout` | `600` | Seconds `mcp_device_call` waits when the agent gives no `timeout`, at most 3600 |
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

The browser devices that sessions and scheduled jobs chose are in a separate file, `$HERMES_HOME/device/browser.json` (docs/browser.md section 3.2). acp-host does not read it.

### 8.3 Binding resolution

For each command and file operation, the plugin resolves the session id of the running turn through its lineage. It reads the id only from the `HERMES_SESSION_ID` context variable of `gateway.session_context`, never from the process environment (`get_session_env` falls back to `os.environ`, which agent construction overwrites, tools/delegate_tool_dispatch.py:75-79). When the context variable is unset, the command fails closed.

1. Walk from the session to its root through `parent_session_id` (compression children, delegate subagents, branches and forks all record their parent in the sessions table; agent/compression_facade.py rotates `HERMES_SESSION_ID` mid-session, tools/delegate_tool_dispatch.py gives subagents their own id).
2. The nearest session on that path with a binding gives the binding. Its device and directory serve the command.
3. If no session on the path has a binding but any of them is in `ever_bound`, the command fails closed: it does not run, and the tool gets an error that names the session and says to rebind with `/device`.
4. If the lineage cannot be read completely (no row yet, a store error, a cycle, more than 100 hops), the command fails closed the same way.
5. A session whose cwd (the session cwd context variable that Hermes sets from the frozen policy) is the placeholder, and that has no binding, fails closed. This covers sessions created for a device but not bound yet or no longer bound: an ACP bind failure, a hub-created session between create and bind, a create the splice lost.
6. Side agents with no parent row (`/bg` and other background tasks, gateway/session_tui_side.py and gateway/run_background.py) inherit the binding of the session that started them; the plugin wraps their creation to record that link. If the link cannot be recorded, a background task started from a bound session is refused. Cron jobs and Kanban runs are not bound to the session that created them. For a session bound to a device, the plugin refuses the tools that create them (`POD_ONLY_TOOLS`). With `fallback: device` a cron run is a never-bound session that runs on the sandbox (section 8.10), so a sandbox session and a session bound to a device may use `cronjob_manage` (create, list, pause, remove and the rest), but the plugin refuses the job fields that run or fetch on the pod: `script`, `monitor`, `monitor_script`, `monitor_url`, `no_agent` and `workdir`. It also refuses every action except `create`, `list`, `pause`, `remove` and `cancel` on a stored job that has one of those fields (a job made on the server), so such a session cannot run, resume or change it. The plugin marks each job that a device or sandbox session creates or updates (in `$HERMES_HOME/device/browser.json`). When `fallback` is later set to something other than `device`, the runs of a marked job may not use any tool, so such a job never acts on the pod. Jobs made before plugin 0.14.0 are not marked, and the marks are lost when that file is deleted (which needs access to the pod); check the job list before you change `fallback`. A job's run uses the Firefox of the job's browser device, which the job takes from the session that created it (docs/browser.md section 3.2); its commands still run on the sandbox. The Kanban tools stay refused.
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

For every file tool of a bound session (`read_file`, `write_file`, `patch`, `search_files` and the like), the guard rewrites path arguments to absolute device paths (relative paths against the session's device cwd, `~` against the device `home`) with `{"action": "modify", "args": ...}`. For `write_file` and `patch` (including each file of a V4A patch) it then resolves the target path on the device and blocks the call (`{"action": "block", "message": ...}`) when it hits Hermes's write denylist rebuilt for the device's `home` from `hello` (agent/file_safety.py `build_write_denied_paths`, `build_write_denied_prefixes`, `build_write_approval_paths`), or lies outside the device's roots. The home rules use both the `home` from `hello` and its `pwd -P` on the device (asked once per connection), since roots are real paths, and compare paths case-folded, since macOS file systems ignore case. The hook also keeps the session's cwd record pointed at the device cwd, so file tools resolve relative paths there, and blocks any tool call of a bound session whose terminal environment is not the device backend (a session created before `terminal.backend: device`).

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

With `fallback: device`, a session that rule 7 of section 8.3 finds never bound runs on the sandbox device `fallback_device`, a service device that the hub knows from its own config and that authenticates on the exec leg only. The sandbox is a separate pod (image `hermes-device-sandbox`, one per person, in its own namespace) that runs `talaria run --no-splice`.

1. The plugin gives the session an implicit binding: device = the connected device whose id is `fallback_device`, else, only when no device with that id connected since the gateway started, the one connected device with that name; dir = `<fallback_root>/<name>`, where `<name>` is the id of the session's lineage root (the session at the top of its `parent_session_id` chain) when it matches `[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}`, else a cleaned prefix of the id, `~`, and the first 12 hex digits of its SHA-256 (a name that passes unchanged never holds `~`, so two ids never share a directory). Every session of one lineage (compression children, subagents, branches, side agents) shares the directory.
2. The implicit binding is computed for each resolution and never stored. It is not in `bindings.json`, not in `ever_bound`, and not returned by `GET /bindings`, so acp-host does not filter on it and section 9's PUT works on the session as on any never-bound session.
3. Everything that treats a bound session specially treats it the same way: the terminal backend runs its commands on the sandbox in that directory, `uses_host_filesystem` is false, the guard (8.6) rewrites file tool paths and checks writes against the denylist rebuilt for the sandbox's `home` and its roots, edit approval and context references read from the sandbox, checkpoints are skipped, and the Kanban tools of `POD_ONLY_TOOLS`, the pod fields of `cronjob_manage` (rule 6 of 8.3) and bypass (safe mode) turns are refused. Browser tools do not use the sandbox: they use the session's browser device, one of the user's computers (docs/browser.md section 3.2). As for a bound session whose terminal backend is not `device`, the guard blocks every tool call of a never-bound session whose frozen backend is not `device` (a session created before `terminal.backend: device`).

   One difference: the session's server cwd stays `terminal.cwd`, not the placeholder (rule 5 of 8.3 would fail it closed). Hermes builds the system prompt in the gateway and reads project context files (`AGENTS.md`, `.hermes.md` and the like) and the git state from that server directory (agent/system_prompt.py). The agent cannot choose these paths, but keep `terminal.cwd` an empty directory, so the prompt holds no pod files.
4. Before the first command whose cwd is in that directory, on each connection of the sandbox, the plugin runs `mkdir -p -- <dir>` on the sandbox, with cwd the sandbox root that holds `fallback_root`, so `fallback_root` itself may be missing. If that fails, the command fails with exit 126 (255 when the sandbox dropped) and a note, and nothing else runs. When the sandbox refuses a command in that directory (for example because the agent deleted it), that command fails and the next one creates the directory again.
5. The command fails, and never runs on the pod, when the sandbox device is not connected, when the name matches more than one connected device, when `fallback_root` is not an absolute path, and when `fallback_root` is not inside the sandbox's roots from `hello`. The terminal gets exit 1 and a message that names the sandbox; a command whose sandbox drops while it is sent gets exit 255. File tools are blocked with the same message.
6. An explicit bind (`/device <device> <dir>`, the splice, the ACP bridge or the PUT route) wins, and the PUT rules of section 9 allow it, since the implicit binding is not stored: the session then runs on that device, and is in `ever_bound` like any bound session. After `/device off` it fails closed (rule 3); it does not return to the sandbox.
7. `/device` without arguments names the sandbox and the directory; `/device off` answers that no device was bound.

Name the sandbox by its device id. A name lookup takes any connected device of that name, including a PC the user named the same, and an id falls back to a name lookup until the sandbox has connected once since the gateway started. Either way the device must also have `fallback_root` inside its roots.

The plugin also checks `fallback_root` against the pod-writable paths of section 8.7. Add it to `device_write_roots` and to `HERMES_WRITE_SAFE_ROOT` when that variable is set, or file tools cannot write there.

The bindings file format does not change (version 2).

### 8.11 device-mcp toolset

The plugin registers three tools in the toolset `device-mcp`, so the agent can use the MCP servers of section 4.9:

| Tool | What it does |
|---|---|
| `mcp_device_servers` | Lists the servers of the session's device, with their state (op `list`) |
| `mcp_device_tools` | Lists a server's tools, by name and the first line of the description cut to 160 characters (at most 400, filtered by `query`), or with `tool` gives one tool's description, input schema and annotations (op `tools`; the plugin keeps the list 5 minutes per connection, `refresh` asks again) |
| `mcp_device_call` | Runs one tool with `arguments` (op `call`) and returns its content |

The set of tools is fixed, so it does not depend on which device a session uses and needs no change to Hermes. Hermes's tool list and its `check_fn` results are per process, not per session (tools/registry.py, model_tools.py), so a tool per device and per server could not be shown only to the sessions that may use it. The fixed set keeps the model's tool list small too: a server can have hundreds of tools, which the agent finds with `mcp_device_tools` instead.

Gating. Each tool resolves the binding of the running session (section 8.3) on every call and uses only the bound device. A session that is not bound, fails closed, or runs on the sandbox device (section 8.10) gets an error and sends nothing. A session cannot name a device: `server` must be one of the bound device's `mcp_servers`, and the device runs only the servers its own config lists. `check_fn` shows the tools when the setting `mcp` is not `false` and any connected device offers `mcp`; it cannot see the session, so it gates nothing.

Approval. Before `mcp_device_call`, the plugin's `pre_tool_call` asks the user through Hermes's approval gate (`{"action": "approve"}` with rule key `device-mcp:<device id>:<server>:<tool>`, so the user can allow one tool on one device for the session or always), following `mcp_approval`. The prompt shows the arguments as the tool will send them (a JSON string is parsed first), up to 2000 characters, and says how long they are when it cuts them. With `write`, a tool whose server marks it `readOnlyHint: true` does not ask. That trusts the server's own claim, which MCP calls untrusted; set `always` to ask for every tool. The hook never waits for the device: Hermes blocks every tool call while a `pre_tool_call` hook runs over its timeout (hermes_cli/plugins_dispatch.py), so the hook uses only the cached tool list, asks when it has none, and fetches the list in the background. The hook blocks a call the session cannot make (no binding, a server the device does not list, arguments that are not an object) and records the device it checked; the handler refuses the call if the session's device changed in between. Hermes's own MCP approval rules do not apply, because these are plugin tools.

Results. The result is JSON with `device`, `server`, `tool`, `is_error`, `content` (the text blocks joined, with notes for resource links, embedded resources and other content; cut in the middle above 2,000,000 characters), `structured_content` when the server sent it, and a `note` that the content is untrusted data. Up to 4 PNG, JPEG, GIF or WebP images of valid base64 and at most 5 MiB each come back as Hermes's multimodal tool result when the model can take images in tool results. Every tool name starts with `mcp_`, so Hermes wraps the results in its untrusted tool result markers and applies its MCP result size budget (agent/tool_dispatch_helpers.py, tools/budget_config.py). A Hermes interrupt (`/stop`) of the turn sends `kill` for the request.

### 8.12 Session list audience

With a Hermes that has the plugin hook `filter_session_list` (hermes-patches 0004, `runtime.describe` lists `session-list-audience-v1`), the plugin registers it. A gateway `session.list` with `audience` `hermes-device:<device id>` then lists only the sessions whose lineage the plugin resolves (8.3, stored bindings only) as bound to that device. Sessions that are never bound, fail closed, run on the sandbox device or are bound to another device are left out. The plugin returns `None` for any other audience, so it judges only its own. hermes-hub sends that audience on a device's gateway leg once a probe listing with it succeeds. On a Hermes without the hook the plugin does not register it, and the gateway refuses every `audience`.

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
| `POST /sessions/{session_id}/files` | Body `{"name", "content_base64"}`. Writes a file the user attached into the session's device directory (upload rules below). `200` `{"path", "name", "device_id", "bytes"}`: `path` is the absolute device path, `name` the file name the plugin chose |
| `GET /sessions/{session_id}/files?path=<abs>` | Reads a file of the device that serves the session, for the hub to show the user (read rules below). `200` with the file's bytes as `application/octet-stream`, with `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox` and `Cache-Control: no-store` |
| `GET /sessions/{session_id}/tree?path=<abs>` | Lists a folder of the device that serves the session, for the hub's file viewer (listing rules below). `200` `{"path", "dir", "device_id", "entries": [{"name", "kind", "link", "size"}], "truncated"}`. `path` defaults to the session's folder `dir`. `kind` is `dir`, `file` or `other`, `link` is true for a symbolic link (`kind` is its target's), `size` is the size of a regular file that is not a link, else 0. Folders come first, then by name |
| `GET /sessions/{session_id}/diff` | What git shows as changed in the session's folder (diff rules below). `200` `{"dir", "device_id", "git", "refused", "files": [{"path", "status"}], "untracked": [<path>], "diff", "truncated", "hidden"}` |
| `GET /sessions/{session_id}/changes` | The files the agent wrote in the chat, section 9.2 |
| `GET /search?q=&limit=` | Full-text search across chats, section 9.2 |
| `GET /usage?days=` | Token counters per session and model, section 9.2 |
| `GET /memory` | `200` Memory: Hermes's built-in memory, section 9.1 |
| `POST /memory/{target}/add` | Body `{"content"}`. Adds an entry. `200` Memory |
| `POST /memory/{target}/edit` | Body `{"entry", "content"}`. Replaces the entry that equals `entry`. `200` Memory |
| `POST /memory/{target}/remove` | Body `{"entry"}`. Removes the entry that equals `entry`. `200` Memory |
| `POST /memory/{target}/replace` | Body `{"text", "base"}`. Applies a whole-file edit. `200` Memory |
| `POST /memory/pending/{id}/approve` | Applies a staged memory write. `200` Memory |
| `POST /memory/pending/{id}/reject` | Drops a staged memory write. `200` Memory |
| `GET /agents/requests?wait=<seconds>` | The hub's long poll for agent message requests, section 12. `200` `{"requests": [Request]}` once one is waiting, or empty after `wait` (at most 25) |
| `POST /agents/requests/{id}` | Body `{"result": <any>}` or `{"error": {"code", "message"}}`: the answer to a request a tool call waits for. `204`; `404` `unknown_request` when no call waits for it (it timed out) |
| `POST /agents/deliver` | Body `{"mode": "steer" or "inbox", "message_id", "session_id", "subagent_id"?, "text"}`: delivers a checked message into a running agent of this process, section 12. `200` `{"status": "steered" or "queued" or "not_running" or "gone" or "full"}` |
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

Upload rules:

- The plugin resolves the session through its lineage (8.3). A session that is never bound goes to the sandbox with `fallback: device` (8.10). With `fallback: local` the answer is `409` `runs_on_pod`: the session's commands run on the pod, so the hub hands the file to Hermes (`file.attach`) instead. Every other session that no device serves is refused with `409` `fail_closed` and the reason. A device that is not connected gives `409` `device_offline`.
- The file goes into `<dir>/.hermes/attachments/`, where `<dir>` is the binding's directory, the folder Hermes uses for workspace attachments (tui_gateway/prompt_attachments.py `_session_home_dir`). The plugin creates `.hermes` and `attachments` with mode `0700` and refuses (`422` `write_refused`) when either one is a symbolic link or not a folder, or when the device's `pwd -P` of the folder is not exactly `<pwd -P of dir>/.hermes/attachments`.
- The name is the last part of `name` without control characters, slashes, backslashes and leading dots, cut to 120 bytes, else `attachment`. The plugin never overwrites a file: when the name is taken it tries `<stem>-2<ext>` up to `<stem>-999<ext>`, and it opens the file once, with `O_EXCL` (the shell's `set -C`), so it does not follow a symbolic link put there meanwhile. The file gets mode `0600`.
- The target must pass the write rules of file tools (8.6 and 8.7): inside the device's roots, not on the credential denylist rebuilt for the device's `home`, and inside `HERMES_WRITE_SAFE_ROOT` when that is set. Otherwise `422` `write_refused`.
- The file is at most 32 MiB (`413` `too_large`). The plugin sends it to the device as base64 on the stdin of one `exec`, and the device decodes it with `base64 -d`, `base64 -D` or `openssl base64 -d -A`, whichever it has. `400` `bad_request` for a malformed body or base64. Each upload holds the file several times in the gateway's memory, so at most 2 run at once; another one gets `429` `busy`.

Read rules:

- The hub uses this route for a `MEDIA:<path>` file of a chat that is not on the Hermes server, and for its file viewer. The hub checks that the user may use the backend, and for a `MEDIA:` file that the chat names the path; the plugin applies the rules here.
- The session resolves as for uploads: `409` `runs_on_pod` for a session whose commands run on the pod, `409` `fail_closed` when no device serves it, `409` `device_offline`. The file is read from that device only.
- `path` must be absolute and clean (no `.` or `..` parts, no double or trailing slash, no leading `//`), at most 4096 bytes, without control characters, and name a file (`400` `bad_path`).
- The path must pass the read rules of file tools rebuilt for the device's home, before the read and again for its real path on the device: inside the device's roots, not a sensitive home file or credential folder (`agent.context_references._SENSITIVE_HOME_FILES`, `agent.file_safety.HOME_CREDENTIAL_DIRS`), not on the write denylist of 8.6, not a Hermes credential store or read-denied folder under `<home>/.hermes` or any `<home>/.hermes/profiles/<name>` (`agent.file_safety._CREDENTIAL_FILE_NAMES`, `_READ_DENIED_DIRS`, `skills/.hub`), not under `.config/talaria` (this daemon's state and control secret), `.config/hermes`, `.claude`, `.codex` or `.terraform.d`, not a project `.env` file (`_BLOCKED_PROJECT_ENV_BASENAMES`), and not a key file (`.key`, `.pem`, `.p12`, `.pfx`). Otherwise `422` `read_refused`. The home rules use both the `home` the device reported and its `pwd -P` on the device, which the plugin asks once per connection, because roots are real paths and a home behind a symbolic link would match none of them. Paths compare case-folded, since macOS file systems ignore case. The write denylist of 8.6 uses the same two homes and case folding.
- The device runs one `/bin/sh` script in the binding's directory. It enters the file's folder with `cd -P` and reports that folder's `pwd -P`, which gives the real path the plugin checks. It refuses a file whose last part is a symbolic link, that is not a regular file, that it cannot read, or that has more than one hard link (`422` `read_refused`). It opens the file twice and checks that both descriptors are the file the name holds (`test -ef` against `/dev/fd/3` and `/dev/fd/4`), so a name swapped for a link meanwhile is refused. It then sends the size (from `ls -ln`) and the base64 of the bytes on one line (`base64`, else `openssl base64 -A`). The plugin drops the bytes unless every check passes. File systems that always report one hard link (some FUSE, 9p and WSL drvfs mounts) do not catch other hard links.
- A missing file is `404` `not_found`. A file over 32 MiB is `413` `too_large`, before any byte is sent. The read times out after 120 seconds. When the bytes do not decode or do not match the size (the file changed, or a dropped exec leg lost output), the answer is `422` `read_failed`. Error messages never hold the device's output or the real path.
- At its peak a read holds the file about four times in the gateway's memory (128 MiB for a 32 MiB file in a replay of the parsing), so at most 2 run at once, each until its answer is sent; another one gets `429` `busy`.

Listing rules:

- The session resolves as for reads, and the folder must pass the read rules above, before the listing and again for its real path on the device (`422` `read_refused`). `path` must be absolute and clean (`400` `bad_path`).
- The device runs one `/bin/sh` script that enters the folder with `cd -P`, reports its `pwd -P` and prints one record per entry, at most 1000 (`truncated` is then true). The plugin leaves out every entry whose path does not pass the read rules (credential folders, `.env` files, key files) and every name with a control character or one that is not valid UTF-8. A missing folder is `404` `not_found`; a folder the device user cannot open is `422` `read_refused`. Opening an entry goes through the read route and its rules; a symbolic link is listed but the read route refuses it, and a linked folder lists only when its real path passes the rules.

Diff rules:

- The session resolves as for reads, and its folder `dir` and its real path must pass the read rules (`422` `read_refused`).
- The device runs git in the real folder: first the `--name-status` list of changes against `HEAD` (the empty tree before the first commit) and the untracked files that `.gitignore` does not hide, both limited to the folder (`--relative`, pathspec `.`), then the diff of the listed paths only, each given as a `:(literal)` pathspec. So the diff never shows a file outside the folder, even when the repository holds more. The plugin leaves out every path that does not pass the read rules and counts them in `hidden`, so a changed `.env` file or key stays out of the diff. Renames show as a delete and an add (`--no-renames`), so each path is checked on its own.
- git must work on a repository of the folder: the work tree's top (`rev-parse --show-toplevel`, real path) must hold the folder, and the git dir and common git dir (`--absolute-git-dir`, `--git-common-dir`, real paths) must pass the read rules, so they are inside the device roots and not credential folders. Otherwise `git` is false and `refused` is true. So a `.git` file in the folder that points at a repository elsewhere, or a `core.worktree` that points at other files, shows nothing. The diff then runs with that `GIT_DIR` and `GIT_WORK_TREE` set explicitly.
- git runs read-only and runs no helper programs it can avoid: `GIT_OPTIONAL_LOCKS=0` (no index refresh), `GIT_CONFIG_NOSYSTEM=1`, no pager, no hooks, `core.fsmonitor=false`, `safe.bareRepository=explicit`, `--no-ext-diff`, `--no-textconv`, no color. Submodules show as one line (`--submodule=short`, `diff.submodule=short`), never with their contents, and changes inside them that are not committed are ignored. Clean filters that the repository's config sets still run, as for any `git diff`.
- A changed path that is a regular file with more than one hard link is left out of the diff (the device checks its link count with `ls -ln`) and counted in `hidden`, as the read route refuses such files. File systems that always report one link do not catch this.
- `git` is false when the device has no git or the folder is not in a work tree. The diff holds at most 1 MiB, at most 500 files, 256 KiB of changed names and 256 KiB of untracked names; `truncated` is true when any of these cut it short. Binary files show as git shows them, without content.
- At most 4 listings and diffs run at once; another one gets `429` `busy`. Each times out after 60 seconds. Error messages never hold the device's output.

Errors from the dirs route: `409` `device_offline` for any device that is not connected, `422` `dir_refused`, `400` `bad_dir`.

`/device <device> <dir>` uses wider rules (8.8). `GET /sessions/{id}/binding` returns the binding that serves the session, which may belong to an ancestor (`resolved.via` names it). `DELETE` removes only the session's own binding; `/device off` removes the binding that serves the current session.

### 9.1 Built-in memory

The memory routes serve the hub's Memory page. They read and change `MEMORY.md` and `USER.md` in `<HERMES_HOME>/memories/` and the memory writes that Hermes staged for approval in `<HERMES_HOME>/pending/memory/` (`memory.write_approval`). `{target}` is `memory` (`MEMORY.md`) or `user` (`USER.md`); any other is `404` `unknown_target`.

Memory object: `{"approval": <bool>, "targets": {"memory": Store, "user": Store}, "pending": [Pending]}`. Store: `{"file", "limit", "used", "enabled", "entries": [<string>]}`, where `limit` is the configured char limit (`memory.memory_char_limit`, `memory.user_char_limit`), `used` the chars of the entries joined by `\n§\n` (the count Hermes checks against the limit) and `enabled` false when `memory.memory_enabled` or `memory.user_profile_enabled` turns the store off. Pending: `{"id", "target", "summary", "origin", "created_at", "changes": [{"action", "content", "old_text", "entry"}]}`, oldest first; `entry` is the whole entry a staged `replace`, `patch` or `remove` is pinned to.

Rules:

- Every change goes through Hermes's own `MemoryStore` (`tools.memory_tool.load_on_disk_store`), with the limits from config, the `MEMORY.md.lock` lock, the re-read from disk and the threat scan. The routes never write the files themselves. The changes are the user's own, so they do not pass the approval gate.
- `edit` and `remove` act on the one entry that equals `entry` exactly (the store's `matched_entry` pinning), never on a substring match. When no entry equals it any more: `409` `changed`.
- `replace` splits `text` into entries at lines that hold only `§`. `base` is the list of entries the editor showed. It then applies one all-or-nothing batch: removes of the entries of `base` that the text no longer has, pinned to those entries, and adds of the entries of the text that `base` did not have. An entry Hermes added after the editor opened is kept. When Hermes changed or removed an entry of `base` meanwhile, the edit fails with `409` `changed`. Kept entries keep their place; new ones go last. An edit that would leave the file empty is `409` `would_empty`.
- `approve` runs `tools.memory_tool.apply_memory_pending` against a fresh store and then deletes the record, as `/memory approve <id>` does. A staged write whose pinned entry changed is refused with `409` `changed` and its record stays. When applying it raises, the answer is `500` `apply_failed` and the error goes to the gateway log. `reject` deletes the record (`/memory reject <id>`). `id` must match `[A-Za-z0-9_-]{1,64}`, else `404` `unknown_pending`, as for an unknown record.
- Errors: `409` `over_limit` when the result would pass the store's limit, `422` `blocked` when the threat scan refuses the text (the message gives the reason), `409` `disabled` for a store that is turned off, `400` `bad_request` for a malformed body or empty text, `413` `too_large` for a text over 64 KiB. Other store refusals are `409` with the store's failure class as the code and its message.
- The routes do not change `memory.write_approval`. The hub sets it with the dashboard's own `PUT /api/config`.
- Running chats keep the memory they loaded at their start, so changes apply to new chats.

### 9.2 Session store views

These routes read Hermes's session store (`state.db`) through its own `SessionDB` and never write it. They serve the hub's chat search, its Usage page and its list of files the agent changed. At most 4 of them run at once; another one gets `429` `busy`. `503` `store_unavailable` when the store cannot be opened.

- `GET /search?q=&limit=`: `q` is 1 to 200 characters, Hermes's search syntax (keywords, `"phrases"`, `AND`, `OR`, `NOT`, `prefix*`), else `400` `bad_query`. It runs `SessionDB.search_messages` over user and assistant messages, without the sources that `session_search` hides (`kanban`, `subagent`, `tool`). `200` `{"results": [{"session_id", "title", "source", "started_at", "last_active", "archived", "hits": [{"row_id", "session_id", "role", "snippet", "timestamp"}]}]}`, best match first, at most `limit` chats (1 to 50, default 20) and 3 hits each. A result's `session_id` is the chat to open: the tip of the compression chain that holds the message (`get_compression_tip`). A hit's `session_id` is the part of that chain that holds it, and `row_id` is the message's `messages.id`, the `row_id` of the transcript that `session.history` sends. Snippets come from FTS5 `snippet()` and mark matches with `>>>` and `<<<`; a fallback search returns a plain excerpt. Text is cut to 400 characters, with control characters replaced.
- `GET /usage?days=`: `days` is 1 to 90 (default 30). `200` `{"since", "sessions": [Session], "models": [Model], "truncated"}`. Session: `{"session_id", "chat_id", "title", "source", "model", "provider", "started_at", "last_active", "input", "output", "cache_read", "cache_write", "reasoning", "calls", "cost_usd"}` for each session row with tokens or API calls whose last activity is in the window, newest first, at most 5000. `chat_id` is the tip of its compression chain. The counters are the session's totals, so a session that ran over several days counts on the day it was last active. Model: `{"model", "provider", "input", "output", "cache_read", "cache_write", "reasoning", "calls", "cost_usd"}`, summed over the `session_model_usage` rows last seen in the window. Hermes stores no per-call times, durations or rate-limit headers, so none are here.
- `GET /sessions/{session_id}/changes`: `200` `{"session_id", "files": [{"path", "tools", "count", "last_at", "row_id"}], "truncated"}`: every path that a `write_file` or `patch` tool call names in any part of the chat's compression chain, most recent first, at most 500. A V4A patch names its files in `*** Update File:`, `*** Add File:`, `*** Delete File:` and `*** Move to:` lines. Paths are as the agent gave them (absolute, `~/...` or relative to its working folder) and say what it asked for, not whether the write worked. `404` `unknown_session`.

## 10. Security notes

- The device daemon runs commands only in its own roots, and only on its own user's backend. It can lie about command output.
- The hub enforces the allowlist and the refused params of 5.4, the create rewrite and the session checks of 5.3 on the gateway leg, so a holder of a device token who dials the gateway leg directly meets them. A method that a new Hermes release adds is refused until the hub's allowlist names it. The hub also parses the ACP leg (6.1): it allows only the ACP methods Hermes implements, pins their params and refuses `_meta` and `field_meta`, refuses sessions that are not bound to the device, refuses slash commands outside the allowlist in prompts, rewrites cwd and MCP servers, and answers the agent's `fs/*` and `terminal/*` requests itself. Behind it, acp-host pins the params too, refuses sessions that are not bound to the device itself, applies the same slash rule to prompts (`/discard` and `/tools` included, as in the hub) and rewrites cwd and MCP servers, but it removes `_meta` instead of refusing it and passes other methods and the agent's requests (6.1, 6.2). A token cannot reach another user's backend, mint assertions, or learn the backend's dashboard password.
- What a device token still allows on the gateway leg: running commands in the sessions bound to that device, and the calls of the 5.4 allowlist, which act on those sessions or only read. Some of them read beyond the device's own sessions: `session.list`, `session.active_list` and `session.most_recent` are not filtered by device, so they show the titles and other list fields of all of the user's sessions. This is intended: they are the user's own sessions, shown to the user's own devices, and the splice uses `session.list` to find a session by title or the latest one (5.3). Every call that names a session must name one of the device's sessions. Sessions that are bound to no device are refused on the gateway leg (`hermes_device_not_bound`) and on the ACP leg (`session is not bound to this device`). A device can still bind a never-bound session to itself with the PUT route (7.4), then use it. A compromised device can also lie about command output.
- Only the hub reaches the plugin's HTTP routes (they are behind dashboard sign-in), so a device token cannot use the upload or read routes (section 9). The route writes only into `.hermes/attachments` of the directory that serves the session, refuses symbolic links on the way there, never overwrites a file, and applies the write rules of file tools.
- The read route reads only from the device that serves the session, inside its roots, and refuses credential paths, symbolic links as the last part, other hard links and files over 32 MiB. These rules are by path: the agent can still copy a credential file into a root under another name with a command, as it can with any command. The hub shows such a file only when the chat named it. With the sandbox, all sessions of one person share `/work`, so one chat can name and show a file of another chat's directory.
- The memory routes (9.1) are behind the same sign-in, so a device token cannot read or change memory either. Memory text enters every new chat's system prompt, so the routes keep Hermes's threat scan on every text they write, and the hub shows a backend's memory only to the users who may use that backend.
- A device token can bind only sessions that resolve to its device or were never bound in their lineage (7.4, and the plugin's own rules in section 9). It cannot take a session of another device's lineage (`409` `bound_to_other_device`), or one whose binding was removed (`409` `was_bound`). It can still claim any never-bound session of the user, such as a web chat, and then run commands in it on itself. When a create's bind fails, the hub deletes the new session only when it is never bound, so a create cannot delete a session that is or was bound.
- A compromised Hermes pod can do anything the user's Hermes can, including running commands on connected devices in their roots. It cannot mint hub assertions, because it holds only public keys.
- A compromised Hermes pod can use the browser of a connected device that has browser control on, as the agent can (docs/browser.md section 8), as it can run commands there.
- A session's browser device (docs/browser.md section 3.2) is one of the connected devices of the session's hub user, never a service device: the user of the computer the session is bound to, else the plugin setting `user`. Without that setting, a session that no computer serves has no browser device. Scheduling unattended work with a browser device asks the user each time. A run of a job that has no browser device has no browser. A scheduled job keeps the browser device of the session that created it in the plugin's own file on the pod, which a device cannot reach. A run of the job uses only that device and fails when it is offline or now reports another user; a revoked device cannot connect, so its jobs fail until the user picks another device. The job's commands still run on the sandbox.
- MCP servers on a device (4.9) run only when the device's own config lists them; the plugin and the hub cannot add one or change its command. They run as the device's user, outside `device.roots`, with what that user can reach, so list only servers you would let the agent use. A session reaches only the servers of the device it is bound to (8.11). A compromised Hermes pod can call any tool of a connected device's servers with any arguments, as it can run commands there. MCP results come from the server and from what it reads, so they are untrusted data: the plugin marks them so and Hermes wraps them (8.11).
- The hub's private key is the root of trust for every backend. Keep it in the hub's secret store only.
- The managed Hermes (5.7) runs the build that the plugin names in `welcome.hermes_image`. That is no new trust: a Hermes pod that can name an image can also run any command on its devices through the exec leg. The device pulls only by digest and checks every byte against it, so a registry or mirror in between cannot change the build. No signature of the Hermes project is checked: Hermes publishes none for its images or releases.
- With `fallback: local`, the commands of never-bound sessions run in the gateway container and can read its secrets, environment, state database and processes. `fallback: device` (8.10) moves them to a sandbox pod that holds only its own device token and the `/work` volume. The sandbox's commands run as the same user as its daemon, so they can read its token file and open an exec leg of their own, which replaces the daemon's connection (`4409`). That token is a service device token, which the hub accepts on the exec leg only, so it reaches no gateway or ACP leg. `run` removes `TALARIA_HUB_TOKEN` from the environment of the commands it runs, but they can still read it from `/proc` of the daemon, so an environment variable or a flag is no better hidden than the file. All of one person's sessions share the `/work` volume, so one session can read another's directory. Limit the sandbox's network to the internet and the hub.

## 11. Versions

The device client, the plugin and acp-host come from one talaria release. Release 0.3.0 speaks exec leg protocol versions 2 and 3 and negotiates the newest both sides speak (section 4.1), so a 0.3.0 device works with a 0.2.0 plugin and the other way round, without resume. Upgrade the plugin first: devices then get resume as soon as they upgrade. A side that shares no version with its peer closes with `4426` and a reason that names both versions. The ACP and gateway legs did not change. On Linux, `chat` runs the server's own Hermes build from `welcome.hermes_image` (5.7). Otherwise the local Hermes that `chat` runs must be the same release as the server's Hermes (`chat` checks `hermes_version`).

| Peers | Exec leg |
|---|---|
| device 0.3.0, plugin 0.3.0 | version 3, with resume |
| device 0.3.0, plugin 0.2.0 | version 2 (the device's `hello.version` is 2) |
| device 0.2.0, plugin 0.3.0 | version 2 (`hello` has no `max_version`) |

MCP servers (4.9) need a device and a plugin that both know the `mcp` capability; the hub needs no change. With an older peer the capability is not negotiated and the plugin's MCP tools report that the device offers no MCP servers.

Browser control needs a device and a plugin that both know the `browser` capability. With an older peer on either side the capability is not negotiated: the leg works as before, and the plugin's browser tools report that the device does not offer browser control.

hermes-hub v0.5.0 refuses a `session.resume` by title or `latest` or with an empty `session_id` (5.4). The Ink TUI never sends those: it resumes by `session_id`, which may hold a title (5.3). With a device client of release 0.4.1 or older, `talaria chat -- -c` and `--resume <title>` fail with any hub, because the local Hermes launcher resolves them in its own session store (5.2), and a title sent as `session_id` fails the splice's bind. Later releases look the session up on the server and resume it by id (5.2, 5.3). The acp-host of those releases applies the hub v0.5.0 slash rules to prompts (6.2).

Plugin 0.4.1 and the acp-host of the same release add refusals and change no wire format. The plugin refuses a device bind of a session of another device's lineage or of a fail-closed lineage (section 9), and `/device` can move a session between devices (8.8). acp-host pins ACP params, removes `_meta`, refuses sessions that are not bound to the device itself, and applies the slash-command allowlist to prompts (6.1, 6.2). Editors and the bridge need no change: the bridge binds every session before it uses it.

## 12. Agent messages

The plugin registers three tools in the toolset `device-agents`, so the user's agents can message each other through the hub, as Claude Code sessions do: a chat, or a subagent that a chat delegated to (`delegate_task`), lists its peers and sends one of them a message. Where a chat runs its commands (a computer, the sandbox or the Hermes server) does not matter: a message goes from Hermes to the hub and back to Hermes, never through a device. The toolset is on while a hub polled the bus in the last 60 seconds (`check_fn`); the setting `agent_messages: false` turns it off.

| Tool | Args | Does |
|---|---|---|
| `agent_peers` | none | Lists the user's other chats on this Hermes (session id, short name, title, computer and folder, whether a turn runs, who started it) and the live subagents of the caller's delegation tree with their relation (parent, child, sibling) |
| `agent_send` | `to`, `text`, `reply_to`? | Sends `text` (at most 8000 characters) to `to`: a session id, a short name, an exact chat title, a subagent id (`sa-...`) or `parent` |
| `agent_set_name` | `name` | Gives the calling chat a short name for `to` (1 to 40 of `A-Za-z0-9_-`, unique per backend); `""` removes it |

Bus. A tool call puts a Request `{"id": "ar_<hex>", "op": "peers" or "send" or "name", "caller": Caller, "args"}` on an in-process queue and waits up to 30 seconds for the hub's answer, which it returns to the model as is (an error as `{"error", "code"}`). The hub takes requests with `GET /agents/requests` and answers each with `POST /agents/requests/{id}`. Caller is `{"session_id", "root_session_id", "tree": [{"subagent_id", "session_id", "parent_id", "depth", "goal"}]}`, plus `"subagent_id"`, `"parent_session_id"` and `"parent_subagent_id"` for a subagent. The plugin takes the caller from the `session_id` keyword Hermes passes to every tool handler and from Hermes's live subagent registry (`tools.delegate_tool_registry`), never from the model's arguments. `root_session_id` is the chat at the root of the caller's tree; `tree` holds the live subagents with the same root. The plugin also sends an `"undelivered"` request `{"message_id", "session_id"}` that no call waits for (inbox, below). At most 64 calls wait in the queue, at most 4 of them from one session; notices wait in a queue of their own (256, the oldest dropped first), which the hub gets first.

Delivery. The hub checks and records every message and delivers it in one of three ways:

- To another chat: a new input of that chat (`prompt.submit` under the submission id `hermes-hub-agent-<message id>`). An idle chat starts a turn; a busy one runs it after its current turn.
- To a running subagent from its own parent: `mode: steer`, Hermes's own steer (`steer_subagent`), which the child reads after its current tool batch without an interrupt. Only the direct parent steers a child, since Hermes shows a steer to the model as its user's words.
- To any other agent of the caller's tree (a sibling, the parent subagent, or the root chat while its turn runs): `mode: inbox`. The plugin keeps the text for the target's session and adds it to the end of the target's next tool result (`transform_tool_result`), the channel of untrusted data. The plugin knows a chat's turn runs between `pre_llm_call` and `post_llm_call`; for a chat that is not running it answers `not_running` (checked under the lock the turn's end takes) and the hub sends an input instead. A text that no tool result took when the turn ends (`post_llm_call`), or within 90 seconds, goes back to the hub as `undelivered`: a chat then gets it as an input, a subagent that finished never gets it. At most 20 texts wait per session (`full`).

The text the target gets always starts with the line `[Agent message am_<16 hex> from <sender>]` and a paragraph that says another agent wrote it, not the user, that it is no instruction or approval of the user, and how to answer (with `reply_to`). The hub drops control and format characters, turns each `@` that Hermes would expand as a context reference into a full-width `＠`, and turns each `[` that could open a frame marker (such as `[OUT-OF-BAND USER MESSAGE` or `[System note:`) into a full-width `［`, so the text cannot read files into the target's context or fake the end of its frame. The text never starts with `/`, so Hermes does not run it as a command.

Rules (in the hub, hermes-hub README "Agent messages"): only the agents of one backend reach each other, so one person's agents never reach another person's; at most 20 messages per tree and 8 to one target in 10 minutes, 200 per backend in an hour; a chain of answers ends after 6 hops; approvals still go to the user, since Hermes answers an approval only through `approval.respond`, never through a message's text.

Security notes:

- The target model reads a peer message as untrusted input. The frame says so, but a model can still follow what it says; the target's own approvals apply to every command it then runs, on its own computer, with its own permissions.
- A sibling's message rides in a tool result, not as a steer, so it never carries the user's authority. A parent's steer does, as any `delegate_task` goal does.
- Only the hub reaches the routes (dashboard sign-in). A device token reaches neither the routes nor the bus.
- Hermes keeps the first string that a `transform_tool_result` hook returns. When another plugin's hook wins for a tool call, the inbox texts that the plugin added to that result are lost, though the hub recorded them as delivered.
- The hub delivers no input to a chat in yolo mode, and only the agents of the hub, an editor or the terminal (not conversations of a messaging platform) are peers.
- The plugin uses Hermes internals that are not a public plugin API (`tools.delegate_tool_registry`), so a Hermes release can break subagent messages; chat messages need only `prompt.submit`.
