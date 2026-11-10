# hermes-device

Proof of concept for Hermes Agent v0.22.7: one server Hermes owns every session, and a small client on a PC dials out to it and runs the agent's shell and file operations on that PC, in a chosen directory, for the sessions bound to it.

## Parts

- `plugin/device`: Hermes user plugin. It registers the `device` terminal backend, the `/device` command and a `pre_tool_call` hook, and serves the device WebSocket at `/api/plugins/device/ws` on the gateway's dashboard listener.
- `client`: Go device client. See `client/README.md` for its configuration.
- `scripts/interrupt.py`: stops a session's running turn, as Stop does in the interactive CLI.
- `scripts/acp_probe.py`: scripted ACP client that shows what an editor bridge must do.

## How it works

1. All CLI, TUI, Desktop, ACP and messaging turns run in the gateway process. The dashboard (and so the plugin route) is mounted in the same process, so the terminal backend and the device WebSocket share one in-memory hub.
2. The device sends `hello` with its name, token, allowed roots and temp dir. The plugin checks the token against `plugins.entries.device.settings.devices.<name>.token`.
3. `/device <name> <dir>` binds the current session id to the device and directory, after the device confirms the directory with `pwd -P`. Bindings are saved in `$HERMES_HOME/device/bindings.json`.
4. Each command is looked up by the session id ContextVar, sent as `exec`, and streamed back as `output` and `exit` messages. Kill and timeout send `kill`; the client kills the process group.
5. The session's server cwd must exist on the server, so it is a placeholder directory. The backend ignores placeholder paths and keeps the device directory as its cwd. The `pre_tool_call` hook points the session's cwd record at the device directory so file tools resolve relative paths there.

## Server config

```yaml
plugins:
  enabled: [device]
  entries:
    device:
      settings:
        placeholder_roots: [/srv/hermes/placeholder]
        devices:
          desktop:
            token_env: HERMES_DEVICE_DESKTOP_TOKEN
terminal:
  backend: device
  cwd: /srv/hermes/placeholder
webapp:
  enabled: true
  host: 127.0.0.1
  port: 9137
```

## Tests

- Client: `cd client && go test -race ./...`
- Plugin, inside the Hermes runtime: `python3 plugin/device/tests/test_core.py` with the Hermes install's interpreter and `hermes_bootstrap` imported.
