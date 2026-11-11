# Browser control

Status: built (phase 5). The device side and the plugin's `device-browser` toolset run end to end against a throwaway headless Firefox (section 10). Each release signs the extension on addons.mozilla.org once the AMO secrets are set (section 6.1). No signed release exists yet, so for now it loads only as a temporary add-on.

The goal: the server Hermes agent uses the user's own Firefox on their PC, with the user's logins, in a way the user can see and stop. This document picks the mechanism, defines the tool surface and the exec leg messages, and states the security model.

In this document, "measured" means seen in a run of the end-to-end test on Firefox 157.0.1 (Linux). "Source" means read in Firefox 157.0.1's `omni.ja` or in Hermes Agent v0.22.7. "Inference" means neither.

## 1. Recommendation

Use a Firefox WebExtension that talks to the `hermes-device` binary through native messaging. The binary is the native messaging host. It relays to the `hermes-device run` daemon over a private Unix socket, and the daemon serves `browser` requests from the device plugin on the exec leg.

```text
Hermes pod                 hermes-hub          this PC
device plugin  ==exec leg==> relay ==exec leg==> hermes-device run
  browser_* tools                                 | Unix socket <state dir>/browser.sock (0600, peer uid checked)
                                                  v
                                                hermes-device (native host, started by Firefox)
                                                  | stdin/stdout, native messaging
                                                  v
                                                Hermes extension (background page) -> tabs in the "Hermes" tab group
```

Reasons:

- It works in the user's everyday profile with their logins, and needs no browser restart. The extension is installed once.
- It opens no network port. Only Firefox can start the host, and only for the extension ids in the host manifest's `allowed_extensions`. Web pages cannot reach native messaging.
- The agent works in its own tab group, named "Hermes". The user sees every tab it opens, and can hand it a tab by dragging the tab into the group. The toolbar badge shows when the extension is connected and when an operation runs.
- It works the same on X11 and Wayland, because it does not synthesize OS input.
- The alternatives change the user's browser in ways that break everyday use (section 2).

The main cost: input from a WebExtension is synthetic (`isTrusted` is false). Most sites accept it; some do not (section 9).

## 2. Options evaluated

| Option | Real logged-in profile | Restart needed | Exposure | Verdict |
|---|---|---|---|---|
| WebExtension and native messaging | Yes | No (install once) | No port. Only the allowed extension can start the host | Recommended |
| WebDriver BiDi, `--remote-debugging-port` on the everyday profile | Yes | Yes, Firefox must start with the flag every time | Unauthenticated WebSocket on loopback for every local process | Rejected |
| BiDi "dynamic start" (`remote.experimental.dynamicstart.enabled`) | Yes | No | Same port, but with a per-connection prompt and a banner | Not usable in 157; watch it |
| Marionette (`--marionette`) | Yes | Yes | Unauthenticated TCP port 2828 | Rejected, legacy |
| BiDi on a separate agent profile | No, the user logs in again there | No | Port, but only that profile is at risk | Possible later as a "sandbox" mode |
| Extension with a Firefox container (`contextualIdentities`) for agent tabs | Only the sites the user signs in to in that container | No | As the recommended option | A good refinement of the recommendation (section 12) |
| OS-level or AT-SPI input on top of the extension | Yes | No | Any input the user's session accepts | Fallback for sites that need trusted input; fragile on Wayland |

### 2.1 WebDriver BiDi with --remote-debugging-port

Source, `chrome/remote/content/components/RemoteAgent.sys.mjs` and `server/WebSocketHandshake.sys.mjs`:

- The Remote Agent starts only from the command line flag at startup, so the user's Firefox must always be launched with it.
- The server has no authentication. The handshake checks only that the `Host` header is an IP address or `localhost`, and that there is no `Origin` header (or one listed in `--remote-allow-origins`). This blocks web pages, but any local process of any user can connect to the loopback port and drive the browser.
- `RecommendedPreferences.applyPreferences()` runs at startup with the flag. Among about 110 preferences it turns off Safe Browsing (`browser.safebrowsing.phishing.enabled`, `malware`, `downloads`), application and add-on updates, address and credit card autofill, and crash session restore. They are cleared at `profile-before-change`, so a crash can leave them in `prefs.js`. `remote.prefs.recommended=false` turns this off.

Measured: with `--remote-debugging-port`, pages see `navigator.webdriver === true`. Sites that check it (some sign-in pages do) treat the browser as a bot.

Firefox also shows the striped URL bar and robot icon for the whole session (source, `browser.js` `gRemoteControl`).

### 2.2 BiDi dynamic start

Source: Firefox 157 contains `RemoteAgent.startAtRuntime({isBrowserAutomation: false})`, `moz-src/browser/components/remotecontrol/` (a banner: "An application is now controlling this browser session", with Disconnect) and `shared/webdriver/ConnectionPrompt.sys.mjs` (an "Allow remote control?" prompt for each new session, rememberable until exit). In this mode recommended preferences are not applied (`WebDriverBiDi.sys.mjs`: only when `isBrowserAutomationRunning`). The strings name the feature "Remote control for AI assistants".

`RemoteControlServers.start()` starts both the Remote Agent and Marionette, and writes Marionette's port to `~/.firefox-devtools-mcp/instances/<pid>.port`: the feature is built for Mozilla's firefox-devtools-mcp. In 157.0.1 the pref `remote.experimental.dynamicstart.enabled` can be set, but nothing in the browser calls `start()`: the toolbar button and panel exist only as strings and CSS. So there is no way for the user to turn it on. When Mozilla ships the UI, it gives trusted input and a Firefox-owned consent prompt without a restart. It still listens on two TCP ports that every local process can reach (the prompt is the gate), and it does not show which tabs the agent uses. Revisit then; the tool surface below does not depend on the mechanism.

### 2.3 Marionette

The same startup flag problem and an unauthenticated port as BiDi, with an older protocol. No reason to choose it.

### 2.4 Separate agent profile

A second Firefox instance on its own profile, driven over BiDi, is safe for the user's profile and gives trusted input, but it does not have the user's logins, which is the point of this feature. It could be added later as an option for risky tasks.

## 3. Tool surface (Hermes side)

Hermes v0.22.7 removed the core `browser_navigate`, `browser_snapshot`, `browser_click`, `browser_type` and `browser_vision` tools in favor of `browser_exec` (Python over CDP; source, the stub `tools/browser_tool.py` and `tools/browser_use_cli.py`). The catalog plugin `camofox` brings the old names back as its own toolset and turns the core `browser` toolset off (source, `plugin-catalog/camofox.yaml`, `hermes_cli/left_core_migration.py:485-495`). The device plugin follows that precedent: it registers a toolset `device-browser` with the old names, which models know. The core toolsets of v0.22.7 hold only `browser_exec`, `browser_cdp`, `browser_dialog` and `browser_vault`, so no core tool name collides (source, `toolsets.py`; checked by `test_no_core_tool_collision`). The camofox catalog entry uses six of the same names, so a profile cannot enable both plugins: Hermes refuses the second registration of a name (source, `hermes_cli/plugins.py:479-482`). `browser_exec` cannot be reused: it needs a CDP endpoint, and Firefox has no CDP.

The ACP display formatter reads `title`, `url`, `text` and `snapshot` from the results of `browser_navigate`, `browser_snapshot`, `browser_vision` and `browser_get_images` (source, `acp_adapter/tools.py:496-513, 634`), so results use those keys.

| Tool | Parameters | Device op | Result |
|---|---|---|---|
| `browser_status` | none | `tabs_list` | `available`, the device, the browser and extension versions, and the agent's tabs; or `available: false` and the `reason` |
| `browser_tabs` | `action`: `list`, `open` or `close`; `url` (open); `tab_id` (close) | `tabs_list`, `tab_open`, `tab_close` | Tabs in the Hermes group: `tab_id`, `url`, `title`, `active` |
| `browser_navigate` | `url`; `tab_id` (optional: without it the plugin opens a tab) | `navigate` or `tab_open` | `tab_id`, `url`, `title` |
| `browser_back` | `tab_id` | `back` | `tab_id`, `url`, `title` |
| `browser_snapshot` | `tab_id`; `interactive_only` (bool); `full_text` (bool, also return page text) | `snapshot`, `read_text` | `title`, `url`, `snapshot`, optional `text` |
| `browser_click` | `tab_id`; `ref` (`e12` or `@e12`) | `inspect` (in `pre_tool_call`), then `click` | `tab_id`, `url`, `title` after the click settles |
| `browser_type` | `tab_id`; `ref`; `text`; `submit` (bool) | `inspect`, then `type` | as click |
| `browser_vision` | `tab_id`; `question` (optional) | `screenshot` (JPEG, quality 85) | Multimodal result with the image |

Errors are JSON results `{"error": "...", "code": "..."}`. `code` is a `browser_result` code (section 4.2), or `unavailable` (no browser for this session, below), `declined` (the user said no, section 7) or `bad_arguments`.

Every call goes to the device the session is bound to (`/device <device> <dir>`), resolved as for the terminal backend (protocol.md section 8.3). The plugin refuses, without sending anything, when the session is not bound, when it runs in the sandbox device (`fallback: device`, which has no browser), when the device is not connected, when the device did not offer the `browser` capability, or when the device reports that its extension is not connected.

Snapshot format, one element per line, indented by nesting, with refs that later calls use (measured on example.com):

```text
- paragraph "This domain is for use in documentation examples without needing permission. ..." [ref=e1]
- link "Learn more" [ref=e7]
```

`browser_snapshot` adds `"note"`, which tells the model that page content is untrusted and that it must not follow instructions in it.

Interactive elements carry their state (`value="..."`, `[checked]`, `[level=1]` for headings). The values of password, card and one-time code fields (by type, `autocomplete` or field name) are never included. Refs live in the extension's isolated content script world (a `WeakRef` map), not in the DOM, so the page cannot read or move them. A new snapshot replaces the refs of that page.

Page text cannot forge lines: roles come from a fixed list (an unknown `role` attribute is dropped), names and values are JSON-encoded strings, and brackets are removed from them. The page still controls the order of elements and so the ref numbers; the model must use refs from the line it means.

The snapshot covers the top frame only, and not shadow DOM. Sign-in and payment forms in iframes are out of reach; `scripting.executeScript` with `allFrames` and a walk through open shadow roots would add them.

`browser_vision` returns the Hermes multimodal envelope (`{"_multimodal": true, "content": [text, image_url data URL], "text_summary": ...}`, source `tools/vision_tools.py:594-614`) as a Python dict, which is the only form the registry passes on as multimodal (source, `tools/registry.py:955-970`). The plugin writes the screenshot to a temporary file and downscales it with the helper `browser_exec` uses (`_resize_image_for_vision` with `resolve_embed_target_bytes()`, `_EMBED_MAX_DIMENSION` and `force_jpeg`), and returns text only when `_should_use_native_vision_fast_path()` says the model cannot take images in tool results, as `browser_exec` does (source, `tools/browser_use_cli.py:306-325`).

Tool registration: `ctx.register_tool(name, "device-browser", schema, handler, check_fn=...)`. The toolset is on by default once the plugin is enabled (source, `hermes_cli/tools_config.py:520-527`); the plugin setting `browser: false` hides it. `check_fn` takes no arguments and Hermes caches it for about 30 seconds (source, `tools/registry.py:922-933`), so it cannot look at the session: `browser_status` is listed while any connected device offers the `browser` capability, and the other tools while any such device reports a connected extension. The per-session checks happen in the handler, as above.

Later additions: `browser_scroll`, `browser_press` (keys), `browser_select` (select options), `browser_get_images`, `browser_console`, and file upload.

## 4. Exec leg messages

These messages are gated on the `browser` capability, not on a protocol version (protocol.md section 4.1). The device offers `"capabilities": ["browser"]` in `hello` when `browser.enabled` is on; the plugin answers with the capabilities it accepts in `welcome.capabilities`. Both sides send browser messages only on a leg where the plugin accepted the capability. A capability, not a version bump, because the messages add a feature that only some devices have: the sandbox device and devices with `browser.enabled` off speak the same version 3 as the rest. Older plugins ignore `hello.capabilities` and send no `welcome.capabilities`, so a new device never sends them `browser_status`; older devices send no `capabilities`, so a new plugin never sends them `browser`.

Browser requests are not kept across a drop: when the leg drops, the device cancels every request in flight and the plugin fails every request it waits for with `not_connected` at once, also on version 3 (resume covers commands only). A request might have taken effect in the browser before the drop.

### 4.1 browser (plugin to device)

```json
{"type": "browser", "id": "9c0f...", "op": "snapshot", "args": {"tab_id": 2, "interactive_only": true}, "timeout": 60}
```

| Field | Meaning |
|---|---|
| `id` | Request id, unique per connection (32 hex characters) |
| `op` | One of `tabs_list`, `tab_open`, `tab_close`, `navigate`, `back`, `read_text`, `snapshot`, `inspect`, `click`, `type`, `screenshot`. The device refuses any other op |
| `args` | Op arguments, as in section 3 |
| `timeout` | Seconds. Default <!-- docgen:const pkg=protocol name=BrowserDefaultTimeout print=config -->`1m0s`<!-- /docgen:const -->, capped at <!-- docgen:const pkg=protocol name=BrowserMaxTimeout print=config -->`5m0s`<!-- /docgen:const --> by the device. The plugin waits 10 seconds more for the answer, then sends `kill` and fails the call with `timeout` |

`kill` with the same `id` cancels the request: the device answers `cancelled` at once. The extension does not stop an operation it already started, so `cancelled` means "result abandoned", not "nothing happened"; a click or submit may still take effect.

Op arguments (`args`):

| Op | Arguments | Result |
|---|---|---|
| `tabs_list` | none | `{"tabs": [{"tab_id", "url", "title", "active", "group_id", "agent"}]}` |
| `tab_open` | `url`; `guard` | the tab |
| `tab_close` | `tab_id` | `{"closed": tab_id}` |
| `navigate` | `tab_id`; `url`; `guard` | the tab, after the load completes |
| `back` | `tab_id` | as navigate |
| `read_text` | `tab_id`; `max_chars` (at most 100000) | `{"title", "url", "text", "truncated"}` |
| `snapshot` | `tab_id`; `interactive_only` | `{"title", "url", "snapshot", "element_count"}` |
| `inspect` | `tab_id`, `ref` and `url`, each optional | `{"url", "host", "sensitive": [...], "target": {"kind", "host", "url"}}` for the tab and ref, and `url_host`, the host of `url` as the browser parses it (section 7) |
| `click` | `tab_id`; `ref`; `guard` | the tab after the click settles, plus `clicked` |
| `type` | `tab_id`; `ref`; `text`; `submit`; `guard` | as click, plus `typed` and `submitted` |
| `screenshot` | `tab_id`; `format` (`png` or `jpeg`); `quality` (jpeg, 10 to 100) | `{"tab_id", "url", "title", "data_url"}` |

`guard` is `{"submit_hosts": [...], "visit_hosts": [...], "sensitive": bool}`: what the user approved for this call (section 7). A missing `guard` approves nothing. `navigate` and `tab_open` refuse a URL on another site than the tab's current one (any site, for a new tab) unless `visit_hosts` names it.

### 4.2 browser_result (device to plugin)

```json
{"type": "browser_result", "id": "9c0f...", "ok": true, "result": {"title": "Example Domain", "url": "https://example.com/", "snapshot": "..."}}
{"type": "browser_result", "id": "9c0f...", "ok": false, "code": "not_connected", "error": "the Hermes Firefox extension is not connected"}
```

| `code` | Meaning |
|---|---|
| `not_connected` | Browser control is off on the device, or the extension is not connected or went away during the operation |
| `refused` | The device or the extension refused: unknown op, a tab outside the Hermes group, a tab that shows a non-http(s) page, a non-http(s) URL, a loopback or private network host that is not allowed |
| `timeout` | The operation passed its timeout |
| `cancelled` | Cancelled by `kill` or by the exec leg dropping |
| `failed` | The operation ran and failed; `error` says why |

Size: a PNG screenshot of a 1366x682 page is about 80 KB (measured: 79306 bytes) and the JPEG the plugin asks for is about 70 KB (measured: 69471 bytes). A 4K page is a few MB (inference). The device answers `failed` instead of sending a result over <!-- docgen:const pkg=protocol name=MaxBrowserResult code=false -->16777216<!-- /docgen:const --> bytes (16 MiB). Every hop allows that: the native host reads up to 64 MiB from Firefox, hermes-hub reads up to 64 MiB per message on both hops of the exec leg (source, hermes-hub `internal/device/legs.go`: `maxMessage = 64 << 20`, `SetReadLimit` on the device and the backend connection, and the pump copies messages unchanged), and the Hermes gateway's WebSocket allows 384 MiB (source, `hermes_cli/web_server_boundary.py:26`). No hub change is needed.

### 4.3 browser_status (device to plugin)

```json
{"type": "browser_status", "connected": true, "browser": "Firefox 157.0.1", "extension_version": "0.2.0"}
```

The device sends it right after the `welcome` (and the `resume` on version 3), and again whenever the extension connects or goes away. The plugin keeps the last one per connection for `check_fn`, `browser_status` and `GET /devices` (the Device object gains `"capabilities"` and `"browser"`).

## 5. Device side

- `hermes-device run` with `browser.enabled: true` listens on `<state dir>/browser.sock` (the state directory is `store.state-dir`, by default `hermes-device` in the user config directory). The directory is made mode 0700 and the socket 0600; on Linux it also checks the peer's uid with `SO_PEERCRED` and refuses other users. `browser.allow-hosts` lists loopback or private network host names the agent may open (section 6).
- `hermes-device browser install` (alias `install-host`) writes, for the current user and without root, the host manifest `hermes_device.json` (`"allowed_extensions": ["browser@hermes-device"]`, `"path"` = the running binary with symlinks resolved) and `hermes_device.socket` (the socket path for the configured state directory) into `~/.mozilla/native-messaging-hosts` (Linux) or `~/Library/Application Support/Mozilla/NativeMessagingHosts` (macOS). Run it again after moving the binary or changing `store.state-dir`. `hermes-device browser uninstall` removes both files.
- `hermes-device browser extension --out <dir>` writes the extension's files, which the binary embeds, for loading as a temporary add-on.
- Firefox starts the binary as `<binary> <manifest path> <extension id>`. `main` recognizes that form, checks the extension id, finds the socket (`HERMES_DEVICE_BROWSER_SOCKET`, else the path in `hermes_device.socket` next to the manifest, else the default state directory), dials it and sends `host_hello` with the extension id and its pid. Then it copies native messaging frames both ways and enforces Firefox's 1 MiB limit towards the browser.
- The extension sends `hello` (its version and the browser's name and version). The broker answers with `config` (`allow_hosts`). The broker accepts one extension connection; a newer one replaces the older, and the older gets `replaced` first. An extension that was replaced does not reconnect on its own; a click on its toolbar button makes it take over again. So two Firefox instances with the extension no longer take turns.
- Wire format on the socket: native messaging frames (32-bit little-endian length, then JSON). Requests are `{"type": "request", "id", "op", "args"}`, answers `{"type": "response", "id", "ok", "result" | "error", "code"}`.

## 6. Extension

- Manifest V2 with a persistent background page. Firefox keeps Manifest V2 supported, and a persistent page keeps the native port open. Manifest V3 in Firefox makes host permissions optional at install, which `tabs.captureTab` needs (inference; check before moving).
- Permissions: `nativeMessaging`, `tabs`, `tabGroups`, `scripting`, `webRequest`, `webRequestBlocking`, `dns`, `<all_urls>`. Firefox shows these at install.
- No content scripts are registered, nothing listens for page messages, and there is no `externally_connectable` or web accessible resource. Page code runs only through `scripting.executeScript` during an operation, in the isolated world.
- It acts only on tabs that are in its tab group now (tabs it opened, and tabs the user moved in; a tab dragged out is no longer the agent's). Anything else gets `refused`. Firefox puts tabs opened from a grouped tab (a link with a target, `window.open`, the user's middle click) into the same group (source, `Tabbrowser.sys.mjs`), so those become agent tabs too. Planned: count only tabs the agent opened or the user granted with a context menu item, since another extension can also move tabs into the group.
- It reads, snapshots and screenshots only tabs that show an `http` or `https` page, so going back or redirecting to `file:` or `about:` does not expose those pages. Measured: `read_text` on a user's tab is refused, and so is `navigate` to `file:`.
- It blocks every request from an agent tab (navigations, redirects, links, forms, subresources, WebSockets) to loopback, RFC 1918, link-local, CGNAT (100.64/10, which Tailscale uses), unique-local IPv6 and `.local`, `.localhost`, `.internal` and `.home.arpa` names, unless the host is in `browser.allow-hosts`. Host names are resolved with `dns.resolve`, so public names that point at private addresses are also caught. Without this, an injected instruction could use the user's browser against local services, such as a router or Hermes's own loopback-trusted web server. This needs the `webRequest`, `webRequestBlocking` and `dns` permissions. A name the resolver cannot resolve is blocked. IPv4-mapped and NAT64 IPv6 literals are checked as the IPv4 address they hold (measured: `http://[::ffff:127.0.0.1]/` is refused). DNS rebinding between the check and the connection is still possible (inference). Requests a page's service worker makes have no tab (`tabId` -1) and are not checked (suspected; not tested). `tab_open` puts the tab in the group while it is still blank, so the first request is checked.
- It navigates only to `http` and `https` URLs. `about:`, `file:` and `moz-extension:` are out of reach, and Firefox itself keeps extensions off the domains in `extensions.webextensions.restrictedDomains` (addons.mozilla.org, accounts.firefox.com and others; source, `greprefs.js`).
- It checks a `guard` right before each click and type, in the page (section 7).

### 6.1 Loading and signing

Release and Beta Firefox install only add-ons that Mozilla signed; `xpinstall.signatures.required` has no effect there (Mozilla's documentation; not tested here). Each release tag publishes the extension signed by addons.mozilla.org (AMO) as a self-distributed ("unlisted") add-on. It is not listed on AMO.

Install it once:

1. Open the latest release at https://github.com/USA-RedDragon/hermes-device/releases/latest and download `hermes-device-firefox-<version>.xpi`.
2. In Firefox, open `about:addons`, choose the gear menu, then "Install Add-on From File...", and pick the file. You can also drag the file onto a Firefox window.

Updates: the manifest's `browser_specific_settings.gecko.update_url` is `https://github.com/USA-RedDragon/hermes-device/releases/latest/download/updates.json`. Firefox checks it about once a day (the default `extensions.update.interval`) and installs a newer version from the `update_link` in that file, after it checks the file's `update_hash` (SHA-256). GitHub answers that URL with a redirect to the latest release's asset. Firefox follows redirects for update manifests: `AddonUpdateChecker.sys.mjs` loads the manifest with an XHR and its `CertUtils.BadCertHandler` lets every redirect through, after a certificate check when `extensions.update.requireBuiltInCerts` is on (source, mozilla-firefox `toolkit/mozapps/extensions/internal/AddonUpdateChecker.sys.mjs` and `toolkit/modules/CertUtils.sys.mjs`; not yet tested with a real update). To check now, choose "Check for Updates" in the gear menu of `about:addons`.

What the Release workflow does (`.github/workflows/release.yaml`, job `firefox-extension`, after GoReleaser published the release):

1. It builds the extension with `hermes-device browser extension --out DIR --ext-version X`, where X is the tag without its leading `v`. Only the `version` line differs from the files in `extension/firefox`.
2. It runs `web-ext lint`, the same validator AMO runs, so problems show in the job log. The Tests workflow runs the same lint on every push.
3. It signs with `web-ext sign --channel unlisted`, with the repository secrets `AMO_JWT_ISSUER` and `AMO_JWT_SECRET`.
4. It checks that the signed file has a Mozilla signature and the expected version, then uploads `hermes-device-firefox-<version>.xpi` and `updates.json` to the release.

The job skips signing, with a notice in its log, and the release then has no `.xpi` and no `updates.json`, when:

- The secrets are not set (forks, or before they are added). The binaries and images are still released.
- The tag is a pre-release or has build metadata (`v1.2.0-rc.1`). Firefox versions are one to four dot-separated numbers, and no such number for `1.2.0-rc.1` sorts before `1.2.0`.
- The release already has the `.xpi` (a re-run). AMO accepts each version only once.

While the latest release has no `updates.json`, Firefox's update check fails quietly and the installed version stays. The next signed release fixes that.

One-time setup for the maintainer:

1. Sign in at https://addons.mozilla.org/developers/addon/api/key/ and accept the developer agreement. Generate credentials. The JWT issuer looks like `user:12345:67`.
2. In a clone of the repository, run `gh secret set AMO_JWT_ISSUER` and `gh secret set AMO_JWT_SECRET`, and paste each value at the prompt.
3. Tag a release. The first upload creates the add-on on AMO under the id `browser@hermes-device`. That id stays fixed: the host manifest's `allowed_extensions` names it.

AMO runs automated validation. Unlisted add-ons are signed without a review by hand, but Mozilla may review them later (Mozilla's Extension Workshop documentation). AMO requires a new `version` for every upload, and `data_collection_permissions` in the manifest (present: `"required": ["none"]`). The extension has no minified or generated code, so no source code upload is needed.

For development, load the extension as a temporary add-on instead:

1. `hermes-device browser extension --out ~/hermes-browser-extension`. A release binary writes its own version into the manifest; `--ext-version` sets another.
2. In Firefox, open `about:debugging#/runtime/this-firefox`, choose "Load Temporary Add-on..." and pick `manifest.json` in that directory.

A temporary add-on stays until Firefox restarts. While it is loaded, it takes the place of an installed add-on with the same id (inference from Mozilla's documentation; not tested). Firefox Developer Edition, Nightly and ESR can instead set `xpinstall.signatures.required` to false and install an unsigned `.xpi` permanently (inference from Mozilla's documentation; not tested).

## 7. Permission and confirmation model

The agent acts with the user's logins, so a page's text is untrusted input to the model (prompt injection), and some actions cannot be undone.

| Action | Default |
|---|---|
| Snapshot, read text, screenshot, back, list or close tabs | Allowed |
| Click or type that does not submit and does not leave the site | Allowed |
| Open a site the session did not use yet: `browser_navigate` or `browser_tabs` `open` to another host than the tab's, or a click on a link to another host | Ask once per site per session (`rule_key` `browser:visit:<site>`) |
| Submit a form: a click on a submit control, or `browser_type` with `submit` | Ask once per site per session (`rule_key` `browser:submit:<site>`, the site of the form's action) |
| Any click or type on a page with a password, payment or one-time code field | Ask every time |
| Downloads, file uploads, permission prompts, browser dialogs | Not offered |

A site is the host name in lower case without a leading `www.`. It is not the registrable domain: `accounts.example.com` and `example.com` are separate sites. That asks more often, but an approval never covers a host the user did not see.

How the plugin asks:

1. Its `pre_tool_call` hook runs for `browser_navigate`, `browser_tabs`, `browser_click` and `browser_type`. It first drops any grant left from an earlier call with the same arguments (one the user denied). For a click or type it sends `inspect` for the target ref; for a navigate or a new tab it sends `inspect` with the URL (and the tab), so the site it asks about is the host the browser itself parses from the URL, not the plugin's reading of it. When that `inspect` fails, it asks about the raw URL and approves no host, so the extension refuses to open another site. `inspect` returns the page's host, the kinds of secret fields on the page (`sensitive`: `password` for `type=password` or a password `autocomplete`; `payment` for `cc-*` `autocomplete`, card, CVC or expiry field names, or an iframe of a known payment provider; `one_time_code` for `autocomplete="one-time-code"` or OTP, 2FA or verification code field names) and what the ref is (`submit` with the host of the button's `formaction` or its form's action, for a submit button around the ref, a label's control, or a button outside its form through `form=`; else `link` with its host; `field`; or `other`).
2. For a submit or a new site it returns `{"action": "approve", "message": ..., "rule_key": ...}`. Hermes sends that to its approval gate, which asks wherever the user talks to the agent (CLI prompt, web UI, Telegram buttons, the ACP editor) and offers once, session, always and deny. "Session" remembers the rule key for the session and "always" for good, in `command_allowlist` (source, `hermes_cli/plugins.py:1987-2016`, `tools/approval.py:996-1068`, `412-425`). The gate approves at once in yolo mode or with `approvals.mode: off`, and denies when no human can answer (cron, `-q`, webhooks) unless `approvals.cron_mode`, `single_query_mode` or `unattended_mode` say approve.
3. On a page with secret fields the hook asks nothing. The handler calls `request_elicitation_consent` (source, `tools/approval_prompt.py:296-376`) without a `remember_key`, so the user can only allow this one call or deny it. Yolo and `approvals.mode: off` do not skip it, and it denies when no human can answer.
4. The hook hands the handler what was approved, and the handler sends it as the `guard` of the `click`, `type`, `navigate` or `tab_open`. The extension classifies the element and the page again in the page before it acts, and again after the synthetic pointer and mouse events and right before the activation (page handlers of those events can rewrite links and forms). It refuses (`refused`) a submit to a host not in `submit_hosts`, a link or navigation to another site not in `visit_hosts`, and any action on a page with secret fields without `sensitive`. So a page that changed between the hook and the action cannot slip a submit, a new site or a secret field past the user, and a handler call without the hook (or after a failed `inspect`) can do only what needs no approval.

The hook's `inspect` runs inside Hermes's hook timeout (`plugins.hook_callback_timeout`, default 30 seconds; the plugin gives `inspect` 15). When it fails, a navigate still asks, and a click or type goes ahead with an empty guard.

Known gaps:

- Navigation that a page starts by itself after a click (a script that sets `location`, a redirect, a `target` link) is not classified. The extension's private network block still applies to requests from agent tabs.
- Script-driven submits are not seen: a `type=button` or `role=button` control that posts with `fetch`, as most single-page apps do, counts as a plain click and needs no approval. Only a page with secret fields then asks.
- Sensitive field detection is heuristic. A sign-in form in a cross-origin iframe (other than the listed payment providers) or in shadow DOM is not seen.
- Planned: the user's block list (banks, the password manager) in the device config, so a compromised pod cannot remove it.

The user can stop everything at any time: close the Hermes tab group, disable the extension, or stop `hermes-device run`.

## 8. Security analysis

Who can drive the browser:

- Web pages: no. Native messaging is reachable only from the extension's privileged pages. The extension has no page-facing message channel. Content script refs are invisible to the page.
- Other users on the machine: no. The socket is 0600 in a 0700 directory, and the broker checks the peer uid on Linux. There is no TCP port.
- Other processes of the same user: yes, by connecting to the socket and pretending to be a host, or by editing the manifest. They can already read the Firefox profile and the device token, so this is not a new boundary. The broker accepts only `host_hello` for the expected extension id, which stops accidents, not attacks.
- Other extensions: no. Firefox starts the host only for ids in `allowed_extensions`, and the host checks the id again.
- Other extensions: they cannot talk to the host, but one with the `tabs` permission may be able to move a tab into the Hermes group and so hand it to the agent (suspected, not tested).
- The Hermes pod: yes, within the rules above. A compromised pod can do what the agent can: act in the Hermes tab group with the user's cookies. It cannot read tabs outside the group, cannot read `file:` or `about:` pages, and cannot reach loopback or private network hosts the device did not allow. This matches the exec leg, where a compromised pod can run commands in the device's roots.
- The hub: relays messages and cannot mint them except as the pod would (protocol.md section 10).
- The approvals in section 7 are decided by the plugin in the pod. A compromised pod can send any `guard`; the extension's checks stop mistakes and changed pages, not a hostile pod.

What the agent can see: the content of pages in its group, including whatever the user's cookies show there. Password field values are omitted from snapshots, but a screenshot shows what is on screen. Users should not move tabs with secrets into the group.

Prompt injection: page text reaches the model. The confirmation rules in section 7 and the scope rules above limit what an injected instruction can do. The plugin SHOULD wrap page text in a marked block that tells the model it is untrusted data.

## 9. Per platform

| Platform | Needs |
|---|---|
| Linux (X11 or Wayland) | A Firefox with the `tabs.group` API (measured on 157; the extension asks for 139 or later, which is an inference from Mozilla's release notes, not tested). Without the API the extension still works but shows no group. `hermes-device browser install` writes `~/.mozilla/native-messaging-hosts/hermes_device.json` (measured: Firefox 157 reads it from `$HOME/.mozilla/native-messaging-hosts`). The signed extension from a release (section 6.1). Nothing depends on the display server |
| Linux, Firefox as Snap | The Snap sandbox does not see host manifests in `~/.mozilla` by default and cannot run binaries outside the snap without the `native-messaging` portal (inference from Snap's confinement; not tested). Arch packages are not Snaps |
| Linux, Firefox as Flatpak | Same problem; needs the WebExtensions portal or a manual `--filesystem` override (inference; not tested) |
| macOS | Manifest in `~/Library/Application Support/Mozilla/NativeMessagingHosts/` (the code handles it). No `SO_PEERCRED`: the socket's mode is the only check (it could use `LOCAL_PEERCRED`) |
| Windows | Not supported, as for the rest of hermes-device. Native hosts there are registered in the registry |

## 10. Code and tests

- `internal/browser/nativemsg`: native messaging framing.
- `internal/browser/broker`: the daemon's socket server and request calls.
- `internal/browser/host`: the native host relay and its manifest.
- `extension/firefox`: the extension (`manifest.json`, `background.js`), embedded in the binary. The `firefox-extension` job in `.github/workflows/release.yaml` signs and publishes it.
- `internal/device`: the `browser` capability, `browser`, `browser_result` and `browser_status` on the exec leg; `cmd/run.go` starts the broker when `browser.enabled` is on; `cmd/browser.go` is `hermes-device browser`.
- `plugin/device/browser.py`: the `device-browser` toolset and its `pre_tool_call` hook. `plugin/device/core.py` and `dashboard/plugin_api.py`: capability negotiation and browser calls on the exec leg.
- Unit tests: `internal/device/browser_test.go`, `internal/browser/*/..._test.go`, and `plugin/device/tests/test_browser.py` (the tools, the approvals and the negotiation with a fake device and browser, through a fresh Hermes `ToolRegistry`).
- `internal/browser/bidi` and `internal/browser/firefoxtest`: an end-to-end test that builds the binary, writes the host manifest into a fresh `HOME`, starts headless Firefox on a fresh profile with `--no-remote`, loads the extension as a temporary add-on over BiDi (`webExtension.install`; temporary add-ons skip signing), and drives it through the broker. When `hermes` is on `PATH` it then runs the real device client against the broker, with `plugin/device/tests/test_browser_e2e.py` on the other end of the exec leg in the Hermes runtime, and drives Firefox through the plugin's tools and approval hook.

Run the end-to-end test (it needs the network for example.com):

```sh
HERMES_BROWSER_E2E=1 go test -count=1 -v -run TestFirefoxEndToEnd ./internal/browser/firefoxtest/
```

It opens example.com in a new tab in the Hermes group, checks the title, the text, the snapshot and a PNG screenshot, checks that a link to another site is refused without approval and followed with it, fills a form on a local test server and checks that its submit is refused without approval and works with it, checks that opening a new site without approval is refused, checks the private network block (also for IPv4-mapped IPv6 literals) and the refusals, checks that a submit button inside a link and one outside its form are submits, and checks that every action on a page with password and one-time code fields needs confirmation. The plugin part does the same through `browser_navigate`, `browser_snapshot`, `browser_vision`, `browser_click`, `browser_back`, `browser_type` and `browser_tabs`. BiDi is used only to install the unsigned extension in the test; the browser control itself goes through the extension.

## 11. Enable it

On the server: the device plugin of this release, with the toolset on (the default; `plugins.entries.device.settings.browser: false` turns it off). In a profile that also enables the camofox plugin, only one of the two gets the tool names.

On the computer:

1. Set `browser.enabled: true` in the hermes-device config (and `browser.allow-hosts` for any LAN or loopback host the agent may open).
2. Run `hermes-device browser install` as the user who runs Firefox.
3. Restart `hermes-device run`.
4. Install the signed extension from a release (section 6.1). Its toolbar button shows "Hermes: connected" when it reaches the daemon.
5. Bind a session to this device with `/device <device> <dir>`. `browser_status` in that session shows whether the agent can use the browser.

## 12. Open risks

- The measurements come from a Firefox started with `--remote-debugging-port` (needed to load the unsigned extension in the test), which applies the Remote Agent's recommended preferences, among them the popup blocker off. Behavior in the user's own Firefox can differ, so the first manual test in a normal profile is still due.
- Synthetic input: `isTrusted` is false for every click and key. Sites that check it, canvas editors and some anti-bot flows will not work. Firefox gives WebExtensions no debugger API. If this blocks real use, the BiDi dynamic start mode (2.2) is the path, once Firefox ships it.
- Screenshots of background tabs: measured headless only. In a headed Firefox, `tabs.captureTab` of a tab that was never shown may return a blank or stale image (inference). The fix is to activate the tab in its own window briefly, or to open the Hermes group in a separate window.
- Signing: every release signs the extension on AMO (section 6.1). AMO validation rules can change and fail the signing step, and Mozilla may still review the unlisted add-on by hand.
- Snap and Flatpak Firefox (section 9).
- Iframes and shadow DOM are not covered (section 3).
- Cancelled operations are not stopped in the browser (section 4.1).
- The approval rules have the gaps listed in section 7.
- A Firefox container for agent tabs would limit the agent to sites the user chose to sign in to there, which shrinks what a compromised pod or an injected instruction can reach. It costs the user one sign-in per site. Worth trying before release.
- The extension keeps its group and tab ids in memory: after a Firefox restart it creates a new Hermes group, and old Hermes tabs are no longer the agent's.
