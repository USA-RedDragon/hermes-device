# Fix computer problems

Start with the daemon's log. On Linux:

```sh
systemctl --user status talaria
journalctl --user -u talaria -n 100 --no-pager
```

On macOS: `tail -n 100 ~/Library/Logs/talaria.log`. On Windows, in PowerShell: `Get-Content -Tail 100 $env:LOCALAPPDATA\talaria\talaria.log`. For more detail, set `log-level: debug` in the config and restart the service. To check the config file without starting anything, run `talaria run --check-config`.

## Setup says `give the hub URL`

Release builds do not know any hub. Pass the address: `talaria setup https://hub.example.com`.

## `the pairing code expired` or `pairing was declined on the hub`

Run `talaria login` again, and confirm the code in the browser within 10 minutes. If the hub page does not open, open the link that the terminal prints yourself. The hub's address must be the one people use in the browser (its `public_url`), or the link will not load.

## The computer shows offline on the hub

1. Is the daemon running? `systemctl --user status talaria` on Linux, `launchctl list | grep talaria` on macOS (a number in the first column is its process id), `schtasks /Query /TN talaria` on Windows.
2. Can the computer reach the hub? `curl -fsS https://hub.example.com/healthz`.
3. Does the log say the token was refused? The computer was removed on the hub, or the token store changed. Pair again with `talaria login`.
4. On a headless Linux machine, the service stops when you log out. Run `loginctl enable-linger "$USER"` once.
5. On macOS with a hub on your local network, a log full of `connect: no route to host` means macOS has not let talaria use the local network yet: answer the question on the Mac's screen with Allow, or turn talaria on under System Settings, Privacy & Security, Local Network. See [Add a computer](../start/add-a-computer.md#on-macos).
6. On Windows, `schtasks /Query /TN talaria` should say `Running`. See [Windows: the scheduled task does not run](#windows-the-scheduled-task-does-not-run).

## macOS: `talaria` waits and prints nothing

This happens only with an unsigned build (see Add a computer). macOS blocked a program downloaded with a browser and shows "talaria" Not Opened on the Mac's screen. Press Ctrl-C, choose Done, unpack the archive again and run `xattr -d com.apple.quarantine ./talaria` before the first run. See [Add a computer](../start/add-a-computer.md#on-macos).

## macOS over SSH: `could not read the device token from the OS keyring`

The token is in the login keychain, which macOS keeps locked in SSH sessions. Run the command in Terminal on the Mac, or unlock the keychain first with `security unlock-keychain`.

## No systemd user manager

Setup stops with `no systemd user manager or launchd to run talaria run`. This happens on Alpine, in containers and in WSL without systemd. Run `talaria setup --no-service https://hub.example.com`, then start `~/.talaria/bin/talaria run` yourself from your own supervisor.

## Windows: "Windows protected your PC"

This is Microsoft Defender SmartScreen. Windows shows it when you start a downloaded program from Explorer that has no reputation with Microsoft yet, as unsigned programs such as talaria usually do. Choose More info, check that the app is `talaria.exe`, then Run anyway. Or run `.\talaria.exe setup ...` from a terminal, which does not show it, or run `Unblock-File` on the zip before you unpack it. See [Add a computer](../start/add-a-computer.md#on-windows).

## Windows: Defender removed `talaria.exe`

Microsoft Defender can flag new, unsigned programs. Check the zip against the release's checksum file and its attestation (see [Releases and verification](../releases.md)). If both match, restore the file under Windows Security, Virus & threat protection, Protection history, and report it to Microsoft as a false positive at <https://www.microsoft.com/en-us/wdsi/filesubmission>.

## Windows: PowerShell execution policy

You do not need to change it. `talaria.exe` is a program, not a script, and with the `powershell` shell each command starts PowerShell with `-ExecutionPolicy Bypass` and `-NoProfile`, for that process only. With `git-bash` or `wsl`, commands do not use PowerShell.

## Windows: `WSL has no distribution` or `Git Bash is not installed`

The shell in the config is missing on this PC, and the log repeats the error every 5 seconds while the computer shows offline:

- `WSL has no Linux distribution`: install one, for example `wsl --install -d Ubuntu`, then open it once to create your Linux user.
- `WSL has no distribution "Debian" (found Ubuntu)`: `device.wsl-distro` names a distribution that `wsl --list` does not show. Set it to one that does, or remove it to use your default distribution.
- `Git Bash is not installed`: install [Git for Windows](https://git-scm.com/download/win) (`winget install Git.Git`). The `powershell` shell needs it too.

Then restart the service: `schtasks /End /TN talaria`, then `schtasks /Run /TN talaria`. To pick another shell, set `device.shell` in `%LOCALAPPDATA%\talaria\config.yaml`, or run `talaria setup --overwrite-config --shell <shell>`.

## Windows: the scheduled task does not run

`schtasks /Query /TN talaria` shows the task's status:

- `Running`: the service runs. If the computer is still offline, read the log.
- `Ready`: it is not running. Start it with `schtasks /Run /TN talaria`. The task starts when you log on to Windows and runs only while you are logged on, so it does not run before the first logon after a restart.
- `ERROR: The system cannot find the file specified`: there is no task. Run `talaria setup` again.

Log lines `talaria service: the daemon exited (code N); starting it again in 5s` mean that the daemon starts and fails; the error is on the line before.

## Windows over SSH: `no device token stored`

When setup ran in a desktop session, the token is in Windows Credential Manager, which an SSH session cannot read. The service is not affected. Run commands that need the token, such as `talaria chat`, in a terminal on the PC's desktop.

## `talaria: command not found`

`~/.talaria/bin` is not on your `PATH`. Run `talaria setup` again and say yes when it offers to fix this, then open a new terminal. Commands the agent runs use `bash -l`, so the directory must be on the `PATH` of a login shell too.

## A command was refused: working directory outside the roots

The agent tried to work in a folder that is not under `device.roots`. Add the folder to the config and restart the service, or tell the agent to work inside an allowed folder.

## A command was killed: out of memory

The agent sees `[killed: out of memory ...]`. Only that command was killed. Either the computer ran out of memory, or you set `device.command-memory-max` (there is no cap by default) and the command used more. Raise the cap (for example `16G`) or turn it off with `off`. macOS has no per-command cap.

## The Talaria extension does not connect

- The toolbar button should show a green "on" badge. A grey one means the extension cannot reach the daemon.
- Check that `browser.enabled: true` is in your config and that you ran `talaria browser install`, then restart the daemon.
- Flatpak Firefox cannot reach the host. Use a Firefox from your distribution or from Mozilla. Snap Firefox probably cannot either.
- Another Firefox profile or window may have taken over. Open the toolbar button in the window you want.

## `update` refuses a release

- `the release archive's attestation did not verify`: do not install it. Check you are connected to GitHub directly and try again. Only on an emergency, and only if you checked the file by hand, `--insecure-skip-attestation` skips the check.
- `refusing to replace a newer talaria`: you have a newer release. Use `--force` to go back.
- GitHub rate limit: the program caches the lookup and backs off by itself. Try again later.

## `chat` cannot start the agent

- On Linux, `chat` downloads the server's agent build on first use. Check disk space (about 1.4 GB) and that the computer can reach the registry that holds the image. `talaria chat --status` shows what it knows.
- With a local agent install (macOS, or `chat.managed: false`), it must be the same release as the server's. `talaria chat` says which two releases differ.

## Still stuck

Collect the version (`talaria --version`), the last 100 lines of the log and the steps you took, and open an issue at [github.com/USA-RedDragon/talaria](https://github.com/USA-RedDragon/talaria/issues). Remove tokens from anything you paste.
