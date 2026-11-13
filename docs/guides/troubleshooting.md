# Troubleshooting

Start with the daemon's log. On Linux:

```sh
systemctl --user status talaria
journalctl --user -u talaria -n 100 --no-pager
```

On macOS: `tail -n 100 ~/Library/Logs/talaria.log`. For more detail, set `log-level: debug` in the config and restart the service. To check the config file without starting anything, run `talaria run --check-config`.

## Setup says `give the hub URL`

Release builds do not know any hub. Pass the address: `talaria setup https://hub.example.com`.

## `the pairing code expired` or `pairing was declined on the hub`

Run `talaria login` again, and confirm the code in the browser within 10 minutes. If the hub page does not open, open the link that the terminal prints yourself. The hub's address must be the one people use in the browser (its `public_url`), or the link will not load.

## The computer shows offline on the hub

1. Is the daemon running? `systemctl --user status talaria`.
2. Can the computer reach the hub? `curl -fsS https://hub.example.com/healthz`.
3. Does the log say the token was refused? The computer was removed on the hub, or the token store changed. Pair again with `talaria login`.
4. On a headless Linux machine, the service stops when you log out. Run `loginctl enable-linger "$USER"` once.

## No systemd user manager

Setup stops with `no systemd user manager or launchd to run talaria run`. This happens on Alpine, in containers and in WSL without systemd. Run `talaria setup --no-service https://hub.example.com`, then start `~/.talaria/bin/talaria run` yourself from your own supervisor.

## `talaria: command not found`

`~/.talaria/bin` is not on your `PATH`. Run `talaria setup` again and say yes when it offers to fix this, then open a new terminal. Commands the agent runs use `bash -l`, so the directory must be on the `PATH` of a login shell too.

## A command was refused: working directory outside the roots

The agent tried to work in a folder that is not under `device.roots`. Add the folder to the config and restart the service, or tell the agent to work inside an allowed folder.

## A command was killed: out of memory

The agent sees `[killed: out of memory (limit 1.5 GiB)...]`. Only that command was killed. Raise the cap with `device.command-memory-max` (for example `4G`) or turn it off with `off`. macOS has no per-command cap.

## The Talaria extension does not connect

- The toolbar button should show a green "on" badge. A grey one means the extension cannot reach the daemon.
- Check that `browser.enabled: true` is in your config and that you ran `talaria browser install`, then restart the daemon.
- Flatpak Firefox cannot reach the host. Use a Firefox from your distribution or from Mozilla. Snap Firefox probably cannot either.
- Another Firefox profile or window may have taken over. Open the toolbar button in the window you want.

## `update` refuses a release

- `the release archive's attestation did not verify`: do not install it. Check you are connected to GitHub directly and try again. Only on an emergency, and only if you checked the file by hand, `--insecure-skip-attestation` skips the check.
- `refusing to replace a newer talaria`: you have a newer release. Use `--force` to go back.
- GitHub rate limit: the program caches the lookup and backs off by itself. Try again later.

## `chat` cannot start Hermes

- On Linux, `chat` downloads the server's Hermes build on first use. Check disk space (about 1.4 GB) and that the computer can reach the registry that holds the image. `talaria chat --status` shows what it knows.
- With a local Hermes (macOS, or `chat.managed: false`), it must be the same release as the server's. `talaria chat` says which two releases differ.

## Still stuck

Collect the version (`talaria --version`), the last 100 lines of the log and the steps you took, and open an issue at [github.com/USA-RedDragon/talaria](https://github.com/USA-RedDragon/talaria/issues). Remove tokens from anything you paste.
