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
Pair this device with the Talaria hub and store its token

Usage:
  talaria login [flags]

Flags:
  -h, --help[=true|false]   help for login
      --name string         Name to suggest for this device on the hub (default the host name)
```
<!-- /docgen:output -->

## run

The daemon: runs the agent's commands here and serves the loopback connection that `chat` uses. The service runs this.

<!-- docgen:output exec="scripts/cli-usage.sh run" -->
```text
Run the device daemon: the exec leg and the chat splice. With --no-splice (or gateway.splice false) it runs only the exec leg, for a headless service device such as the sandbox pod.

Usage:
  talaria run [flags]

Flags:
      --check-config[=true|false]   Load and check the config the way run does, then exit; auto-update runs a new release with it before it installs it
  -h, --help[=true|false]           help for run
      --no-splice[=true|false]      Run only the exec leg, without the chat splice (sets gateway.splice to false)
```
<!-- /docgen:output -->

## chat

Open the Hermes terminal interface on a server session. See [Chat from a terminal](../guides/terminal-chat.md).

<!-- docgen:output exec="scripts/cli-usage.sh chat" -->
```text
chat starts the local Hermes TUI (hermes --tui) in the current directory and points it at the running talaria run daemon, which carries it to the server's Hermes. On Linux it runs the exact build the server runs, which run fetches from the server's container image and verifies (chat.managed); otherwise, or when the server reports no image, it runs chat.hermes of the same release. Arguments after -- go to Hermes. Hermes' -c/--continue and -r/--resume flags are looked up on the server: -c alone continues the most recent session bound to this device. --status shows the managed builds and --refresh fetches the server's build again.

Usage:
  talaria chat [-- hermes-args...] [flags]

Flags:
      --allow-version-mismatch[=true|false]   Start the TUI even when the local Hermes is not the server's release
  -h, --help[=true|false]                     help for chat
      --refresh[=true|false]                  Fetch and verify the server's Hermes build again, remove old builds, then exit
      --status[=true|false]                   Show the server's Hermes and the managed builds on this device, then exit
```
<!-- /docgen:output -->

## acp

Bridge an editor's ACP over standard input and output. See [Use Talaria in an editor](../guides/editor-bridge.md).

<!-- docgen:output exec="scripts/cli-usage.sh acp" -->
```text
acp speaks the Agent Client Protocol with an editor on stdin and stdout and relays it to the server's Hermes through the Talaria hub. Point the editor's agent command at `talaria acp`. Logs go to stderr.

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
update downloads the release archive for this OS and architecture from GitHub, checks it against the release's checksum file, verifies the archive's GitHub artifact attestation (Sigstore-signed build provenance from this repository's release workflow at the release tag), checks that the new binary runs and reports the release's version, replaces the installed binary (~/.talaria/bin/talaria when it exists, else this binary) and restarts the service when it runs. It refuses a downgrade, and replacing a development build, without --force. It refuses a release without a valid attestation, which includes every release before v0.13.0, without --insecure-skip-attestation.

Usage:
  talaria update [version] [flags]

Flags:
      --check[=true|false]                       Download and verify the release (checksum, attestation, the binary runs) but install nothing
      --force[=true|false]                       Install even when it is a downgrade, the same version, or replaces a development build
  -h, --help[=true|false]                        help for update
      --insecure-skip-attestation[=true|false]   UNSAFE: install without verifying the release's attestation, for emergencies and releases before v0.13.0
      --no-restart[=true|false]                  Do not restart the service
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
Remove the native messaging host manifest that install wrote

Usage:
  talaria browser uninstall [flags]

Flags:
  -h, --help[=true|false]   help for uninstall
```
<!-- /docgen:output -->

## plugin install

On the server: write the Hermes device plugin into a plugins folder. For people who run the server by hand; the [install kit](../deploy/getting-started.md) does this for you.

<!-- docgen:output exec="scripts/cli-usage.sh plugin install" -->
```text
Writes the device plugin files and a VERSION file into --dest, replacing an older copy. Run it before Hermes starts, for example from an init container, then enable the plugin with hermes plugins enable device.

Usage:
  talaria plugin install [flags]

Flags:
      --dest string         Plugin directory to write, for example /opt/data/hermes/plugins/device
  -h, --help[=true|false]   help for install
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
