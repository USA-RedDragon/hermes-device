# Talaria

Talaria is a self-hosted web app and Telegram bot for your own AI agent, built on
[Hermes Agent](https://github.com/NousResearch/hermes-agent).
You chat with the agent from a phone or a browser; it works in a sandbox on your server or on your own computers,
asks before it does anything risky, and can show you interactive pages, use Firefox and work in your editor.

Docs: https://usa-reddragon.github.io/talaria/

Talaria is built on Hermes Agent by Nous Research (MIT). Not affiliated with or endorsed by Nous Research.

## Install

Talaria includes its own agent, the part that talks to models, so you need nothing else. You connect models
once, from Talaria's own page (Connect a model) or with the agent's own setup command (`hermes model`, run inside
Talaria's container), and then pick them in the chat's model menu. Every install guide has that as a numbered
step right after you make your account.

Every install runs the hub (web app and Telegram bot, with its own accounts), the agent (Hermes Agent with the
Talaria patches), and a sandbox for the agent's commands. Secrets are made for you on the first start, and the
first person who opens Talaria makes the admin account.

| You have | Use | Guide |
| -------- | --- | ----- |
| A Linux host with Docker | Docker Compose, [`deploy/compose`](deploy/compose) | [Docker Compose](docs/deploy/compose.md) |
| A Kubernetes cluster | Helm chart [`deploy/charts/talaria`](deploy/charts/talaria) | [Kubernetes with Helm](docs/deploy/helm.md) |
| Unraid | All-in-one container, [`deploy/aio/unraid`](deploy/aio/unraid) | [Unraid](docs/deploy/unraid.md) |
| TrueNAS SCALE 24.10 or later | All-in-one container, [`deploy/aio/truenas`](deploy/aio/truenas) | [TrueNAS](docs/deploy/truenas.md) |

Quick start with Docker Compose:

```bash
mkdir talaria && cd talaria
curl -fsSL https://github.com/USA-RedDragon/talaria/archive/refs/heads/main.tar.gz \
  | tar -xz --strip-components=3 talaria-main/deploy/compose
cp .env.example .env
docker compose up -d --wait
docker compose logs init
```

Open the address it prints right away and make your account (the first visitor becomes the admin). Then click
**Connect a model** in the chat area and connect Ollama, an API key or a subscription ([details](docs/deploy/models.md)),
or do it from the server with the agent's own setup command:

```bash
docker compose exec -u hermes hermes hermes model
```

To look around without a provider, set `COMPOSE_PROFILES=demo` in `.env`, run `docker compose up -d --wait` again
and pick `talaria-demo` there. To let the agent work on your own computers, install the `talaria`
program on each one and [pair it](docs/start/add-a-computer.md).

## What is in this repository

| Path | What | Releases |
| ---- | ---- | -------- |
| `/` with [`cmd/`](cmd), [`internal/`](internal), [`main.go`](main.go) (this README from [Parts](#parts) on) | `talaria`, the program for your own computers | tags `vX.Y.Z`, the [latest release](https://github.com/USA-RedDragon/talaria/releases/latest), image `ghcr.io/usa-reddragon/talaria` |
| [`Dockerfile.sandbox`](Dockerfile.sandbox) | The agent sandbox: a container that runs `talaria run` as a service computer, so the agent has a shell before you pair your own | with `talaria`, image `ghcr.io/usa-reddragon/talaria-sandbox` |
| [`plugin/`](plugin) | The device plugin for Hermes, built into `talaria` and installed by it | with `talaria` |
| [`extension/`](extension) | The Firefox extension that lets the agent use your browser | with `talaria`, signed `.xpi` on the release |
| [`protocol/`](protocol), [`assertion/`](assertion) | The wire protocol between `talaria`, the plugin and the hub, and the hub's signed connection assertion | with `talaria` and the hub |
| [`hub/`](hub) | The hub: web app, Telegram bot, accounts, API | tags `hub/vX.Y.Z`, image `ghcr.io/usa-reddragon/talaria-hub` |
| [`hermes/`](hermes) | Talaria Hermes: Hermes Agent with the Talaria patches | tags `hermes/v<release>-p<n>`, image `ghcr.io/usa-reddragon/talaria-hermes` |
| [`plugins/`](plugins) | Hermes plugins Talaria uses: subagent routes, project memory | tags `plugins/vX.Y.Z`, tarball on the release |
| [`deploy/`](deploy) | The install kit: Docker Compose, Helm chart, all-in-one image for Unraid and TrueNAS, SearXNG settings for web search | tags `aio/vX.Y.Z`, image `ghcr.io/usa-reddragon/talaria-aio`; Compose and the Helm chart are used from `main` |
| [`docs/`](docs) | The docs site (with the pages in `hub/docs`) | https://usa-reddragon.github.io/talaria/ |
| [`scripts/`](scripts) | Maintainer scripts for tests and releases | not released |

# The talaria program

`talaria` lets the server's Hermes Agent run its shell and file operations on your own PC, in a directory you
choose, while the session itself stays on the server, where the web UI, Telegram and every other client can see it.
Pinned to Hermes Agent v0.23.1. Runs on Linux, macOS and Windows (commands in Git Bash, PowerShell or WSL). `talaria` was called `hermes-device` before release 0.19.0. See
[Renamed from hermes-device](#renamed-from-hermes-device).

- [Add a computer](docs/start/add-a-computer.md): install on Linux, macOS or Windows and pair it with your hub.
- [Chat from a terminal](docs/guides/terminal-chat.md), [let the agent use Firefox](docs/guides/browser-extension.md) and [use Talaria in an editor](docs/guides/editor-bridge.md).
- [Update, move or remove a computer](docs/guides/update-and-remove.md) a computer, and [troubleshooting](docs/guides/troubleshooting.md).
- Reference: [command line](docs/reference/cli.md) and [configuration](docs/reference/configuration.md).

### Parts

One Go binary, `talaria` (also `tal`, a link that setup makes next to it), with these subcommands:

| Subcommand | Runs on | What it does |
|---|---|---|
| `setup` | your PC | Sets the PC up in one step: installs the binary, pairs, writes the config, starts the service and connects Firefox |
| `update` | your PC | Installs the latest release for this OS and architecture and restarts the service |
| `login` | your PC | Pairs the PC with the Talaria hub and stores its device token |
| `run` | your PC | Daemon: runs the agent's commands on this PC (the exec leg), and serves the loopback splice that `chat` uses |
| `chat` | your PC | Opens your local Hermes TUI on a server session in the current directory |
| `acp` | your PC | ACP bridge for editors such as VS Code |
| `plugin install` | Hermes pod | Writes the embedded Hermes device plugin into a plugins directory |
| `acp-host` | Hermes pod | Runs one `hermes acp` per editor connection that the hub forwards |

`plugin/device` is the Hermes user plugin. It adds the `device` terminal backend, the `/device` command, its HTTP routes and the device WebSocket to the gateway. The binary embeds it.

The wire protocol between the PC, the hub and the pod is in [docs/protocol.md](docs/protocol.md).

### How it works

Your PC dials only the hub, over HTTPS and WebSockets. The hub checks the device token, finds the one backend the device was paired with, and forwards:

- the exec leg to the plugin in the gateway process, with a short-lived Ed25519 assertion that only the hub can sign;
- the gateway leg (for `chat`) to the gateway's session socket;
- the ACP leg (for editors) to `acp-host` in the Hermes pod.

A session is bound to a device and a directory. Every command of a bound session runs on that device, in that directory, and only inside the roots you allow in the config. The session's server-side working directory is a placeholder directory in the pod. A session that was ever bound never falls back to the pod shell, also after compression, branching or a subagent. Sessions that were never bound (web or Telegram sessions, for example) run where the plugin's `fallback` setting says: in the pod (`local`, the default), nowhere (`refuse`), or in a per-person sandbox pod (`device`, see [Agent sandbox](#agent-sandbox)).

#### Setup

Download the archive `talaria_<version>_<os>_<arch>.tar.gz` for your OS and architecture from the [latest release](https://github.com/USA-RedDragon/talaria/releases/latest), check it against `talaria_<version>_checksums.txt` (`sha256sum --ignore-missing -c talaria_<version>_checksums.txt`, or `shasum -a 256 --ignore-missing -c ...` on macOS), unpack it and run:

```sh
./talaria setup https://hub.example.com
```

The hub URL is required: a release does not know any hub, so `./talaria setup` without it stops with `give the hub URL`. `setup` does each of these steps, and only the ones that are not done yet, so you can run it again at any time:

1. It copies itself to `~/.talaria/bin/talaria`. It refuses to replace a newer release there without `--force`.
2. It pairs the computer with the hub, as `login` does: it prints a link and a code, and opens the link in your browser when it can. Sign in to the hub, check the code, name the device and confirm. The device token goes into the OS keyring (Secret Service on Linux, Keychain on macOS), or into a file with mode 0600 when no keyring is available. A stored token that the hub still accepts is kept.
3. It writes `~/.config/talaria/config.yaml` (or the file `-c` names) when the file does not exist, with `hub.url`, `device.roots` set to your home directory, and `browser.enabled: true` when it finds Firefox. It never changes an existing config file unless you pass `--overwrite-config`, and then it backs the old one up first.
4. With browser control on, it registers the binary as Firefox's native messaging host (as `talaria browser install` does).
5. It checks that the install directory (`~/.talaria/bin`, or `--bin-dir`) is on `PATH` for your login shell, as a terminal starts it, and for `bash -l`, because agent commands run with `bash -l`. When a shell lacks it, setup asks before it adds the directory to `~/.config/shell/env.sh` and sources that file from bash's startup file (`~/.bash_profile`, `~/.bash_login` or `~/.profile`, whichever bash reads), from `~/.zshrc` for zsh and from `~/.bashrc` for bash. A startup file that already sources `env.sh` is left as it is. For fish, it writes `~/.config/fish/conf.d/talaria.fish` instead. For another login shell, it changes only bash's files and tells you to add the directory to your shell's startup file yourself. Then, when your login shell is zsh or fish, it compares that shell's `PATH` with the one `bash -l` gets. When bash lacks directories, it lists them and asks before it adds them to `~/.config/shell/env.sh` and sources that file in the same way. It backs each file up first (`<file>.talaria-<time>.bak`), changes nothing without a yes, and only lists what to do when it does not run in a terminal. A directory that is already on `PATH` is not added again.
6. It installs and starts the daemon as a service that starts at login: the systemd user unit `~/.config/systemd/user/talaria.service` on Linux, the launchd agent `~/Library/LaunchAgents/io.github.usa-reddragon.talaria.plist` on macOS. The unit has `OOMPolicy=continue`, so when the kernel kills a process of the service for memory, the service keeps running. It restarts the service when it changed the binary, the config or the unit. A unit that setup wrote is replaced by the current one on every run, so run `talaria setup` again to get the settings of a newer release; `talaria update` does not change the unit. A unit you edited is backed up first, and so is a unit that the move from hermes-device wrote (see [Renamed from hermes-device](#renamed-from-hermes-device)), which setup replaces like its own. Drop-in files in `talaria.service.d` stay as they are. It keeps a unit that it did not write when that unit has the same settings, and replaces another one only with `--force`, after a backup.
7. When the Talaria extension is not installed in Firefox, it opens Firefox at the signed `.xpi` of the matching release. Choose "Continue to Installation", then "Add". Then it waits (`--wait`, 10 minutes by default) for the extension to connect to the daemon and reports when it has.
8. It prints what it did and what is left for you. When a step fails, the summary ends with the error instead of "Nothing is left to do", and setup exits with an error; the steps after the failed one did not run.

`--no-service`, `--no-browser` and `--no-shell-check` skip those steps. `--bin-dir` installs the binary elsewhere.

`setup --uninstall` reverses what setup did, from the record it keeps in `setup.json` in the state directory: it stops and disables the service and removes its file, removes the native messaging host and the device token, removes the config file unless you changed it since, puts back a config or service file that setup replaced, takes its lines out of your shell files (the backups stay), and removes the binary. When it cannot stop the service, it keeps the binary, the config and the token, so you can stop the service and run it again. Remove the extension in Firefox and the computer on the hub (Settings, Computers) yourself.

`login` and `run` still work on their own, for example for a service device or a computer without a desktop.

#### Updates

```sh
talaria update
```

`update` downloads the latest release's archive for this OS and architecture, checks its SHA-256 against the release's checksum file, verifies the archive's GitHub artifact attestation (build provenance signed with Sigstore by this repository's release workflow at the release tag), checks that the new binary runs here and reports the release's version, replaces `~/.talaria/bin/talaria` (or this binary when that does not exist) and restarts the service. It refuses a downgrade and replacing a development build without `--force`. `talaria update 0.9.0` installs that release. It refuses a release whose attestation does not verify, and every release before v<!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const -->, which has none. `--insecure-skip-attestation` installs without the attestation check; that is unsafe and meant for emergencies only. `update --check` runs every check and installs nothing. When you run `update` in a terminal and `~/.talaria/bin` is not on your `PATH`, it prints a line that says to run `~/.talaria/bin/talaria setup` to add it. [docs/releases.md](docs/releases.md) says what is attested, what `update` checks and trusts, and how to verify a file by hand with `gh attestation verify <file> --repo USA-RedDragon/talaria`. Firefox updates the extension by itself.

The daemon also updates itself. When the talaria service runs `run` (the service that `setup` installs) and `update.auto` is on (the default), `run` looks for a new release shortly after it starts and then every `update.interval` (default 6h, moved by up to 10% each time so that computers do not check at the same moment). It installs a newer stable release with the same steps as `update`, all checks included; there is no way to skip them. It never installs a pre-release, an older release or the same release. Then it waits until no command, browser operation, MCP call or `chat` runs on this computer, for up to 30 minutes, and exits so that the service manager starts the new binary. When work is still running after 30 minutes, it tries again at the next check. A failed check or install is logged as a warning and waits for the next check. It does not update itself when you run `run` by hand, when it does not run `~/.talaria/bin/talaria`, for a development build, or on a service device (`hub.token` or `hub.token-file`); the sandbox and device images also set `update.auto` to false, because their images are updated by redeploying them.

Commands that you run (`chat`, `acp`, `setup`, `login` and `browser`) look for a new release at most once a day, in the background, and never wait for it. When one exists, they print one line on stderr that names it and says whether the service installs it by itself. Set `TALARIA_NO_UPDATE_NOTICE=1` to turn the line off.

Both share one cache of the release lookup, `~/.cache/talaria/release.json` on Linux and `~/Library/Caches/talaria/release.json` on macOS. They ask the GitHub API at most once an hour (the daemon) or once a day (the CLI), with the cached ETag, which keeps them far below GitHub's limit of 60 requests per hour per IP address without a token. When GitHub answers that the limit is reached, they stop asking until the time it gives.

#### The daemon

The service runs `talaria run -c ~/.config/talaria/config.yaml`. To run it yourself:

```sh
talaria run --hub.url https://hub.example.com --device.roots /home/alice/src
```

It reconnects by itself. A command whose working directory is outside `device.roots` (by default your home directory) is refused. On Linux, `journalctl --user -u talaria -f` shows its logs; on macOS, `~/Library/Logs/talaria.log`.

On Linux with a systemd user manager, `run` starts each agent command in a systemd scope of its own (`systemd-run --user --scope --expand-environment=no`, unit `talaria-cmd-<random>.scope`; the last option keeps systemd 258 and later from expanding `${...}` in the command before the shell sees it, and is left out on systemd before 254, which has no such option) with the `MemoryMax` that `device.command-memory-max` sets. By default that is 75% of the service's `MemoryMax` (1.5 GiB with the `MemoryMax=2G` that setup writes), or 4 GiB when the daemon's cgroup has no memory limit. When a command uses more, the kernel kills that command (all its processes) and nothing else: the daemon, its other commands, browser calls and legs keep running. The agent sees `[killed: out of memory (limit 1.5 GiB). ...]` after the command's output, and the daemon logs a warning. The scopes are outside the service's cgroup, so the service's `MemoryMax` no longer counts commands. Each scope has the service's `TasksMax` and is `PartOf` the service, so stopping or restarting the service stops the commands, as before. The daemon logs at start whether commands get a scope. Without `systemd-run` or a user manager (for example in the sandbox image), or with `device.command-memory-max: off`, commands run in the daemon's cgroup as before. macOS has no cheap per-process memory cap that fits: XNU does not enforce `RLIMIT_RSS`, and `RLIMIT_AS` limits address space, which breaks programs such as Node.js that reserve large address ranges. So on macOS commands have no cap of their own.

`run --no-splice` (or `gateway.splice: false`) runs only the exec leg: no loopback listener, no daemon state file and no whoami call, so `chat` cannot use it. Use it for a headless service device such as the sandbox pod. There, give the token with `hub.token` or `hub.token-file` (a file that holds only the token and that others cannot read) instead of `login` and the keyring.

#### Chat

```sh
cd ~/src/app
talaria chat
```

`chat` starts your local Hermes Ink TUI, pointed at the `run` daemon's loopback splice. The splice creates the session on the server with this directory as the device directory, binds the session before the TUI sees it, and refuses calls that would act on the pod instead of your PC (pod shell commands, pod path completion, vault and profile changes). `/new` in the TUI starts another session in the same way.

To continue a session, pass Hermes' resume flags after `--`: `talaria chat -- -c` continues the most recent session bound to this PC, and `talaria chat -- -c "fix the parser"` or `-- --resume <id>` continues the session with that title or id. `chat` looks the session up on the server, not in the local Hermes session store, and the resumed session moves to the current directory. `/resume <title>` in the TUI works the same way.

On Linux, `chat` runs the exact Hermes build the server runs:

- The plugin reports the container image of the server's Hermes, pinned by digest, in the exec leg's welcome (Server setup, step 4). `run` records it in its state file and fetches that build in the background when it changes, after each reconnect and after each restart, an automatic update included. When `chat` starts before the build is on this PC, it fetches it first and shows the progress.
- `run` pulls the image by its digest from the reported registry, without credentials. When the reference is a pull-through mirror path such as `registry.example.com/docker.io/nousresearch/hermes-agent` that names a well-known public registry (Docker Hub, GHCR, Quay and a few others) and the mirror fails (it is unreachable or wants a login), it pulls the same digest from that registry, here Docker Hub. It checks every manifest and layer against its SHA-256 digest while they stream and runs nothing that does not match. It keeps only Hermes from `/opt/hermes` with its bundled Python and Node, about 1.4 GB on disk, in `~/.cache/talaria/hermes` (`chat.managed-dir`).
- `run` keeps the current build and the one used most recently before it. It never removes a build that a running chat uses.
- The managed Hermes has a home of its own: `HOME`, `HERMES_HOME` and the XDG directories point into `home` in that directory, and `chat` removes the variables that point Python, Node or Hermes at other installs (`PYTHONPATH`, `VIRTUAL_ENV`, `NODE_OPTIONS`, `npm_config_*`, `NVM_*`, `LD_LIBRARY_PATH`, `HERMES_*` and others). Its `PATH` starts with the build's own directories and leaves out the directories in your home. It does not read or write `~/.hermes`, `~/.local` or nvm. It needs glibc 2.28 or newer and the C++ runtime (`libstdc++`, `libgcc_s` and `libatomic`), which desktop distributions have.

The server can already run any command on this PC through the exec leg, so an image that it names is as trusted as the server. The digest makes sure that the build is exactly that image. Hermes publishes no signed release files or image signatures that `chat` could check in addition.

`talaria chat --status` shows the server's image and the builds on this PC. `talaria chat --refresh` fetches and verifies the server's build again, replaces the copy on this PC and removes old builds.

With `chat.managed` set to `false`, on macOS, when the server reports no image, or when the image has no build for this PC's architecture (the Hermes image has amd64 and arm64), `chat` runs `chat.hermes` (default `hermes` on `PATH`) with your own Hermes home. That Hermes must be the same release as the server's: `chat` checks this and refuses to start on a mismatch (`--allow-version-mismatch` starts it anyway).

#### VS Code and other ACP editors

Point the editor's ACP client at `talaria acp` as the agent command. Under Remote-SSH, the ACP extension and so `talaria acp` run on the SSH host, which is then the device. The bridge binds each new session to the editor's folder, inlines the files you attach, and reconnects by itself, without the editor seeing the replayed history again.

The editor's chat list shows the chats bound to this computer in the editor's folder, and the chats that were never bound to any computer, such as web chats, when the server lists them (Hermes setting `sessions.acp_list_sources`). It never shows chats bound to another computer. Opening an existing chat never moves it. A chat bound to this computer and the editor's folder opens and runs as usual. A chat bound to another folder of this computer opens with its history and a notice that names its folder. A chat that is not bound to this computer, such as a web or Telegram chat that runs in the sandbox, opens empty with a notice. Neither runs from the editor: a message you send gets a notice back and is not sent. To move the chat to the editor's folder on this computer, send `/move-here` in it (the editor also offers it as a slash command). A chat bound to another computer does not open; the editor shows an error, and only `/device` in the chat can move it. A moved chat reloads after a reconnect without asking again.

Each editor window costs one `hermes acp` process in the pod.

#### Firefox

The agent can also use your own Firefox, with your logins, in its own "Hermes" tab group. It sees and acts only on tabs in that group. It asks you the first time it opens a site or submits a form on a site in a session, and every time it acts on a page with password, payment or one-time code fields.

`talaria setup` turns it on when it finds Firefox (section [Setup](#setup), steps 3, 4 and 7). Add `browser.allow-hosts` to the config for LAN or loopback hosts the agent may open; all others are blocked in its tabs. To turn it on by hand instead:

1. Set `browser.enabled: true` in the config.
2. Run `talaria browser install`. It registers this binary as Firefox's native messaging host for the Talaria extension, for your user only.
3. Restart `talaria run`.
4. Install the extension: download `talaria-firefox-<version>.xpi` from the [latest release](https://github.com/USA-RedDragon/talaria/releases/latest), then in Firefox open `about:addons`, choose the gear menu, "Install Add-on From File..." and pick the file. Mozilla signs it, so it stays installed, and Firefox updates it from later releases. The extension's version can be older than the daemon's (a v0.25.3 release ships `talaria-firefox-0.24.0.xpi`): it is bumped only when the extension itself changes, and a release whose extension did not change reuses the last signed file. Use the `.xpi` the latest release has, whatever its number. For development, `talaria browser extension --out DIR` writes the files to load as a temporary add-on (docs/browser.md section 6.1).
   The Talaria button in Firefox's toolbar then shows a green "on" badge, and its tooltip reads "Talaria: connected".

The browser needs no sign-in of its own. It is authorized by this computer: the extension talks only to the local daemon, the daemon's socket is private to your OS user, and the daemon is paired to your hub account, so only your own sessions reach it. Any program you run as the same OS user can also reach it (docs/browser.md section 13).

The agent's tools are `browser_status`, `browser_use_device`, `browser_tabs`, `browser_navigate`, `browser_back`, `browser_snapshot`, `browser_click`, `browser_type`, `browser_vision` and `browser_screenshot` (a screenshot the user sees in the Talaria hub). A session bound to this computer uses its Firefox. A session that runs in the sandbox, such as a web chat or a scheduled job, uses the Firefox of your one computer with browser control, or the one `browser_use_device` picks, when the plugin setting `user` names you as the backend's one user. A scheduled job keeps the browser computer of the chat that created it: its commands run in the sandbox and its browser steps run here, also when Firefox was closed (docs/browser.md section 3.2). To stop it, close the tab group, disable the extension, stop `run`, or remove the computer on the hub. [docs/browser.md](docs/browser.md) has the design, the approval rules and the security model.

#### MCP servers

The agent can use MCP servers that run on this computer, for tools that must work on its own files and programs (for example a reverse engineering server). List them in the config file under `mcp.servers`; only servers listed there can run, and the plugin cannot add one. `run` starts a server over stdio when the agent first uses it, keeps it running while it is used, stops it after `mcp.idle-timeout` without requests (it loses its state, such as open files; a server's own `idle-timeout` overrides this, and `0` keeps it running), starts it again after it exits (with a wait that grows while it keeps failing), and stops it when `run` stops. The server's stderr goes to the daemon log.

```yaml
mcp:
  servers:
    rea:
      command: /bin/bash
      args: ["-lc", "exec npx -y rea-agents@6.7.0 mcp"]
      idle-timeout: 12h
      env:
        HOPPER_LAUNCHER_PATH: /opt/hopper/bin/Hopper
```

The server gets the daemon's environment (without the device token) plus `env`. A systemd user service often has a shorter `PATH` than your shell. Starting the server through a login shell (`bash -lc`, as above) gives it the `PATH` your profile sets, so it keeps working when a version manager such as nvm moves `node`; otherwise give `command` as an absolute path and set `PATH` in `env`. `idle-timeout` keeps a server with costly state (such as opened binaries) running longer than `mcp.idle-timeout`. The server runs as you, in the daemon's cgroup (with its `MemoryMax` and `TasksMax`), and is not limited to `device.roots`.

The agent's tools are `mcp_device_servers`, `mcp_device_tools` (list or search a server's tools, or show one tool's schema) and `mcp_device_call`, for sessions bound to this computer. It asks you before it runs a tool that its server does not mark read-only, once per tool for the session or always (plugin setting `mcp_approval`; `always` asks for every tool, since the read-only mark is the server's own claim). Tool results are untrusted data. [docs/protocol.md](docs/protocol.md) sections 4.9 and 8.11 have the details.

#### Agent messages

The plugin's `device-agents` tools let the user's agents message each other through the hub: `agent_peers` lists the user's other chats (with the computer and folder of each) and the subagents of the caller's own delegation tree, `agent_send` sends one of them a message, and `agent_set_name` gives a chat a short name. It does not matter which computer a chat runs on. A message to an idle chat starts a turn, one to a busy chat waits for its current turn, and a running subagent or parent gets it with its next tool result. The target sees it marked as from another agent, never as the user's words, and it never approves anything. The hub limits how many messages agents send and how long a chain of answers can be. [docs/protocol.md](docs/protocol.md) section 12 has the details.

#### Claude Code usage

The daemon counts the tokens that Claude Code used on the computer and sends the counts to the hub, which shows them on its Usage page with every computer and Hermes together. It reads the transcripts in `~/.claude` (or each directory in `$CLAUDE_CONFIG_DIR`) and in `usage.claude-dirs`, and only these parts of them: the time, model, the four token counts, the account and organization ids, whether the line is a subagent's (sidechain), and the base name of the session's folder, such as `app` for `/home/alice/src/app`. It reads the signed-in account's ids and email from `.claude.json`, so the hub can name the account, and keeps a list of when the signed-in account switched (the account timeline file in the state directory, never a token). Claude Code does not record which account a request used, so each request counts for the account that was signed in at its time; requests from before the list began count for the account of their session's start. It never reads or sends what was said, tool output, file contents or full paths. It reads only what was added since its last look, every 30 seconds, and keeps its place in `claude-usage.gob` in the state directory, so the first scan of a large history is the only long one.

In the Hermes pod, `acp-host` counts the same way in each per-account directory under `usage.hermes-accounts-dir` (and `usage.claude-dirs`, but not `~/.claude`), named by the directory's label, and the hub asks it for the counts. The hub shows them as the computer "Hermes".

`usage.enabled: false` turns counting off. It is on by default, except for `run` in a container. [docs/protocol.md](docs/protocol.md) section 13 lists exactly what is read and what is sent.

### Renamed from hermes-device

Release 0.19.0 renamed the program from `hermes-device` to `talaria`, and the repository was renamed to `USA-RedDragon/talaria` after 0.19.1. The first release after 0.25.4 renames the rest:

- The Go module is `github.com/USA-RedDragon/talaria` (it was `github.com/USA-RedDragon/hermes-device`).
- The container images are `ghcr.io/usa-reddragon/talaria` and `ghcr.io/usa-reddragon/talaria-sandbox`, with the binary as `/talaria` and `/usr/local/bin/talaria` (the sandbox keeps a `hermes-device` link to it). Releases up to 0.25.4 stay at `ghcr.io/usa-reddragon/hermes-device` and `hermes-device-sandbox`; later releases are not published there, so move image references to the new names.
- The blocks that `setup` writes to shell files are marked `talaria setup`. Blocks marked `hermes-device setup` that older releases wrote stay in place, count as the same block, and `setup --uninstall` removes them.
- The plugin reads the hub public keys from `TALARIA_HUB_PUBLIC_KEY`; when it is unset, from `HERMES_DEVICE_HUB_PUBLIC_KEY`, with a deprecation warning. Its messages start with `talaria:`.

Some names stay because deployed hubs, plugins and Firefox depend on them: the Firefox extension's id `browser@hermes-device` (signed by addons.mozilla.org), the assertion header `X-Hermes-Device-Assertion` and type `hermes-device-assertion`, the `hermes-device:<id>` session list audience, the `hermes_device_*` error reasons and `hermes-device-*` request ids ([docs/protocol.md](docs/protocol.md)), the screenshot folder `cache/screenshots/hermes-device` that the hub shows, and the plugin's Python module names.

Releases before 0.19.3 accept only release attestations signed in a repository named `hermes-device`, so they cannot install releases built after the repository rename by themselves: install 0.19.3 or later by hand once (verify the archive with `gh attestation verify <archive> --repo USA-RedDragon/talaria` and its checksum first). From 0.19.3 on, `update` and auto-update accept both names for the same repository ID.

| What | Before 0.19.0 | From 0.19.0 |
|---|---|---|
| Binary | `~/.hermes-device/bin/hermes-device` | `~/.talaria/bin/talaria`, with `tal` linked to it |
| Config and state directory | `~/.config/hermes-device` | `~/.config/talaria` |
| Cache directory | `~/.cache/hermes-device` | `~/.cache/talaria` |
| systemd user unit | `hermes-device.service` | `talaria.service` |
| launchd agent | `io.github.usa-reddragon.hermes-device` | `io.github.usa-reddragon.talaria` |
| Environment variables | `HERMES_DEVICE_*` | `TALARIA_*` |
| Keyring entry | service `hermes-device` | service `talaria` |
| Firefox native messaging host | `hermes_device` | `talaria` |
| Release archives | `hermes-device_<version>_<os>_<arch>.tar.gz` | `talaria_<version>_<os>_<arch>.tar.gz` |

The first `talaria` command on a computer that has the old names moves them, and logs each move:

- It moves the binary and leaves `~/.hermes-device/bin/hermes-device` as a link to it, so editor settings and scripts that use the old path keep working. It does not change your shell files, because it often runs without a terminal to ask in (under the service, for example) and setup changes them only after a yes. When `~/.talaria/bin` is not on `PATH`, it logs a note to run `~/.talaria/bin/talaria setup`, which offers to add it.
- It moves the config and cache directories, and changes paths in `config.yaml` and in setup's record that named the old directory.
- It writes the new unit or launchd agent from the old one (with your changes to it, such as `MemoryMax`, and the unit's drop-in directory, and with `OOMPolicy=continue` when it has no `OOMPolicy`), removes the old one, and enables and starts the new one in its place. The new unit has `Conflicts=hermes-device.service`, so starting it stops the old one first, also when the old one runs the process that migrates. A file that setup did not write gets a comment that marks it as moved, so that a later `talaria setup` backs it up and replaces it with its own file instead of refusing it.
- It registers the `talaria` native messaging host and points the old `hermes_device` host at the new binary too, because Firefox updates the extension on its own schedule. The extension tries `talaria` first, then `hermes_device`.
- It copies the device token in the keyring to the `talaria` entry the first time it reads it, and keeps the old entry for older binaries.

A file that is already at the new place wins; the old one is left where it is and the log says so. `run --check-config`, `acp-host` and `plugin install` move nothing, and neither does a `talaria` that is not the installed binary (one you downloaded and run from elsewhere), except `setup`. While the old service runs, other commands (`chat` or an editor's `acp`, for example) move the files but leave the service alone, so that work the old daemon runs is not stopped; the service moves when the daemon restarts, for example after its automatic update. The auto-update of 0.17.0 and 0.18.0 installs 0.19.0 by itself (the release still has `hermes-device_*` archives for it), and the new daemon then migrates the computer.

`TALARIA_*` environment variables win; an unset one falls back to the `HERMES_DEVICE_*` one, with a deprecation warning.

Releases still publish, for the auto-update of older installs, the `hermes-device_<version>_<os>_<arch>.tar.gz` archives (with a `hermes-device` binary; 0.18.0 and older look only for these) and a copy of the checksum file as `hermes-device_<version>_checksums.txt` (0.25.2 and older read only that one). 0.25.3 and later read the `talaria_*` files. A later release, announced at least one release ahead, stops reading the old variables, stops publishing the `hermes-device_*` files and no longer keeps the old host manifest; installs older than 0.25.3 then need one `talaria update` from a newer binary, or a new install.

### Server setup

1. Install the plugin before Hermes starts, for example from an init container that runs this image:

   ```sh
   talaria plugin install --dest /opt/data/hermes/plugins/device
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

   Put the hub's Ed25519 public key in `TALARIA_HUB_PUBLIC_KEY` (or set `hub_public_key_file`). Add the device roots that file tools may write to `HERMES_WRITE_SAFE_ROOT`. The plugin drops any of them that overlap a pod-writable path.

3. Run `talaria acp-host` in its own container of the Hermes pod, with the same `HERMES_HOME`, on a port that only the hub can reach.

4. Set `HERMES_IMAGE_REPOSITORY` and `HERMES_IMAGE_TAG` in the Hermes gateway container to the repository and the tag of its own image, with the digest in the tag, for example `registry.example.com/docker.io/nousresearch/hermes-agent` and `v0.23.1@sha256:<digest>`. Take both from the same values as the container's `image`, so that they cannot drift apart. The plugin reports the image to devices, and `chat` runs that exact build (Chat). Without them `chat` runs the device's own Hermes. When the image is a patched build of an upstream image that devices cannot pull (for example a private one), also set `HERMES_BASE_IMAGE` to the upstream image, pinned by digest, best in the patched image itself (`ENV HERMES_BASE_IMAGE=...` next to its `FROM`). The plugin reports it, and a device that cannot fetch the patched image runs the upstream build's TUI instead: patches that change only the server do not change the TUI.

docs/protocol.md section 8 lists every plugin setting.

### Agent sandbox

With `fallback: local`, sessions that are not bound to a PC run their commands in the Hermes gateway container, which can read the server's secrets, environment, state database and processes. `fallback: device` runs them on a sandbox device instead: one sandbox pod per person, in its own namespace, that runs `talaria run` as a service device of the hub.

```yaml
plugins:
  entries:
    device:
      settings:
        fallback: device
        fallback_device: d_sandbox_alice   # the sandbox's device id (a unique name also works)
        fallback_root: /work               # default
        user: alice                        # the one hub user of this backend: sandbox chats and jobs use her Firefox
        device_write_roots: [/home/alice/src, /work]
```

Each session lineage gets its own directory, `<fallback_root>/<root session id>`, created on first use. Keep `terminal.cwd` an empty directory: Hermes still reads project context files such as `AGENTS.md` from it when it builds the prompt. File tools, edit approval and `@file` references read and write there. When the sandbox is offline, the command fails with a message; it never runs in the gateway container. `/device <device> <dir>` still moves a session to a PC. Add `fallback_root` to `HERMES_WRITE_SAFE_ROOT` if you set that variable.

The release builds the image `ghcr.io/usa-reddragon/talaria-sandbox` (Alpine, `linux/amd64`, `linux/arm64`, `linux/arm/v7`). It holds bash, coreutils, git, openssh-client, curl, ca-certificates, python3 with pip and venv, nodejs and npm, build-base, jq, ripgrep, fd, tar, gzip, zstd, unzip and less. It runs as uid and gid 568 with home `/work`, starts `talaria run` under tini, and works with a read-only root filesystem: `HOME`, `TMPDIR`, caches, `pip install` (user site) and `npm install -g` all go to the `/work` volume, and `/work/.local/bin` is on `PATH`, also in login shells. The image sets `TALARIA_DEVICE_ROOTS=/work`, `TALARIA_GATEWAY_SPLICE=false`, `TALARIA_STORE_KEYRING=false` and `TALARIA_UPDATE_AUTO=false`. The deployment sets:

| Setting | Value |
|---|---|
| `TALARIA_HUB_URL` | The hub URL |
| `TALARIA_HUB_TOKEN_FILE` | The mounted service device token, mode `0400` or `0440` (`TALARIA_HUB_TOKEN` also works) |
| Volume | A persistent volume at `/work`, with `fsGroup: 568` |
| Network | Egress to the internet and the hub only; no ingress |

### Versions

On Linux, `chat` runs the server's own Hermes build, taken from the image that the plugin reports (Chat). Otherwise the local Hermes that `chat` runs must be the same release as the server's Hermes.

The `talaria` image in the pod and the `talaria` binary on each PC should be the same release. Release 0.3.0 negotiates the exec leg protocol with its peer (versions 2 and 3), so a 0.2.0 binary still works with a 0.3.0 plugin and the other way round, but only version 3 keeps commands alive across a drop. Upgrade the pod first, then the PCs. When the two sides share no version, the leg closes with code 4426 and a message that names both versions, and `run` stops and prints it.

#### Reconnects and restarts

With protocol version 3, a command keeps running when the exec leg drops, for example when the hub restarts or the network goes away. The device keeps the command's output that the plugin has not confirmed (`device.resume-buffer`, 1 MiB per command), and the plugin keeps the tool call waiting, both for the plugin's `grace_seconds` (default 120). When the device reconnects in time, it sends the missing output and the exit code, and the agent sees the command finish as if nothing happened.

A restart of the Hermes gateway (also a rollout) cannot keep commands: they belong to the gateway process. The plugin fails them at once with exit 255 and a note, and the device kills them as soon as it sees the gateway stop, or at the latest when it reconnects to the new gateway. The exception is a graceful restart with `device` in Hermes's `terminal.process_handoff_backends`: commands already moved to the background go to the next gateway and keep running if it starts within `grace_seconds` (set it to `device.max-grace`, `600` for the default), see docs/protocol.md section 4.7. A restart of `run` also loses its commands: they fail with exit 255 when it reconnects. An automatic update restarts `run` only when it holds no commands, not even an exit the plugin has not confirmed yet.

### Configuration

The client and `acp-host` read `talaria/config.yaml` under the user config directory (`~/.config/talaria/config.yaml` on Linux), or the file given with `-c`, then environment variables, then flags. A `config.yaml` in the current directory is not read. `talaria setup` writes the file when it does not exist.

<!-- configulator:begin -->

| Key                              | Type           | Default                            | Environment                            | Flag                             | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
|----------------------------------|----------------|------------------------------------|----------------------------------------|----------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `log-level`                      | string         | `info`                             | `TALARIA_LOG_LEVEL`                    | `--log-level`                    | Log level: debug, info, warn or error                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `ping-interval`                  | string         | `25s`                              | `TALARIA_PING_INTERVAL`                | `--ping-interval`                | Interval of WebSocket pings on every leg. Must be from 5s to 30s                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `hub.url`                        | string         |                                    | `TALARIA_HUB_URL`                      | `--hub.url`                      | Base URL of the Talaria hub, for example https://hub.example.com. Needed by login, run, chat and acp                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `hub.token`                      | string         |                                    | `TALARIA_HUB_TOKEN`                    | `--hub.token`                    | Device token for run, instead of the one login stored, for a service device such as the sandbox pod. Wins over hub.token-file and the token store (secret)                                                                                                                                                                                                                                                                                                                                                           |
| `hub.token-file`                 | string         |                                    | `TALARIA_HUB_TOKEN_FILE`               | `--hub.token-file`               | File holding only the device token for run, for example a mounted Kubernetes secret. Must not be readable by others. Wins over the token store                                                                                                                                                                                                                                                                                                                                                                       |
| `store.keyring`                  | boolean        | `true`                             | `TALARIA_STORE_KEYRING`                | `--store.keyring`                | Keep the device token in the OS keyring when one is available                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `store.token-file`               | string         |                                    | `TALARIA_STORE_TOKEN_FILE`             | `--store.token-file`             | File (mode 0600) for the device token when the keyring is off or unavailable. Defaults to token in the state directory                                                                                                                                                                                                                                                                                                                                                                                               |
| `store.state-dir`                | string         |                                    | `TALARIA_STORE_STATE_DIR`              | `--store.state-dir`              | Directory for the token file and the daemon's state file. Defaults to talaria in the user config directory                                                                                                                                                                                                                                                                                                                                                                                                           |
| `device.roots`                   | list of string |                                    | `TALARIA_DEVICE_ROOTS`                 | `--device.roots`                 | Absolute directories commands may run in. A command whose working directory is outside all of them is refused. Defaults to the home directory                                                                                                                                                                                                                                                                                                                                                                        |
| `device.tmp-dir`                 | string         |                                    | `TALARIA_DEVICE_TMP_DIR`               | `--device.tmp-dir`               | Directory the agent uses on this device for its shell snapshot files. Defaults to the system temp directory                                                                                                                                                                                                                                                                                                                                                                                                          |
| `device.shell`                   | string         | `bash`                             | `TALARIA_DEVICE_SHELL`                 | `--device.shell`                 | Shell used to run commands. On Windows: git-bash (Git for Windows' bash), powershell (pwsh when installed, else Windows PowerShell; needs Git Bash too, for the file tools) or wsl (bash in a WSL distribution); bash there picks Git Bash, else WSL                                                                                                                                                                                                                                                                 |
| `device.wsl-distro`              | string         |                                    | `TALARIA_DEVICE_WSL_DISTRO`            | `--device.wsl-distro`            | WSL distribution for device.shell wsl on Windows. Defaults to WSL's default distribution                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `device.resume-buffer`           | integer        | `1048576`                          | `TALARIA_DEVICE_RESUME_BUFFER`         | `--device.resume-buffer`         | Most output bytes per command that run keeps for a resume of the exec leg until the plugin confirms them. Older output of a command is dropped, and the plugin shows the gap                                                                                                                                                                                                                                                                                                                                         |
| `device.max-grace`               | string         | `10m`                              | `TALARIA_DEVICE_MAX_GRACE`             | `--device.max-grace`             | Longest time run keeps commands running after the exec leg drops, waiting for a resume. The plugin's grace_seconds applies when it is shorter                                                                                                                                                                                                                                                                                                                                                                        |
| `device.command-memory-max`      | string         | `auto`                             | `TALARIA_DEVICE_COMMAND_MEMORY_MAX`    | `--device.command-memory-max`    | Memory cap of each agent command, as bytes or with a K, M, G or T suffix (powers of 1024), for example 4G. On Linux with a systemd user manager, run starts each command in a systemd scope of its own with this MemoryMax, so the kernel kills only a command that uses more, and the agent is told so. auto is 75% of the MemoryMax of the cgroup that run runs in (the talaria service), or 4G when that has no limit. 0 or off runs commands in run's own cgroup, without a cap of their own. No effect on macOS |
| `gateway.listen`                 | string         | `127.0.0.1:0`                      | `TALARIA_GATEWAY_LISTEN`               | `--gateway.listen`               | Loopback address of the chat splice. Port 0 picks a free port                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `gateway.splice`                 | boolean        | `true`                             | `TALARIA_GATEWAY_SPLICE`               | `--gateway.splice`               | Run the chat splice in run. false (or run --no-splice) runs only the exec leg: no listener, no daemon state file and no whoami call, for a headless service device such as the sandbox pod                                                                                                                                                                                                                                                                                                                           |
| `chat.managed`                   | boolean        | `true`                             | `TALARIA_CHAT_MANAGED`                 | `--chat.managed`                 | On Linux, run the exact agent build the server runs: run fetches it from the server's container image by digest when the server reports one, verifies it and keeps it in chat.managed-dir. false, or no image from the server, runs chat.hermes instead                                                                                                                                                                                                                                                              |
| `chat.managed-dir`               | string         |                                    | `TALARIA_CHAT_MANAGED_DIR`             | `--chat.managed-dir`             | Directory for the managed agent builds and their own home. Defaults to talaria/hermes in the user cache directory                                                                                                                                                                                                                                                                                                                                                                                                    |
| `chat.hermes`                    | string         | `hermes`                           | `TALARIA_CHAT_HERMES`                  | `--chat.hermes`                  | Local agent launcher (Hermes Agent) when chat.managed is off or the server reports no image. It must be the same release as the server's agent                                                                                                                                                                                                                                                                                                                                                                       |
| `acp-host.listen`                | string         | `:9130`                            | `TALARIA_ACP_HOST_LISTEN`              | `--acp-host.listen`              | Address the ACP host listens on. Only the Talaria hub may reach it                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `acp-host.backend-id`            | string         |                                    | `TALARIA_ACP_HOST_BACKEND_ID`          | `--acp-host.backend-id`          | This backend's id, the aud the hub puts in its assertions. Needed by acp-host                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `acp-host.hub-public-key`        | string         |                                    | `TALARIA_ACP_HOST_HUB_PUBLIC_KEY`      | `--acp-host.hub-public-key`      | Hub Ed25519 public keys: PEM PUBLIC KEY blocks or base64 lines of the raw 32 bytes                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `acp-host.hub-public-key-file`   | string         |                                    | `TALARIA_ACP_HOST_HUB_PUBLIC_KEY_FILE` | `--acp-host.hub-public-key-file` | File holding the hub public keys, in the same format as hub-public-key                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `acp-host.hermes`                | string         | `hermes`                           | `TALARIA_ACP_HOST_HERMES`              | `--acp-host.hermes`              | Hermes launcher that the host runs as <hermes> acp for each connection                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `acp-host.placeholder`           | string         |                                    | `TALARIA_ACP_HOST_PLACEHOLDER`         | `--acp-host.placeholder`         | Server directory that stands in for the device directory in ACP session cwds. Must match the device plugin's placeholder. Needed by acp-host                                                                                                                                                                                                                                                                                                                                                                         |
| `acp-host.max-processes`         | integer        | `8`                                | `TALARIA_ACP_HOST_MAX_PROCESSES`       | `--acp-host.max-processes`       | Most hermes acp processes the host runs at once                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `acp-host.bindings-file`         | string         |                                    | `TALARIA_ACP_HOST_BINDINGS_FILE`       | `--acp-host.bindings-file`       | Bindings file the device plugin writes. Defaults to device/bindings.json in $HERMES_HOME                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `acp-host.gateway-health-url`    | string         | `http://127.0.0.1:9120/api/health` | `TALARIA_ACP_HOST_GATEWAY_HEALTH_URL`  | `--acp-host.gateway-health-url`  | Health URL of the pod's Hermes gateway. The host starts hermes acp only while it answers, because hermes acp starts a gateway of its own when none runs. Empty turns the check off                                                                                                                                                                                                                                                                                                                                   |
| `reconnect.min-backoff`          | string         | `1s`                               | `TALARIA_RECONNECT_MIN_BACKOFF`        | `--reconnect.min-backoff`        | First delay after a lost connection                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `reconnect.max-backoff`          | string         | `30s`                              | `TALARIA_RECONNECT_MAX_BACKOFF`        | `--reconnect.max-backoff`        | Longest delay between attempts                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `browser.enabled`                | boolean        | `false`                            | `TALARIA_BROWSER_ENABLED`              | `--browser.enabled`              | Serve the Talaria Firefox extension on a private socket in the state directory and offer browser control to the plugin, so the agent can use tabs in its tab group. Run talaria browser install once                                                                                                                                                                                                                                                                                                                 |
| `browser.allow-hosts`            | list of string |                                    | `TALARIA_BROWSER_ALLOW_HOSTS`          | `--browser.allow-hosts`          | Loopback or private network host names the agent may open. All other loopback, private, link-local and CGNAT addresses are blocked in agent tabs                                                                                                                                                                                                                                                                                                                                                                     |
| `browser.confirm-hosts`          | list of string |                                    | `TALARIA_BROWSER_CONFIRM_HOSTS`        | `--browser.confirm-hosts`        | Sites where every click and every Enter or Space key press asks the user each time and scripts may only read. Host names; each covers its subdomains. The plugin cannot change this list                                                                                                                                                                                                                                                                                                                             |
| `browser.autologin-hosts`        | list of string |                                    | `TALARIA_BROWSER_AUTOLOGIN_HOSTS`      | `--browser.autologin-hosts`      | Exact host names where browser_login may submit the login form that Firefox fills from its own saved login, so a scheduled job can sign in without the agent seeing the password. Only https pages whose top-level host is listed; no subdomains. Scripts are refused there, and clicks, typing, key presses and screenshots on its login pages. Example: login.w3.ibm.com. The plugin cannot change this list                                                                                                       |
| `browser.start-firefox`          | boolean        | `true`                             | `TALARIA_BROWSER_START_FIREFOX`        | `--browser.start-firefox`        | Let the agent start Firefox, with the user's default profile, in the user's graphical session when the extension is not connected, so a scheduled job works after Firefox was closed. The sandbox image turns it off                                                                                                                                                                                                                                                                                                 |
| `browser.firefox-command`        | string         | `firefox`                          | `TALARIA_BROWSER_FIREFOX_COMMAND`      | `--browser.firefox-command`      | Command that starts Firefox for start-firefox: a name on PATH or an absolute path                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `mcp.servers`                    | map of objects |                                    | —                                      | —                                | MCP servers the agent may use on this device, by name: lowercase letters, digits, - and _, at most 32 characters. Each one runs on this device with stdio, started on first use. Only servers listed here can run. Config file only                                                                                                                                                                                                                                                                                  |
| `mcp.servers.<key>.command`      | string         |                                    | —                                      | —                                | Program to run, a path or a name looked up in the daemon's PATH. Needed                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `mcp.servers.<key>.args`         | list of string |                                    | —                                      | —                                | Arguments of the program                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `mcp.servers.<key>.env`          | map of string  |                                    | —                                      | —                                | Environment variables added to the daemon's environment for this server (secret)                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `mcp.servers.<key>.dir`          | string         |                                    | —                                      | —                                | Absolute working directory. Defaults to $HOME                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `mcp.servers.<key>.idle-timeout` | string         |                                    | —                                      | —                                | Stop this server after this long without a request, instead of mcp.idle-timeout. 0 keeps it running until run stops                                                                                                                                                                                                                                                                                                                                                                                                  |
| `mcp.idle-timeout`               | string         | `1h`                               | `TALARIA_MCP_IDLE_TIMEOUT`             | `--mcp.idle-timeout`             | Stop an MCP server after this long without a request. It starts again on the next request, without its earlier state. 0 keeps it running until run stops                                                                                                                                                                                                                                                                                                                                                             |
| `update.auto`                    | boolean        | `true`                             | `TALARIA_UPDATE_AUTO`                  | `--update.auto`                  | Let run install new releases by itself: it checks GitHub, verifies the release like talaria update, waits until no command, browser operation, MCP call or chat runs, and exits so the service restarts it. Only when the talaria service runs it and the device token comes from login, so never on a service device that uses hub.token or hub.token-file. The sandbox and device images turn it off                                                                                                               |
| `update.interval`                | string         | `6h`                               | `TALARIA_UPDATE_INTERVAL`              | `--update.interval`              | Time between release checks of run, with up to 10% jitter. At least 1h                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `usage.enabled`                  | boolean        |                                    | `TALARIA_USAGE_ENABLED`                | `--usage.enabled`                | Count the tokens Claude Code used here and send the counts to the hub: only token counts, model names, account ids, the signed-in account's email and project folder names, never what was said. Unset: on for run, except in a container, and on for acp-host                                                                                                                                                                                                                                                       |
| `usage.claude-dirs`              | list of string |                                    | `TALARIA_USAGE_CLAUDE_DIRS`            | `--usage.claude-dirs`            | More Claude Code config directories (CLAUDE_CONFIG_DIR values) to count, besides $CLAUDE_CONFIG_DIR or ~/.claude                                                                                                                                                                                                                                                                                                                                                                                                     |
| `usage.hermes-accounts-dir`      | string         |                                    | `TALARIA_USAGE_HERMES_ACCOUNTS_DIR`    | `--usage.hermes-accounts-dir`    | acp-host: directory of the per-account Claude Code directories. Defaults to $CLAUDE_ACCOUNTS_ROOT, else claude in $HERMES_HOME                                                                                                                                                                                                                                                                                                                                                                                       |
| `usage.state-file`               | string         |                                    | `TALARIA_USAGE_STATE_FILE`             | `--usage.state-file`             | File that keeps what was counted across restarts. Defaults to claude-usage.gob in the state directory for run, and device/claude-usage.gob in $HERMES_HOME for acp-host                                                                                                                                                                                                                                                                                                                                              |

<!-- configulator:end -->

### Tests

- Go: `go test -race ./...`. `protocol/contract_test.go` checks that the Go and Python wire constants agree.
- Plugin: `scripts/test-plugin.sh`, which runs the plugin tests in the local Hermes install's Python runtime, including contract tests against `HERMES_AGENT_SRC`. The workflow "Hermes compatibility" runs it inside the Hermes image of the release in its `HERMES_VERSION`; Renovate opens a PR for each new release. The upgrade steps are in hermes-hub's [hub/docs/hermes-upgrade.md](hub/docs/hermes-upgrade.md).
- MCP: `HERMES_MCP_E2E=1 go test -count=1 -v ./internal/mcp/mcpe2e/` runs the device client and a fake MCP server against the plugin's exec route on uvicorn in the local Hermes runtime, and drives the server through the plugin's `device-mcp` tools.
- Firefox: `HERMES_BROWSER_E2E=1 go test -count=1 -v -run TestFirefoxEndToEnd ./internal/browser/firefoxtest/` drives a throwaway headless Firefox on a fresh profile and `HOME`, through the extension and, when `hermes` is on `PATH`, through the plugin's browser tools (docs/browser.md section 10).
