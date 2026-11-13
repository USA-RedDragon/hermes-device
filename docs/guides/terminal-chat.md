# Chat from a terminal

`talaria chat` opens the agent's terminal interface on this computer and connects it to the session on your server. The chat is the same one you see in the web app and in Telegram, but the agent's commands and file changes happen here, in the folder where you started it.

```sh
cd ~/src/app
talaria chat
```

The daemon (`talaria run`, started by setup) must be running. `chat` binds the new session to the current folder before the interface shows it, and refuses calls that would act on the server instead of your computer.

## Continue a session

Pass the agent's own flags after `--`:

```sh
talaria chat -- -c                      # the most recent session bound to this computer
talaria chat -- -c "fix the parser"     # a session with this title
talaria chat -- --resume <session-id>
```

`chat` looks the session up on the server. The resumed session moves to the current folder. Inside the interface, `/new` starts another session in the same folder and `/resume <title>` works the same way as the flag.

## Which agent build runs

On Linux, `chat` runs the exact agent build that your server runs. The daemon reads the server's image from its welcome message, downloads that build by its digest in the background, checks every layer against its SHA-256, and keeps it in `~/.cache/talaria/hermes` (about 1.4 GB). It keeps the current build and the one before it. The managed build has a home of its own and does not read or write `~/.hermes`.

On macOS, or when you set `chat.managed: false`, `chat` runs the `hermes` on your `PATH`. It must be the same release as the server's, and `chat` refuses to start on a mismatch. `--allow-version-mismatch` starts it anyway.

Two helper flags:

```sh
talaria chat --status     # the server's agent release and the builds on this computer
talaria chat --refresh    # fetch and verify the server's build again, remove old ones
```

The full list of options is in the [command line reference](../reference/cli.md#chat).
