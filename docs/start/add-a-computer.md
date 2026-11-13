# Add a computer

By default the agent runs its commands in the sandbox on the server. To let it work on your own files and programs, install the `talaria` program on your computer and pair it with your hub. After that you choose, per chat, whether the agent works in the sandbox or on that computer, in a folder you pick. The same program also lets the agent use your Firefox, and lets you chat with the agent from a terminal or an editor.

The computer connects out to the hub over HTTPS. You do not open any port on the computer.

## Before you start

- You need a Talaria hub: see [Pick an install](../deploy/getting-started.md) to run your own.
- The hub must be reachable from the computer at its public address (`TALARIA_PUBLIC_URL`, or `publicUrl` in Helm), for example `https://hub.example.com`. For a try-out on one machine, `http://localhost:8080` works from that machine only.
- You need an account on the hub.
- The computer must run Linux, macOS or Windows:

| System | Status |
|---|---|
| Linux, amd64, arm64, armv6, armv7, riscv64 | Supported. The background service needs a systemd user manager. |
| macOS, Intel and Apple silicon | Supported. The background service is a launchd agent. |
| Windows 10 and 11, amd64 and arm64 | Supported. Commands run in Git Bash, PowerShell or WSL; see [On Windows](#on-windows). The background service is a scheduled task. Tested on Windows 11 24H2, amd64. |

Two features have more limits:

- **Chat from a terminal** (`talaria chat`) runs the same Hermes build as your server on Linux with glibc 2.28 or newer. On macOS it runs a Hermes that you installed yourself, and it must be the same release as the server's.
- **Firefox control** needs Firefox 139 or newer from your distribution or from Mozilla. The Flatpak build of Firefox cannot reach the program that the extension talks to, and the Snap build is untested and likely has the same problem, so use another build.

## Download the program

1. Open the [latest release](https://github.com/USA-RedDragon/talaria/releases/latest) and download `talaria_<version>_<os>_<arch>.tar.gz` for your system. `<os>` is `linux` or `darwin`, and `<arch>` is `amd64`, `arm64`, `armv6`, `armv7` or `riscv64`. For Windows, download `talaria_<version>_windows_amd64.zip` (or `windows_arm64`) and continue at [On Windows](#on-windows).
2. Check the download. Every release since <!-- docgen:const pkg=internal/setup name=FirstAttestedVersion code=false -->0.13.0<!-- /docgen:const --> has a signed build record that proves which workflow built the file. With the [GitHub CLI](https://cli.github.com):

   ```sh
   gh attestation verify talaria_<version>_linux_amd64.tar.gz --repo USA-RedDragon/talaria
   ```

   [Releases and verification](../releases.md) explains this and the SHA-256 check by hand.
3. Unpack it:

   ```sh
   tar -xzf talaria_<version>_linux_amd64.tar.gz
   ```

## Run setup

`talaria setup` does everything in one run: it installs the program, pairs the computer with your hub, writes a config file, starts the background service and connects Firefox. It only does the steps that are not done yet, so you can run it again at any time. The hub address is required. A release does not know any hub by itself, so without the address setup stops with `give the hub URL`.

On Linux, run the unpacked program once. It installs itself under `~/.talaria/bin` and does the rest:

```sh
./talaria setup https://hub.example.com
```

### On macOS

Releases that the repository's maintainer has set up for it are signed with an Apple Developer ID and notarized by Apple, and need none of the steps below (on the first run macOS checks with Apple, so the Mac needs network access). The steps are for an unsigned build: a release made before signing was set up, or one from a fork. `codesign -dv ./talaria` tells which you have. What happens on the first run of an unsigned build depends on how you downloaded it:

- **With `curl` in Terminal**, macOS does not mark the file, and it runs at once. This is the simplest way (use `darwin_amd64` on an Intel Mac):

  ```sh
  V=<version>   # for example 0.25.4, without the v
  curl -fsSLO https://github.com/USA-RedDragon/talaria/releases/download/v$V/talaria_${V}_darwin_arm64.tar.gz
  curl -fsSLO https://github.com/USA-RedDragon/talaria/releases/download/v$V/talaria_${V}_checksums.txt
  grep talaria_${V}_darwin_arm64 talaria_${V}_checksums.txt | shasum -a 256 -c -
  tar -xzf talaria_${V}_darwin_arm64.tar.gz
  ./talaria setup https://hub.example.com
  ```

- **With a browser**, macOS marks the archive as downloaded from the internet, and `tar` passes the mark on to `talaria`. Clear the mark before you run it the first time:

  ```sh
  xattr -d com.apple.quarantine ./talaria
  ./talaria setup https://hub.example.com
  ```

  If you run it with the mark, macOS refuses it with "talaria" Not Opened. In Terminal the command then just waits with no output; press Ctrl-C.

  ![macOS dialog: "talaria" Not Opened. Apple could not verify "talaria" is free of malware that may harm your Mac or compromise your privacy. Buttons: Move to Trash, Done](../images/macos-not-opened.png)

  Choose Done. Clearing the mark on that same file afterwards was not enough in our test on macOS 26: unpack the archive again, then run `xattr -d` on the new file before its first run. The other way is System Settings, Privacy & Security, "Open Anyway".

The macOS service is a launchd agent, `~/Library/LaunchAgents/io.github.usa-reddragon.talaria.plist`, and its log is `~/Library/Logs/talaria.log`. macOS shows a notification that a login item was added; that is this agent, and it is listed under System Settings, General, Login Items & Extensions.

**A hub on your local network.** If the hub address is a LAN address (such as `http://192.168.1.10:8080`), macOS asks once whether talaria may use the local network:

![macOS dialog: Allow "talaria" to find devices on local networks? Buttons: Don't Allow, Allow](../images/macos-local-network.png)

Choose Allow. Until you do, the service cannot reach the hub and its log says `connect: no route to host`, even though `curl` to the same address works from an SSH session. If you chose Don't Allow, turn talaria on under System Settings, Privacy & Security, Local Network. A hub at a public HTTPS name or a Tailscale address is not affected.

**Setup over SSH.** You can run setup on a Mac over SSH while you are also logged in at the Mac. Setup then prints the pairing link and does not open a browser or Firefox on the Mac's screen; the Local Network question above still appears on that screen. macOS keeps the login keychain locked in SSH sessions, so:

- setup over SSH stores the token in a file readable only by you, `~/Library/Application Support/talaria/token`, instead of the Keychain;
- if setup ran in Terminal and the token is in the Keychain, commands that read it (`talaria chat`, `talaria acp`) cannot do so over SSH and say so. Run them in Terminal on the Mac.

### On Windows

**Choose your shell.** On Windows the agent's commands run in one shell that you pick at setup. The agent is told which one at the start of each chat, so it writes commands for it:

| Shell | What the agent writes | Paths the agent sees | Needs |
|---|---|---|---|
| `git-bash` (the default when Git is installed) | bash commands; Windows programs (`git`, `where.exe`, `cmd //c`) run by name | `/c/Users/alice/src` for `C:\Users\alice\src` | [Git for Windows](https://git-scm.com/download/win) (`winget install Git.Git`) |
| `powershell` | PowerShell commands (`Get-ChildItem`, `$env:NAME`). PowerShell 7 (`pwsh`) when installed, else Windows PowerShell | the same `/c/...` paths in its file tools; commands see `C:\Users\alice\src` | Git for Windows too: the agent's file tools and the scripts around each command still use Git Bash |
| `wsl` | bash commands that run in Linux, in your default WSL distribution or the one you name with `--wsl-distro` | `/mnt/c/Users/alice/src`; your Linux home (`~`) is not your Windows profile | WSL 2 with a distribution (`wsl --install -d Ubuntu`) |

The allowed folders (`device.roots`) are always Windows folders, your profile folder by default, so in every shell the agent works in your Windows files. To change the shell later, set `device.shell` in the config file (and `device.wsl-distro` for `wsl`) and restart the service, or run setup again with `--overwrite-config --shell <shell>`.

**Install.** Download and unpack the zip (`talaria_<version>_windows_amd64.zip`, or `arm64` on an ARM PC), open PowerShell or Windows Terminal in the unpacked folder (right-click it, "Open in Terminal"), and run:

```powershell
.\talaria.exe setup https://hub.example.com
```

After pairing, setup asks for the shell when more than one is available; press Enter for the first. To answer in advance, pass `--shell git-bash`, `--shell powershell` or `--shell wsl`.

![Windows Terminal: talaria setup asks which shell the agent's commands run in: 1. Git Bash, 2. PowerShell, 3. WSL](../images/windows-setup-shell.png)

Then it works through the steps below, with these Windows places:

- The program goes to `%USERPROFILE%\.talaria\bin\talaria.exe`, and that folder is added to your user `PATH` (new terminals find `talaria`).
- The config is `%LOCALAPPDATA%\talaria\config.yaml`. It stays on this computer and does not roam with your profile.
- The token goes to Windows Credential Manager, or to `%LOCALAPPDATA%\talaria\token` (readable only by you) when Credential Manager cannot be used, as in an SSH session.
- The service is a scheduled task named `talaria` that starts at your logon and runs while you are logged in, with no window. Its log is `%LOCALAPPDATA%\talaria\talaria.log`. Look at it with `Get-Content -Wait -Tail 50 $env:LOCALAPPDATA\talaria\talaria.log`, or at the task in Task Scheduler.
- Firefox control registers the native messaging host under `HKEY_CURRENT_USER\Software\Mozilla\NativeMessagingHosts`.

Setup needs no administrator rights. Stopping a command from the chat ends it and every program it started (a Windows Job Object holds them all).

The paired PC then works like any other computer. In a chat on it, the agent sees your Windows folders in the form its shell uses, here Git Bash:

![A chat on windows-pc, in /c/Users/tester/projects/website: the agent ran pwd, cygpath -w and ls in Git Bash, and the output shows both /c/Users/tester/projects/website and C:\\Users\\tester\\projects\\website](../images/windows-chat.png)

**SmartScreen.** The program is not code-signed. If you unpack the zip in Explorer and double-click `talaria.exe`, Windows shows "Windows protected your PC". Choose "More info", check that the app is `talaria.exe` (the publisher shows as "Unknown publisher"), then "Run anyway". Starting it from a terminal as above does not show this. To clear the downloaded-from-the-internet mark for the whole zip before you unpack it, run `Unblock-File .\talaria_<version>_windows_amd64.zip`.

![Windows SmartScreen after More info: Windows protected your PC. Microsoft Defender SmartScreen prevented an unrecognized app from starting. App: talaria.exe, Publisher: Unknown publisher. Buttons: Run anyway, Don't run](../images/windows-smartscreen.png)

**Microsoft Defender.** Defender scans the program like any download. If it ever quarantines `talaria.exe`, check the file against the release's checksum file and attestation (see [Releases and verification](../releases.md)), then restore it under Windows Security, Virus & threat protection, Protection history.

**PowerShell execution policy.** Nothing to change. `talaria.exe` is a program, not a script, and with the `powershell` shell it starts PowerShell for each command with `-ExecutionPolicy Bypass` for that process only, so your machine's policy stays as it is.

**Uninstall.** Run `talaria setup --uninstall`. It ends the scheduled task and the daemon, takes `%USERPROFILE%\.talaria\bin` out of your `PATH`, removes the native messaging host, the token, the config (unless you changed it) and the program. Windows cannot delete a program while it runs, so the `talaria.exe` that runs the uninstall is moved to your temp folder, and the summary says where; delete it afterwards. Then remove the computer on the hub (Settings, Computers).

#### Run the Linux build inside WSL2 instead

You can also install the Linux release inside a WSL 2 distribution, exactly as on Linux (`talaria_<version>_linux_amd64.tar.gz`, then `./talaria setup https://hub.example.com` in the WSL terminal). The hub then sees a Linux computer: the agent works in the distribution's own files, and your Windows files are under `/mnt/c`. The service needs systemd in the distribution (`[boot]` `systemd=true` in `/etc/wsl.conf`, then `wsl --shutdown`); without it, use `--no-service` and start `talaria run` yourself. WSL stops a distribution some time after its last terminal closes, and the computer then shows offline. The Windows program with `--shell wsl` does not have this problem: its scheduled task starts at logon and starts WSL for each command.

Setup works through these steps, in this order:

1. **Install.** The program copies itself to `~/.talaria/bin/talaria` and makes a short link, `tal`, next to it. It will not replace a newer installed release unless you pass `--force`.
2. **Pair.** It prints a link and a code and opens the link in your browser when it can (not over SSH). [Approve the pairing](#approve-the-pairing) below.
3. **Config.** It writes `~/.config/talaria/config.yaml` (on macOS `~/Library/Application Support/talaria/config.yaml`) when that file does not exist, with your hub address and `device.roots` set to your home directory. An existing file is never changed unless you pass `--overwrite-config`, and then the old one is saved first.
4. **Firefox.** When it finds Firefox, it turns browser control on and registers the native messaging host.
5. **PATH.** It checks that `~/.talaria/bin` is on your `PATH`, also for `bash -l`, because the agent runs commands with a login shell. It asks before it changes any of your shell files, and it saves a backup of each one first. When setup does not run in a terminal, it changes nothing and lists what to add.
6. **Service.** It installs and starts the daemon so that it starts when you log in: a systemd user unit on Linux, a launchd agent on macOS. The daemon connects to the hub and reconnects by itself.
7. **Extension.** If Firefox does not have the Talaria extension yet, it opens Firefox at the extension's install page. Choose "Continue to Installation", then "Add". Setup waits up to 10 minutes for the extension to connect.
8. **Summary.** It prints what it did and what is left for you. If a step failed, the summary ends with the error and the later steps did not run.

Useful options:

- `--no-service` skips the background service. `--no-browser` leaves Firefox alone. `--no-shell-check` leaves your shell files alone.
- `--name my-laptop` suggests the computer's name on the hub.
- `--bin-dir DIR` installs the program somewhere else.

The full list is in the [command line reference](../reference/cli.md#setup).

### A server you reach over SSH, or a machine with no systemd

On a headless Linux machine, the service runs while you are logged in. To keep it running after you log out and after a reboot, ask systemd to keep your user's services running:

```sh
loginctl enable-linger "$USER"
```

If the computer has no systemd user manager (Alpine, WSL without systemd, a container), setup cannot install the service and says so. Run `./talaria setup --no-service https://hub.example.com`, then start the daemon yourself with `~/.talaria/bin/talaria run`, from a process manager of your choice.

For a computer you reach only over SSH, pair once with `talaria login`, then run the daemon under your own supervisor. For a machine with no person at all, such as a sandbox, give the daemon a token with `hub.token-file` instead. Both are covered in the [configuration reference](../reference/configuration.md).

## Approve the pairing

Setup prints a link and a code:

```text
Open https://hub.example.com/device/pair
sign in, enter the code BCDF-GHJK and confirm this device.
```

Open the link, log in to the hub and type the code. The hub then shows what is asking to connect: the computer's name, its system, its address and the time. Check that they match the computer in front of you, name it, and choose "Connect this computer".

![The "Connect a computer" page: it shows the computer's name, system, address and time, a name field and a Connect this computer button](../images/pair-confirm.png)

The page then says the computer is connected, and setup finishes on the computer within a few seconds, with `Paired as <name> (<id>) with backend <backend>`.

![The page after confirming: the computer is now connected to Talaria](../images/paired.png)

The code expires after 10 minutes by default. If it does, run `talaria login` again.

## Check that it works

On the computer, `talaria --help` should print the help. If your shell says `talaria: command not found`, `~/.talaria/bin` is not on your `PATH` yet. Setup offers to add it. Open a new terminal after you say yes, or run `~/.talaria/bin/talaria` by its full path.

In the hub, open Settings and then Computers. The new computer shows as online, next to the Sandbox, while the service runs.

![Settings, Computers: the paired computer and the sandbox, both online, with when and from where the computer was last used](../images/settings-computers.png)

To see the service itself:

```sh
systemctl --user status talaria      # Linux
journalctl --user -u talaria -f      # Linux logs
launchctl list | grep talaria        # macOS
tail -f ~/Library/Logs/talaria.log   # macOS logs
```

On Windows, in PowerShell:

```powershell
schtasks /Query /TN talaria                                   # Running while you are logged in
Get-Content -Wait -Tail 50 $env:LOCALAPPDATA\talaria\talaria.log
```

Then run something on it from a chat. Use the arrow next to the plus button to start a chat on a computer, then pick a folder. Or use `/device` in an existing chat to move it.

![The new chat menu: Sandbox, with no access to your files, and the paired computer, which asks for a folder next](../images/new-chat-where.png)

![The folder picker for a new chat on the paired computer: Home, projects, website, and a Start chat here button](../images/pick-folder.png)

Ask the agent to run a command, such as `pwd`. The chat's header says where it runs, and the command's output comes from the computer:

![A chat on alice-laptop, in /home/alice/projects/website: the agent ran a command and shows its output](../images/command-on-computer.png)

Other ways to reach the computer:

- On Telegram, `/devices` lists your computers and `/new NAME [folder]` starts a chat on one.
- From a terminal on that computer, `talaria chat` opens the agent in the current folder. See [Chat from a terminal](../guides/terminal-chat.md).

A chat that was ever tied to a computer never falls back to the server. If the computer goes offline, its messages fail until it is back.

## Which folders and what the agent can do

By default the agent may run commands in your home directory and below, and nowhere else. A command whose working directory is outside the allowed folders is refused. The folders limit where commands start, not what a command does: a command can still `cd` elsewhere, and the next command in that chat is then refused until it starts in an allowed folder again. To narrow it, edit `~/.config/talaria/config.yaml` (on macOS `~/Library/Application Support/talaria/config.yaml`, on Windows `%LOCALAPPDATA%\talaria\config.yaml`, with Windows paths such as `C:\Users\alice\src`):

```yaml
device:
  roots:
    - /home/alice/src
    - /home/alice/notes
```

Then restart the service: `systemctl --user restart talaria` on Linux, `launchctl kickstart -k gui/$(id -u)/io.github.usa-reddragon.talaria` on macOS, `schtasks /End /TN talaria` then `schtasks /Run /TN talaria` on Windows, or run `talaria setup` again on any of them. Every key is listed in the [configuration reference](../reference/configuration.md).

- The agent runs commands as your user, so it can do what you can do in the allowed folders. Treat it like a person who has your terminal.
- Each command runs with a memory cap on Linux with systemd, so a runaway command is killed and nothing else is affected. The default is 75% of the service's limit.
- The agent asks before it does anything you cannot undo in Firefox: see [Let the agent use Firefox](../guides/browser-extension.md).
- The agent cannot reach the computer when the daemon is not running.
- The computer's token stays in your system keyring (Secret Service on Linux, Keychain on macOS, Credential Manager on Windows), or in a file readable only by you when there is no keyring (or, on macOS, when setup ran over SSH).
- To cut a computer off, remove it in Settings, Computers. Its token stops working at once.

## Firefox and editors

- [Let the agent use Firefox](../guides/browser-extension.md): the extension, what the agent can do in your browser and when it asks you.
- [Use Talaria in an editor](../guides/editor-bridge.md): `talaria acp` for VS Code, Zed and other ACP editors. The computer must be paired first.

## If your hub is not on the public internet

The computer must reach the hub. Put the hub behind a VPN you already use (Tailscale, WireGuard) or a tunnel, and use that address as the public URL everywhere. A computer on the same LAN can use `http://<host>:8080` if you set `TALARIA_PUBLIC_URL` to that, but then pages and push notifications do not work, because they need HTTPS.

## Update, move or remove a computer

[Update, move or remove a computer](../guides/update-and-remove.md) covers self-updates, moving to another hub and `talaria setup --uninstall`.

## If something goes wrong

See [Fix computer problems](../guides/troubleshooting.md): setup errors, an expired code, a computer that shows offline, refused commands and the extension.
