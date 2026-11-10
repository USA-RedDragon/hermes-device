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

A session is bound to a device and a directory. Every command of a bound session runs on that device, in that directory, and only inside the roots you allow in the config. The session's server-side working directory is a placeholder directory in the pod. A session that was ever bound never falls back to the pod shell, also after compression, branching or a subagent. Sessions that were never bound (web or Telegram sessions, for example) keep running in the pod.

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

## Versions

Upgrade these together: the server's Hermes and every local Hermes that `chat` runs (same release), and the `hermes-device` image in the pod and the `hermes-device` binary on each PC (same release, protocol version 2).

## Configuration

The client and `acp-host` read `config.yaml` in the current directory or in `hermes-device` under the user config directory, then environment variables, then flags.

<!-- configulator:begin -->
<!-- configulator:end -->

## Tests

- Go: `go test -race ./...`. Set `HERMES_AGENT_SRC` to a Hermes v0.22.7 checkout (default `~/.hermes/hermes-agent`) to run the contract tests against its source.
- Plugin: `scripts/test-plugin.sh`, which runs the plugin tests in the local Hermes install's Python runtime, including contract tests against `HERMES_AGENT_SRC`.
