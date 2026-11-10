# hermes-device client

Dials the Hermes device plugin over a WebSocket and runs the agent's shell and file commands on this device, only in the allowed root directories.

## Configuration

<!-- configulator:begin -->

| Key                     | Type           | Default | Environment                           | Flag                      | Description                                                                                                              |
|-------------------------|----------------|---------|---------------------------------------|---------------------------|--------------------------------------------------------------------------------------------------------------------------|
| `log-level`             | string         | `info`  | `HERMES_DEVICE_LOG_LEVEL`             | `--log-level`             | Log level: debug, info, warn or error                                                                                    |
| `server.url`            | string         |         | `HERMES_DEVICE_SERVER_URL`            | `--server.url`            | WebSocket URL of the Hermes device plugin, for example ws://127.0.0.1:9119/api/plugins/device/ws (required)              |
| `device.name`           | string         |         | `HERMES_DEVICE_DEVICE_NAME`           | `--device.name`           | Device name, as used in /device <name> <dir> (required)                                                                  |
| `device.token`          | string         |         | `HERMES_DEVICE_DEVICE_TOKEN`          | `--device.token`          | Shared token for this device, matching the server's plugin config (required) (secret)                                    |
| `roots`                 | list of string |         | `HERMES_DEVICE_ROOTS`                 | `--roots`                 | Absolute directories commands may run in. A command whose working directory is outside all of them is refused (required) |
| `tmp-dir`               | string         |         | `HERMES_DEVICE_TMP_DIR`               | `--tmp-dir`               | Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory                 |
| `shell`                 | string         | `bash`  | `HERMES_DEVICE_SHELL`                 | `--shell`                 | Shell used to run commands                                                                                               |
| `reconnect.min-backoff` | string         | `1s`    | `HERMES_DEVICE_RECONNECT_MIN_BACKOFF` | `--reconnect.min-backoff` | First delay after a lost connection                                                                                      |
| `reconnect.max-backoff` | string         | `30s`   | `HERMES_DEVICE_RECONNECT_MAX_BACKOFF` | `--reconnect.max-backoff` | Longest delay between attempts                                                                                           |

<!-- configulator:end -->
