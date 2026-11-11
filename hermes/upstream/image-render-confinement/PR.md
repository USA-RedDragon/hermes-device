# feat(tui): security.image_render_paths confines image.render reads

## What

`image.render` (the JSON-RPC method the Ink TUI uses for inline transcript image previews) reads any local image file on the gateway host whose path the client sends. This adds a config key that can confine those reads:

```yaml
security:
  image_render_paths: any   # any | workspace | media
```

- `any` (default): today's behavior, unchanged.
- `media`: only images Hermes stored itself, under the serving profile's home: uploads and clipboard images (`images/`, `cache/images/`), browser and computer-use screenshots (`cache/screenshots/`), vision inputs (`cache/vision/`), generated images (`cache/generated/`), and the legacy names of those directories.
- `workspace`: `media`, plus files under the host working directory of a session the requesting connection is attached to. On the shared gateway socket these are the sessions the connection subscribes to, with the frozen policy cwd, and only when the session's terminal works in the host cwd (`terminal_uses_host_cwd`, the same rule `complete.path` uses). On the sidecar transport they are the sidecar sessions on that transport whose cwd is not remote (ssh). A workspace that resolves to a filesystem root is ignored.

In both confined modes:

- The path is expanded and resolved (`~`, `..`, symlinks) before the check, and the resolved path is the one that gets read.
- Nothing else under a Hermes home is readable (the serving home, the Hermes root, the launch home). A session whose cwd contains the home, such as `~` with the home at `~/.hermes`, still cannot preview `.env`, `auth.json`, `state.db` or skins.
- The confinement check runs before any existence check, so a refused path gives the same error whether the file exists or not.
- A refused path gets JSON-RPC error `4073` with a message that names the setting.
- An unknown value confines like `media`.

`runtime.describe` lists the capability `image-render-confined-v1` while a confined mode is on. That lets a client or relay know that `image.render` cannot read arbitrary host files, without reading the operator's config.

## Why

Some deployments let remote clients use the TUI against a gateway whose host they do not own. For example, a relay forwards a remote machine's TUI, and that machine's files are not on the gateway host. On such a gateway, `image.render` lets any connected client probe and read image-shaped files anywhere on the host. Such deployments have to block the method, and then they lose terminal previews of uploads, screenshots and generated images, which are all files Hermes wrote itself. With `media` or `workspace`, a relay can allow the method and rely on the gateway to keep the reads in bounds. The capability tells the relay that the confinement is on.

The option is in `config.yaml` and not set per connection. The operator owns it, a client cannot turn it off, and the setting applies to every connection that reaches the gateway. The default keeps the current behavior for local single-user setups.

## How

- `tui_gateway/image_render_paths.py`: the mode parser, the media roots and `allowed_path()`.
- `tui_gateway/methods_images.py`: `image.render` applies the check and reads the resolved path. `_image_render_scope()` collects the homes and workspaces of the requesting connection.
- `tui_gateway/ws_legacy_fallback.py` and `tui_gateway/ws.py`: a legacy-fallback request now carries its authority connection in a context variable (`fallback_connection()`), so a sidecar handler can see the sessions that connection subscribes to.
- `gateway/session_controls.py`: `runtime.describe` adds `image-render-confined-v1` when the profile's config confines `image.render`. It leaves the capability out when the config cannot be read.
- The default in `hermes_cli/config_defaults.py`, `cli-config.yaml.example`, and `website/docs/user-guide/configuration.md`.

## Tests

`tests/tui_gateway/test_image_render_paths.py` (new) checks the following through `server.handle_request`:

- The default (key absent, and `any`) still reads an image anywhere.
- `media` reads a staged upload (`stage_image_bytes`), a canonical upload in `cache/images`, a screenshot and a generated image. It refuses `/etc/passwd`, `.env`, `auth.json`, other files in the home, a workspace file and a missing file.
- `workspace` reads under the attached session's cwd, including `.hermes/attachments`. It refuses `..` traversal, a file symlink and a directory symlink that point outside, `/etc/passwd`, and the cwd of a session on another transport.
- `workspace` with a cwd that contains the Hermes home still refuses `.env` and other files in the home. Files beside the home and in its media directories are still readable.
- On a shared-gateway socket, the workspace is the subscribed session's frozen policy cwd. Its parent directory and an unsubscribed session's cwd are refused.
- Mode parsing, including an unknown value. A workspace at the filesystem root is ignored.

`tests/gateway/test_runtime_describe_skin.py` checks that `runtime.describe` lists `image-render-confined-v1` for `media` and `workspace`, and leaves it out for `any` and when the key is absent.

The existing `image.render`, legacy-fallback, ws, contract and describe tests pass unchanged, and `scripts/ci/check_docs_drift.py` passes.
