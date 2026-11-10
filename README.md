# hermes-device

One server Hermes Agent owns every session. `hermes-device` lets that agent run its shell and file operations on your own PC, in a directory you choose, while the session itself stays on the server, where the web UI, Telegram and every other client can see it.

Pinned to Hermes Agent v0.22.7. Linux and macOS only: Windows is not supported, because device paths must be POSIX paths.

## Parts

One Go binary, `hermes-device`, with these subcommands:

| Subcommand | Runs on | What it does |
|---|---|---|
| `login` | your PC | Pairs the PC with hermes-hub and stores its device token |
| `run` | your PC | Daemon: runs the agent's commands on this PC (the exec leg), and serves the loopback splice that `chat` uses |
| `chat` | your PC | Opens your local Hermes TUI on a server session in the current directory |
| `acp` | your PC | ACP bridge for editors such as VS Code |
| `plugin install` | Hermes pod | Writes the embedded Hermes device plugin into a plugins directory |
| `acp-host` | Hermes pod | Runs one `hermes acp` per editor connection that the hub forwards |

`plugin/device` is the Hermes user plugin. It adds the `device` terminal backend, the `/device` command, its HTTP routes and the device WebSocket to the gateway. The binary embeds it.

The wire protocol between the PC, the hub and the pod is in [docs/protocol.md](docs/protocol.md).

## How it works

Your PC dials only the hub, over HTTPS and WebSockets. The hub checks the device token, finds the one backend the device was paired with, and forwards:

- the exec leg to the plugin in the gateway process, with a short-lived Ed25519 assertion that only the hub can sign;
- the gateway leg (for `chat`) to the gateway's session socket;
- the ACP leg (for editors) to `acp-host` in the Hermes pod.

A session is bound to a device and a directory. Every command of a bound session runs on that device, in that directory, and only inside the roots you allow in the config. The session's server-side working directory is a placeholder directory in the pod. A session that was ever bound never falls back to the pod shell, also after compression, branching or a subagent. Sessions that were never bound (web or Telegram sessions, for example) run where the plugin's `fallback` setting says: in the pod (`local`, the default), nowhere (`refuse`), or in a per-person sandbox pod (`device`, see [Agent sandbox](#agent-sandbox)).

### Pairing

```sh
hermes-device login --hub.url https://hub.example.com
```

`login` prints a link and a code. Open the link, sign in to the hub, check the code, name the device and confirm. `login` then stores the device token in the OS keyring (Secret Service on Linux, Keychain on macOS), or in a file with mode 0600 when no keyring is available. You can revoke a device on the hub at any time.

### The daemon

```sh
hermes-device run --hub.url https://hub.example.com --device.roots /home/alice/src
```

Keep `run` running, for example as a systemd user service. It reconnects by itself. A command whose working directory is outside `device.roots` is refused.

`run --no-splice` (or `gateway.splice: false`) runs only the exec leg: no loopback listener, no daemon state file and no whoami call, so `chat` cannot use it. Use it for a headless service device such as the sandbox pod. There, give the token with `hub.token` or `hub.token-file` (a file that holds only the token and that others cannot read) instead of `login` and the keyring.

### Chat

```sh
cd ~/src/app
hermes-device chat
```

`chat` starts your local Hermes Ink TUI, pointed at the `run` daemon's loopback splice. The splice creates the session on the server with this directory as the device directory, binds the session before the TUI sees it, and refuses calls that would act on the pod instead of your PC (pod shell commands, pod path completion, vault and profile changes). `/new` in the TUI starts another session in the same way.

The local Hermes must be the same release as the server's Hermes. `chat` checks this and refuses to start on a mismatch.

### VS Code and other ACP editors

Point the editor's ACP client at `hermes-device acp` as the agent command. Under Remote-SSH, the ACP extension and so `hermes-device acp` run on the SSH host, which is then the device. The bridge binds each session to the editor's folder, inlines the files you attach, and reconnects by itself, without the editor seeing the replayed history again.

Each editor window costs one `hermes acp` process in the pod.

## Server setup

1. Install the plugin before Hermes starts, for example from an init container that runs this image:

   ```sh
   hermes-device plugin install --dest /opt/data/hermes/plugins/device
   hermes plugins enable device
   ```

2. Configure the plugin in the profile's `config.yaml`:

   ```yaml
   terminal:
     backend: device
   plugins:
     entries:
       device:
         settings:
           backend_id: alice
           placeholder: /opt/data/hermes/device-placeholder
           device_write_roots: [/home/alice/src]
   ```

   Put the hub's Ed25519 public key in `HERMES_DEVICE_HUB_PUBLIC_KEY` (or set `hub_public_key_file`). Add the device roots that file tools may write to `HERMES_WRITE_SAFE_ROOT`. The plugin drops any of them that overlap a pod-writable path.

3. Run `hermes-device acp-host` in its own container of the Hermes pod, with the same `HERMES_HOME`, on a port that only the hub can reach.

docs/protocol.md section 8 lists every plugin setting.

## Agent sandbox

With `fallback: local`, sessions that are not bound to a PC run their commands in the Hermes gateway container, which can read the server's secrets, environment, state database and processes. `fallback: device` runs them on a sandbox device instead: one sandbox pod per person, in its own namespace, that runs `hermes-device run` as a service device of the hub.

```yaml
plugins:
  entries:
    device:
      settings:
        fallback: device
        fallback_device: d_sandbox_alice   # the sandbox's device id (a unique name also works)
        fallback_root: /work               # default
        device_write_roots: [/home/alice/src, /work]
```

Each session lineage gets its own directory, `<fallback_root>/<root session id>`, created on first use. Keep `terminal.cwd` an empty directory: Hermes still reads project context files such as `AGENTS.md` from it when it builds the prompt. File tools, edit approval and `@file` references read and write there. When the sandbox is offline, the command fails with a message; it never runs in the gateway container. `/device <device> <dir>` still moves a session to a PC. Add `fallback_root` to `HERMES_WRITE_SAFE_ROOT` if you set that variable.

The release builds the image `ghcr.io/usa-reddragon/hermes-device-sandbox` (Alpine, `linux/amd64`, `linux/arm64`, `linux/arm/v7`). It holds bash, coreutils, git, openssh-client, curl, ca-certificates, python3 with pip and venv, nodejs and npm, build-base, jq, ripgrep, fd, tar, gzip, zstd, unzip and less. It runs as uid and gid 568 with home `/work`, starts `hermes-device run` under tini, and works with a read-only root filesystem: `HOME`, `TMPDIR`, caches, `pip install` (user site) and `npm install -g` all go to the `/work` volume, and `/work/.local/bin` is on `PATH`, also in login shells. The image sets `HERMES_DEVICE_DEVICE_ROOTS=/work`, `HERMES_DEVICE_GATEWAY_SPLICE=false` and `HERMES_DEVICE_STORE_KEYRING=false`. The deployment sets:

| Setting | Value |
|---|---|
| `HERMES_DEVICE_HUB_URL` | The hub URL |
| `HERMES_DEVICE_HUB_TOKEN_FILE` | The mounted service device token, mode `0400` or `0440` (`HERMES_DEVICE_HUB_TOKEN` also works) |
| Volume | A persistent volume at `/work`, with `fsGroup: 568` |
| Network | Egress to the internet and the hub only; no ingress |

## Versions

Upgrade these together: the server's Hermes and every local Hermes that `chat` runs (same release), and the `hermes-device` image in the pod and the `hermes-device` binary on each PC (same release, protocol version 2).

## Configuration

The client and `acp-host` read `hermes-device/config.yaml` under the user config directory (`~/.config/hermes-device/config.yaml` on Linux), or the file given with `-c`, then environment variables, then flags. A `config.yaml` in the current directory is not read.

<!-- configulator:begin -->

| Key                            | Type           | Default                            | Environment                                  | Flag                             | Description                                                                                                                                                                                |
|--------------------------------|----------------|------------------------------------|----------------------------------------------|----------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `log-level`                    | string         | `info`                             | `HERMES_DEVICE_LOG_LEVEL`                    | `--log-level`                    | Log level: debug, info, warn or error                                                                                                                                                      |
| `ping-interval`                | string         | `25s`                              | `HERMES_DEVICE_PING_INTERVAL`                | `--ping-interval`                | Interval of WebSocket pings on every leg. Must be from 5s to 30s                                                                                                                           |
| `hub.url`                      | string         |                                    | `HERMES_DEVICE_HUB_URL`                      | `--hub.url`                      | Base URL of hermes-hub, for example https://hub.example.com. Needed by login, run, chat and acp                                                                                            |
| `hub.token`                    | string         |                                    | `HERMES_DEVICE_HUB_TOKEN`                    | `--hub.token`                    | Device token for run, instead of the one login stored, for a service device such as the sandbox pod. Wins over hub.token-file and the token store (secret)                                 |
| `hub.token-file`               | string         |                                    | `HERMES_DEVICE_HUB_TOKEN_FILE`               | `--hub.token-file`               | File holding only the device token for run, for example a mounted Kubernetes secret. Must not be readable by others. Wins over the token store                                             |
| `store.keyring`                | boolean        | `true`                             | `HERMES_DEVICE_STORE_KEYRING`                | `--store.keyring`                | Keep the device token in the OS keyring when one is available                                                                                                                              |
| `store.token-file`             | string         |                                    | `HERMES_DEVICE_STORE_TOKEN_FILE`             | `--store.token-file`             | File (mode 0600) for the device token when the keyring is off or unavailable. Defaults to token in the state directory                                                                     |
| `store.state-dir`              | string         |                                    | `HERMES_DEVICE_STORE_STATE_DIR`              | `--store.state-dir`              | Directory for the token file and the daemon's state file. Defaults to hermes-device in the user config directory                                                                           |
| `device.roots`                 | list of string |                                    | `HERMES_DEVICE_DEVICE_ROOTS`                 | `--device.roots`                 | Absolute directories commands may run in. A command whose working directory is outside all of them is refused. Needed by run                                                               |
| `device.tmp-dir`               | string         |                                    | `HERMES_DEVICE_DEVICE_TMP_DIR`               | `--device.tmp-dir`               | Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory                                                                                   |
| `device.shell`                 | string         | `bash`                             | `HERMES_DEVICE_DEVICE_SHELL`                 | `--device.shell`                 | Shell used to run commands                                                                                                                                                                 |
| `device.resume-buffer`         | integer        | `1048576`                          | `HERMES_DEVICE_DEVICE_RESUME_BUFFER`         | `--device.resume-buffer`         | Most output bytes per command that run keeps for a resume of the exec leg until the plugin confirms them. Older output of a command is dropped, and the plugin shows the gap               |
| `device.max-grace`             | string         | `10m`                              | `HERMES_DEVICE_DEVICE_MAX_GRACE`             | `--device.max-grace`             | Longest time run keeps commands running after the exec leg drops, waiting for a resume. The plugin's grace_seconds applies when it is shorter                                              |
| `gateway.listen`               | string         | `127.0.0.1:0`                      | `HERMES_DEVICE_GATEWAY_LISTEN`               | `--gateway.listen`               | Loopback address of the chat splice. Port 0 picks a free port                                                                                                                              |
| `gateway.splice`               | boolean        | `true`                             | `HERMES_DEVICE_GATEWAY_SPLICE`               | `--gateway.splice`               | Run the chat splice in run. false (or run --no-splice) runs only the exec leg: no listener, no daemon state file and no whoami call, for a headless service device such as the sandbox pod |
| `chat.hermes`                  | string         | `hermes`                           | `HERMES_DEVICE_CHAT_HERMES`                  | `--chat.hermes`                  | Local Hermes launcher, the same version as the server's Hermes                                                                                                                             |
| `acp-host.listen`              | string         | `:9130`                            | `HERMES_DEVICE_ACP_HOST_LISTEN`              | `--acp-host.listen`              | Address the ACP host listens on. Only hermes-hub may reach it                                                                                                                              |
| `acp-host.backend-id`          | string         |                                    | `HERMES_DEVICE_ACP_HOST_BACKEND_ID`          | `--acp-host.backend-id`          | This backend's id, the aud the hub puts in its assertions. Needed by acp-host                                                                                                              |
| `acp-host.hub-public-key`      | string         |                                    | `HERMES_DEVICE_ACP_HOST_HUB_PUBLIC_KEY`      | `--acp-host.hub-public-key`      | Hub Ed25519 public keys: PEM PUBLIC KEY blocks or base64 lines of the raw 32 bytes                                                                                                         |
| `acp-host.hub-public-key-file` | string         |                                    | `HERMES_DEVICE_ACP_HOST_HUB_PUBLIC_KEY_FILE` | `--acp-host.hub-public-key-file` | File holding the hub public keys, in the same format as hub-public-key                                                                                                                     |
| `acp-host.hermes`              | string         | `hermes`                           | `HERMES_DEVICE_ACP_HOST_HERMES`              | `--acp-host.hermes`              | Hermes launcher that the host runs as <hermes> acp for each connection                                                                                                                     |
| `acp-host.placeholder`         | string         |                                    | `HERMES_DEVICE_ACP_HOST_PLACEHOLDER`         | `--acp-host.placeholder`         | Server directory that stands in for the device directory in ACP session cwds. Must match the device plugin's placeholder. Needed by acp-host                                               |
| `acp-host.max-processes`       | integer        | `8`                                | `HERMES_DEVICE_ACP_HOST_MAX_PROCESSES`       | `--acp-host.max-processes`       | Most hermes acp processes the host runs at once                                                                                                                                            |
| `acp-host.bindings-file`       | string         |                                    | `HERMES_DEVICE_ACP_HOST_BINDINGS_FILE`       | `--acp-host.bindings-file`       | Bindings file the device plugin writes. Defaults to device/bindings.json in $HERMES_HOME                                                                                                   |
| `acp-host.gateway-health-url`  | string         | `http://127.0.0.1:9120/api/health` | `HERMES_DEVICE_ACP_HOST_GATEWAY_HEALTH_URL`  | `--acp-host.gateway-health-url`  | Health URL of the pod's Hermes gateway. The host starts hermes acp only while it answers, because hermes acp starts a gateway of its own when none runs. Empty turns the check off         |
| `reconnect.min-backoff`        | string         | `1s`                               | `HERMES_DEVICE_RECONNECT_MIN_BACKOFF`        | `--reconnect.min-backoff`        | First delay after a lost connection                                                                                                                                                        |
| `reconnect.max-backoff`        | string         | `30s`                              | `HERMES_DEVICE_RECONNECT_MAX_BACKOFF`        | `--reconnect.max-backoff`        | Longest delay between attempts                                                                                                                                                             |

<!-- configulator:end -->

## Tests

- Go: `go test -race ./...`. `protocol/contract_test.go` checks that the Go and Python wire constants agree.
- Plugin: `scripts/test-plugin.sh`, which runs the plugin tests in the local Hermes install's Python runtime, including contract tests against `HERMES_AGENT_SRC`.
