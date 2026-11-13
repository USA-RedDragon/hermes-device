# Command line

Every command is `talaria <command>`. `tal` is a short link to the same program. Settings can also come from the config file and `TALARIA_*` environment variables: see [Configuration](configuration.md), which lists the flag, the variable and the config key of each one.

Options that every command takes: `-c, --config FILE` (default `~/.config/talaria/config.yaml`), and every configuration key as a flag, such as `--hub.url`. Run `talaria --help` to see them all.

The text under each command is generated from the program's own help.

## setup

Install, pair, configure, start the service and connect Firefox. See [Add a computer](../start/add-a-computer.md).

<!-- docgen:output exec="scripts/cli-usage.sh setup" -->
```text
setup installs talaria for this user in one step: it copies this binary to ~/.talaria/bin, pairs the computer with the hub (as login does), writes the config file when there is none, registers Firefox's native messaging host and opens the extension's install page, starts a systemd user service (launchd agent on macOS), offers to put ~/.talaria/bin on PATH for your login shell and bash -l, and offers to fix bash's PATH for zsh and fish users. On Windows it registers a scheduled task that starts talaria at logon, puts talaria on your PATH and asks which shell the agent's commands run in (--shell). Run it again at any time; it only changes what is missing. setup --uninstall reverses what setup did.

Usage:
  talaria setup [hub-url] [flags]

Flags:
      --bin-dir string                  Directory to install the binary in (default ~/.talaria/bin)
      --force[=true|false]              Replace a newer installed binary, or a service file that setup did not write (after a backup)
  -h, --help[=true|false]               help for setup
      --name string                     Name to suggest for this computer on the hub (default the host name)
      --no-browser[=true|false]         Leave Firefox alone and write browser.enabled false into a new config
      --no-service[=true|false]         Do not install or start the background service
      --no-shell-check[=true|false]     Do not check or change the PATH of the login shell and bash -l
      --overwrite-config[=true|false]   Replace an existing config file (after a backup)
      --shell string                    Windows: the shell for agent commands in a new config, git-bash, powershell or wsl (default: ask, or Git Bash when installed)
      --uninstall[=true|false]          Undo what setup did, as its state file records
      --wait duration                   How long to wait for the Firefox extension to connect; 0 skips (default 10m0s)
      --wsl-distro string               Windows: the WSL distribution for --shell wsl (default: WSL's default)
```
<!-- /docgen:output -->

## login

Pair this computer with the hub and store its token. `setup` does this for you.

<!-- docgen:output exec="scripts/cli-usage.sh login" -->
```text
github.com/USA-RedDragon/talaria/internal/claudeusage: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/browser/broker: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/attest: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/autoupdate: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/oci: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/acpbridge: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/setup: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/daemonstate: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/executor: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/mcp: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/splice: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/pairing: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## run

The daemon: runs the agent's commands here and serves the loopback connection that `chat` uses. The service runs this.

<!-- docgen:output exec="scripts/cli-usage.sh run" -->
```text
github.com/USA-RedDragon/talaria/internal/oci: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/setup: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/executor: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/daemonstate: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## chat

Open the Hermes terminal interface on a server session. See [Chat from a terminal](../guides/terminal-chat.md).

<!-- docgen:output exec="scripts/cli-usage.sh chat" -->
```text
github.com/USA-RedDragon/talaria/internal/browser/broker: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## acp

Bridge an editor's ACP over standard input and output. See [Use Talaria in an editor](../guides/editor-bridge.md).

<!-- docgen:output exec="scripts/cli-usage.sh acp" -->
```text
acp speaks the Agent Client Protocol with an editor on stdin and stdout and relays it to the server's agent through the Talaria hub. Point the editor's agent command at `talaria acp`. Logs go to stderr.

Usage:
  talaria acp [flags]

Flags:
  -h, --help[=true|false]   help for acp
```
<!-- /docgen:output -->

## update

Install the latest release or a given one. See [Update, move or remove a computer](../guides/update-and-remove.md).

<!-- docgen:output exec="scripts/cli-usage.sh update" -->
```text
github.com/USA-RedDragon/talaria/internal/setup: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/executor: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## browser

Firefox browser control. See [Let the agent use Firefox](../guides/browser-extension.md).

<!-- docgen:output exec="scripts/cli-usage.sh browser" -->
```text
Firefox browser control (docs/browser.md)

Usage:
  talaria browser [command]

Available Commands:
  extension   Write the Talaria Firefox extension's files to a directory, to load as a temporary add-on
  install     Register this binary as the native messaging host of the Talaria Firefox extension, for this user
  uninstall   Remove the native messaging host manifest that install wrote

Flags:
  -h, --help[=true|false]   help for browser
```
<!-- /docgen:output -->

## browser install

Register the native messaging host for the Firefox extension.

<!-- docgen:output exec="scripts/cli-usage.sh browser install" -->
```text
Register this binary as the native messaging host of the Talaria Firefox extension, for this user

Usage:
  talaria browser install [flags]

Aliases:
  install, install-host

Flags:
  -h, --help[=true|false]   help for install
```
<!-- /docgen:output -->

## browser extension

Write the extension's files to a folder, to load as a temporary add-on.

<!-- docgen:output exec="scripts/cli-usage.sh browser extension" -->
```text
Write the Talaria Firefox extension's files to a directory, to load as a temporary add-on. Each release also publishes the extension signed by Mozilla, which installs for good and updates itself (docs/browser.md section 6.1).

Usage:
  talaria browser extension [flags]

Flags:
      --ext-version string   Version to put in manifest.json (default: this binary's release version, else the embedded one)
  -h, --help[=true|false]    help for extension
  -o, --out string           Directory to write the extension to (default "hermes-browser-extension")
```
<!-- /docgen:output -->

## browser uninstall

Remove the native messaging host.

<!-- docgen:output exec="scripts/cli-usage.sh browser uninstall" -->
```text
github.com/USA-RedDragon/talaria/internal/attest: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/autoupdate: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
github.com/USA-RedDragon/talaria/internal/acpbridge: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## plugin install

On the server: write the Hermes device plugin into a plugins folder. For people who run the server by hand; the [install kit](../deploy/getting-started.md) does this for you.

<!-- docgen:output exec="scripts/cli-usage.sh plugin install" -->
```text
github.com/USA-RedDragon/talaria/internal/daemonstate: /usr/lib/go/pkg/tool/linux_amd64/compile: fork/exec /usr/lib/go/pkg/tool/linux_amd64/compile: resource temporarily unavailable
```
<!-- /docgen:output -->

## acp-host

On the server: run one `hermes acp` per hub connection. Used by the server install, not on your computer.

<!-- docgen:output exec="scripts/cli-usage.sh acp-host" -->
```text
In the Hermes pod: run one hermes acp per hub connection

Usage:
  talaria acp-host [flags]

Flags:
  -h, --help[=true|false]   help for acp-host
```
<!-- /docgen:output -->
