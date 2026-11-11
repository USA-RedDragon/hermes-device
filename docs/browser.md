# Browser control

Status: built (phase 5). The device side and the plugin's `device-browser` toolset run end to end against a throwaway headless Firefox (section 10). Frames, shadow DOM, scrolling, keys, hover, waiting, full-page screenshots, scripts (`browser_eval`), Salesforce click confirmation and starting Firefox from the device were added in phase 6. Firefox must be signed in to hermes-hub as the device's owner before the agent can use it (section 13). Each release signs the extension on addons.mozilla.org once the AMO secrets are set (section 6.1). No signed release exists yet, so for now it loads only as a temporary add-on.

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
                                                  | HTTPS, sign-in only (section 13)
                                                  v
                                                hermes-hub /browser/authorize, /api/browser/token, /api/browser/revoke
```

Reasons:

- It works in the user's everyday profile with their logins, and needs no browser restart. The extension is installed once.
- It opens no network port. Only Firefox can start the host, and only for the extension ids in the host manifest's `allowed_extensions`. Web pages cannot reach native messaging.
- The agent works in its own tab group, named "Hermes". The user sees every tab it opens, and can hand it a tab by dragging the tab into the group. The toolbar badge shows when the extension is connected and when an operation runs.
- It works the same on X11 and Wayland, because it does not synthesize OS input.
- The alternatives change the user's browser in ways that break everyday use (section 2).
- The user signs Firefox in to the hub explicitly, and the hub refuses browser operations for a device whose browser is not signed in as the device's owner (section 13). Being the same OS user as the daemon is not enough.

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
| `browser_status` | none | `tabs_list` | `available`, the device, `why` the session uses it (section 3.2), the browser and extension versions, `signed_in`, the hub `user`, the agent's tabs, and `devices`: the user's connected computers with browser control and their Firefox state; or `available: false`, the `reason`, such as "Firefox is not signed in to Hermes", and `devices` |
| `browser_use_device` | `device` (a name or id from `browser_status`, or `default`); `job_id` (optional) | none | `ok`, the device, or `job_id` and the device for a job (section 3.2) |
| `browser_tabs` | `action`: `list`, `open` or `close`; `url` (open); `tab_id` (close) | `tabs_list`, `tab_open`, `tab_close` | Tabs in the Hermes group: `tab_id`, `url`, `title`, `active` |
| `browser_navigate` | `url`; `tab_id` (optional: without it the plugin opens a tab) | `navigate` or `tab_open` | `tab_id`, `url`, `title` |
| `browser_back` | `tab_id` | `back` | `tab_id`, `url`, `title` |
| `browser_snapshot` | `tab_id`; `interactive_only` (bool); `full_text` (bool, also return page text); `text_only` (bool, only the text); `text_offset`; `wait_for` (text) or `wait_selector` (CSS) and `wait_seconds` (at most 30) | `wait`, `snapshot`, `read_text` | `title`, `url`, `snapshot`, optional `text`, `text_truncated`, `text_next_offset`, `waited` |
| `browser_click` | `tab_id`; `ref` (`e12` or `@e12`) | `inspect` (in `pre_tool_call`), then `click` | `tab_id`, `url`, `title` after the click settles |
| `browser_type` | `tab_id`; `ref`; `text`; `submit` (bool) | `inspect`, then `type` | as click |
| `browser_vision` | `tab_id`; `question` (optional); `full_page` (bool) | `screenshot` (JPEG, quality 85) | Multimodal result with the image |
| `browser_screenshot` | `tab_id`; `full_page` (bool) | `screenshot` (JPEG, quality 85) | `tab_id`, `url`, `title`, the saved file's `path`, and `show_user`: `MEDIA:<path>` |
| `browser_press` | `tab_id`; `key` (`Enter`, `Tab`, `Shift+Tab`, `Escape`, `Space`, `ArrowDown`, `Control+a`, one character, ...); `ref` (optional: the element to press it on, else the focused element) | `inspect` (in `pre_tool_call`), then `press` | `tab_id`, `url`, `title`, `pressed`, `default_action`, `focused` |
| `browser_hover` | `tab_id`; `ref` | `hover` | `tab_id`, `url`, `title` |
| `browser_scroll` | `tab_id`; `direction` (`down`, `up`, `top`, `bottom`) and `pages`; `ref` (alone: bring it into view; with `direction`: scroll the area that holds it); `to_text`; `frame` | `scroll` | `scrolled` (`page` or `area`), `scroll_y`, `scroll_height`, `view_height`, `at_top`, `at_bottom`, `found` |
| `browser_eval` | `tab_id`; `script` (the body of an async function); `frame` (optional, such as `f2`); `allow_writes` (bool); `max_chars` | `inspect` (in `pre_tool_call`), then `eval` | `tab_id`, `url`, `title`, `read_only`, and `result` (the returned value) or `result_text` and `truncated` when the JSON is longer than `max_chars`; `note` as for snapshots |

Errors are JSON results `{"error": "...", "code": "..."}`. `code` is a `browser_result` code (section 4.2), or `unavailable` (no browser for this session, below, also when Firefox is not signed in), `declined` (the user said no, section 7), `started` (Firefox was closed and is started now: make the same call again, section 5.1) or `bad_arguments`.

Every call goes to the session's browser device (section 3.2). The plugin refuses, without sending anything, when the session fails closed (protocol.md section 8.3), when it has no browser device, when the device is not connected, when the device did not offer the `browser` capability, when the device reports that its extension is not connected, or when the status says Firefox is not signed in (section 13).

Snapshot format, one element per line, indented by nesting, with refs that later calls use (measured on example.com):

```text
- paragraph "This domain is for use in documentation examples without needing permission. ..." [ref=e1]
- link "Learn more" [ref=e7]
```

Frames (measured on a local page with a same-origin and a cross-origin iframe): each visible `iframe` gets a line with its frame name, and its elements follow it, indented, with refs that carry the frame:

```text
- heading "Frames" [level=1] [ref=e1]
- iframe "same" [frame=f1]
  - paragraph "inner 127.0.0.1" [ref=f1e1]
  - button "Inner button" [ref=f1e2]
- iframe "cross" [frame=f2]
  - paragraph "inner 127.0.0.2" [ref=f2e1]
  - button "Inner button" [ref=f2e2]
- button "Shadow button" [ref=e2]
```

The extension runs the snapshot in every frame of the tab (`scripting.executeScript` with `allFrames`) and joins the outlines under the frame lines; `runtime.getFrameId` maps each `iframe` element to its frame. Frame names (`f1`, `f2`) stay the same for a frame while the tab lives. A frame that shows no http(s) page, or that Firefox does not let the extension into, shows `[frame not readable]`. Shadow DOM: the outline follows the rendered tree, so an element with a shadow root shows its shadow tree (open or closed, through Firefox's `openOrClosedShadowRoot`) and a `slot` shows its assigned elements. Text (`full_text`) is the top frame's `innerText`, then the text of shadow trees that `innerText` leaves out, then each frame's text under a `[frame f2: <host>]` line. It is cut at 100,000 characters per call; `text_next_offset` gives the start of the next part.

`browser_snapshot` adds `"note"`, which tells the model that page content is untrusted and that it must not follow instructions in it.

Interactive elements carry their state (`value="..."`, `[checked]`, `[level=1]` for headings). The values of password, card and one-time code fields (by type, `autocomplete` or field name) are never included. Refs live in the extension's isolated content script world (a `WeakRef` map), not in the DOM, so the page cannot read or move them. A new snapshot replaces the refs of that page.

Page text cannot forge lines: roles come from a fixed list (an unknown `role` attribute is dropped), names and values are JSON-encoded strings, and brackets are removed from them. The page still controls the order of elements and so the ref numbers; the model must use refs from the line it means.

Secret field detection (section 7) covers every frame and shadow trees: a password field in any frame of the tab makes every action in the tab ask.

`browser_vision` returns the Hermes multimodal envelope (`{"_multimodal": true, "content": [text, image_url data URL], "text_summary": ...}`, source `tools/vision_tools.py:594-614`) as a Python dict, which is the only form the registry passes on as multimodal (source, `tools/registry.py:955-970`). The plugin writes the screenshot to a temporary file and downscales it with the helper `browser_exec` uses (`_resize_image_for_vision` with `resolve_embed_target_bytes()`, `_EMBED_MAX_DIMENSION` and `force_jpeg`), and returns text only when `_should_use_native_vision_fast_path()` says the model cannot take images in tool results, as `browser_exec` does (source, `tools/browser_use_cli.py:306-325`).

`browser_vision` shows the screenshot only to the model. `browser_screenshot` is for the user: it saves the screenshot in the Hermes server's screenshot cache, in `<HERMES_HOME>/cache/screenshots/hermes-device/<session>/shot-<UTC time>-<random>.jpg` (folder mode 0700, file mode 0600), and returns `show_user`, a `MEDIA:<path>` directive, with a note that tells the model to put that line in its reply. Hermes delivers files to users with `MEDIA:<path>` directives in the reply text (source, `gateway/platforms/base.py` `extract_media`), and hermes-hub shows such a file in its web page and sends it to Telegram. The hub reads the file through the Hermes web server's path-bound file tickets (`POST /api/files/ticket`, then `GET /api/fs/download`). The tool does not give the image to the model: a small model is more likely to pick a tool named for what the user asked ("take a screenshot") than to set a flag on `browser_vision`, and a separate tool keeps the two results simple. Each session has its own folder. Before it saves a file, the plugin deletes screenshots older than 7 days, then the oldest ones over 50 in each session folder, and removes empty session folders. So a busy chat never removes another chat's screenshots. The age limit keeps a chat's screenshots for a week of visits back to it; 50 covers a long browsing task and bounds the disk one chat takes. The plugin also registers Hermes's `on_session_delete` hook and removes the folders of deleted sessions, including the compression and subagent sessions Hermes deletes with them. It keeps them for an archived session, which can come back, and for a session that moved to another profile (`profile_repair`). Hermes runs the hook only in the process that deletes the session. When that process did not load the plugin, or the session id has characters that the folder name replaces, the folder waits for the age limit.

Tool registration: `ctx.register_tool(name, "device-browser", schema, handler, check_fn=...)`. The toolset is on by default once the plugin is enabled (source, `hermes_cli/tools_config.py:520-527`); the plugin setting `browser: false` hides it. `check_fn` takes no arguments and Hermes caches it for about 30 seconds (source, `tools/registry.py:922-933`), so it cannot look at the session: `browser_status` is listed while any connected device offers the `browser` capability, and the other tools while any such device reports a connected and signed-in extension. The per-session checks happen in the handler, as above.

Synthetic key events have no default action in Firefox, so `press` does the usual one itself when the page did not cancel the `keydown`: Enter and Space click a button, link or checkbox, Enter in a field submits its form (both checked against the guard like a click or a submit), Tab and Shift+Tab move the focus, the arrow, page, Home and End keys scroll when the focus is not in a text field, a character is typed into a text field, Backspace deletes, and Control+a selects all. `hover` sends `pointerover`, `pointerenter`, `mouseover`, `mouseenter`, `pointermove` and `mousemove` at the element's center. A full-page screenshot uses `tabs.captureTab` with a `rect` of the whole document (measured: a 5,000 pixel page comes back whole), at most 4,096 by 16,384 CSS pixels and 16 million pixels (larger pages are scaled down, and `clipped` is set when the page is longer). A page that scrolls inside an inner element shows only one screen this way; scroll it and look again.

Later additions: `browser_select` (select options), `browser_get_images`, `browser_console`, and file upload.

### 3.1 Hidden built-in tool

Hermes v0.22.7 has a built-in `browser_vault` tool in its core `browser` toolset, next to `browser_exec`, `browser_cdp` and `browser_dialog` (source, `toolsets.py`). It drives Hermes's own CDP browser, not the user's Firefox, and Hermes has no switch to turn off one tool of a toolset. The plugin removes `browser_vault` from the tool registry when it loads, and again at each session start (the built-in tools can be imported after the plugin), so the model never sees it. `browser_exec` and the rest stay.

Hermes lets a plugin remove a built-in tool only with the operator's opt-in (source, `tools/registry.py` `deregister`, `_PluginOverridePolicy`): set `plugins.entries.device.granted_capabilities: [tools.override]` in the Hermes `config.yaml` (the older `plugins.entries.device.allow_tool_override: true` also works). The removal runs from the plugin's own module, so the registry applies that check. Without the opt-in the plugin logs one warning and the tool stays. `tools.override` also lets the plugin override other built-in tools; it uses it for nothing else.

### 3.2 Browser device

The browser device is the computer whose Firefox a session uses. It is chosen apart from the device that runs the session's commands, so a session that runs in the sandbox (protocol.md section 8.10), such as a web chat or a scheduled job, can still use the Firefox on the user's computer.

Each session acts for one hub user. The plugin knows the user of each device from the hub assertion (`sub`, protocol.md section 3.2). The user of a session bound to a computer is that computer's user. A session that no computer serves (the sandbox, or the pod with `fallback: local`) carries no user the plugin can see, because every chat reaches Hermes through the hub's one dashboard login. Its user is the plugin setting `user` (protocol.md section 8.1): the hub user name of the one person who uses this Hermes. Leave it unset on a backend that more than one hub user may use; such sessions then have no browser device, and only sessions bound to a computer use a browser. While a computer of another hub user is connected, the plugin logs an error and gives such sessions no browser device either, so a wrong setting on a shared backend fails closed.

The plugin picks the browser device in this order:

1. In a run of a scheduled job: the job's browser device, and only that one. A run of a job that has none has no browser.
2. The device chosen with `browser_use_device` in the session or in an ancestor of its lineage (a compression child, subagent or branch inherits it).
3. The computer the session is bound to.
4. For a session that no computer serves: the user's one connected computer with browser control. With none, or more than one, the call fails and says to choose one with `browser_use_device`.

Every device must belong to the session's user. Service devices, such as the sandbox, are never browser devices. A stored choice records the device's user. A choice whose user is not the session's user is refused, and so is a device that now reports another user than its choice. `browser_status` lists only the session's user's computers, and none when the user is unknown.

A run of a scheduled job finds its job through the context Hermes binds for the run (`gateway.session_cron.current_execution`, with the run's session id, which must be the session or an ancestor of it), else through the frozen policy of a live cron session, which holds the job (`request_json.cron_job`). The agent cannot set either.

When `cronjob_manage` creates a job (Hermes's `cron.scheduler.create_job_with_scheduler_registration`, which the plugin wraps), the job gets the browser device of the session that created it, by rules 2 to 4, when the session has one that is connected. When that choice cannot be read or saved, the plugin removes the job again and the create fails, so no job runs with another browser device than its creator meant. A job created without one (the session had none, or the computer was offline), and a job created on the server, has no browser until the user gives it one. `browser_use_device` with `job_id` (an id or a name) sets, or with `default` clears, a job's browser device from any chat of the job's user; a job whose browser device belongs to another user cannot be changed. A run of a scheduled job cannot use `browser_use_device` at all.

Scheduled work that drives a browser with nobody watching needs the user's OK each time, through Hermes's elicitation consent (`request_elicitation_consent`, which offers only allow once or deny and remembers nothing): `cronjob_manage` `create` in a session that has a browser device (only such a job gets one), `update`, `run` and `resume` of a job that has one, and `browser_use_device` with `job_id`. Without it, a prompt injection in a chat could set up a job that reads the user's logged-in sites later. With no user present these calls are blocked. Any unexpected error in this check blocks the call.

A run whose browser device is offline, or was removed from the hub (a revoked device never connects again), fails each browser call at once with a message that names the device, such as "device upstairs-pc [d_...], the browser device of scheduled job ..., is not connected". It never falls back to another computer. The hub's own checks still apply to every request (section 13.4): the browser must be signed in as the device's owner.

The choices are in `$HERMES_HOME/device/browser.json` (mode 0600, written atomically, changed under an exclusive lock on `browser.json.lock`), keyed by session id and job id. It also marks every job created in a session that a device or the sandbox serves (protocol.md section 8.3 rule 6). Every read checks whether the file changed, so other Hermes processes and a repaired file are seen without a restart. Choices of deleted sessions are dropped through `on_session_delete`, and choices of removed jobs when the next job is created. While the file cannot be read, or holds an entry without a device or user, every browser call fails.

The run's commands still go where protocol.md section 8.3 sends them: the sandbox for a scheduled job with `fallback: device`.

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
| `op` | One of `tabs_list`, `tab_open`, `tab_close`, `navigate`, `back`, `read_text`, `snapshot`, `inspect`, `click`, `type`, `press`, `hover`, `scroll`, `wait`, `eval`, `screenshot`. The device refuses any other op |
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
| `back` | `tab_id` | as navigate. Refused in a tab that ran a script (section 7.2) |
| `read_text` | `tab_id`; `max_chars` (at most 100000); `offset` | `{"title", "url", "text", "truncated", "total_chars", "next_offset"}`, every frame's text (section 3) |
| `snapshot` | `tab_id`; `interactive_only` | `{"title", "url", "snapshot", "element_count"}`, every frame (section 3) |
| `inspect` | `tab_id`, `ref` (`e12` or `f2e12`), `frame` (`f2`), `focus` (bool: the focused element) and `url`, each optional | `{"url", "host", "top_host", "sensitive": [...], "protected", "scripted", "target": {"kind", "host", "url", "form", "label"}}`: `host` is the frame's host, `sensitive` covers every frame of the tab, `protected` is true on a protected site (section 7.1), `label` is the element's text (at most 80 characters); and `url_host`, the host of `url` as the browser parses it (section 7) |
| `click` | `tab_id`; `ref`; `guard` | the tab after the click settles, plus `clicked` |
| `type` | `tab_id`; `ref`; `text`; `submit`; `guard` | as click, plus `typed` and `submitted` |
| `press` | `tab_id`; `key`; `ref` (optional); `guard` | as click, plus `pressed`, `default_action` (`typed`, `clicked`, `submitted`, `focus moved`, `scrolled`, `none`, or `cancelled by the page`) and `focused` |
| `hover` | `tab_id`; `ref` | the tab, plus `hovered` |
| `scroll` | `tab_id`; `direction` and `pages` (0.1 to 20), `ref`, `to_text`, `frame` | `{"scrolled", "scroll_y", "scroll_height", "view_height", "at_top", "at_bottom", "found", "frame"}` |
| `wait` | `tab_id`; `text` or `selector`; `timeout_ms` (at most 30000) | `{"found", "waited_ms", "frame"}`; checks every frame every 300 ms |
| `eval` | `tab_id`; `script`; `frame`; `allow_writes`; `max_chars` (at most 1000000, default 100000); `guard` | `{"tab_id", "url", "title", "frame", "read_only", "result"}` or `"result_text", "truncated", "result_chars"`; `hint` when the script returned nothing (section 7.2) |
| `screenshot` | `tab_id`; `format` (`png` or `jpeg`); `quality` (jpeg, 10 to 100); `full_page` | `{"tab_id", "url", "title", "data_url"}`, plus `page_height`, `clipped` and `scale` for a full page |

`guard` is `{"submit_hosts": [...], "visit_hosts": [...], "sensitive": bool, "confirmed": bool}` for actions and `{"script_hosts": [...], "write_hosts": [...], "sensitive": bool}` for `eval`: what the user approved for this call (section 7). `sensitive` and `confirmed` mean the user confirmed this one call. A missing `guard` approves nothing. `navigate` and `tab_open` refuse a URL on another site than the tab's current one (any site, for a new tab) unless `visit_hosts` names it.

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
| `not_signed_in` | Firefox is not signed in to the hub as the device's owner. The hub answers it itself and does not forward the request, or the device refuses it (section 13) |

Size: a PNG screenshot of a 1366x682 page is about 80 KB (measured: 79306 bytes) and the JPEG the plugin asks for is about 70 KB (measured: 69471 bytes). A 4K page is a few MB (inference). The device answers `failed` instead of sending a result over <!-- docgen:const pkg=protocol name=MaxBrowserResult code=false -->16777216<!-- /docgen:const --> bytes (16 MiB). Every hop allows that: the native host reads up to 64 MiB from Firefox, hermes-hub reads up to 64 MiB per message on both hops of the exec leg (source, hermes-hub `internal/device/legs.go`: `maxMessage = 64 << 20`, `SetReadLimit` on the device and the backend connection, and the pump copies messages unchanged), and the Hermes gateway's WebSocket allows 384 MiB (source, `hermes_cli/web_server_boundary.py:26`). No hub change is needed.

### 4.3 browser_status (device to plugin)

```json
{"type": "browser_status", "connected": true, "signed_in": true, "user": "alice", "browser": "Firefox 157.0.1", "extension_version": "0.2.0"}
{"type": "browser_status", "connected": false, "signed_in": false, "reason": "Firefox is not signed in to Hermes", "browser": "Firefox 157.0.1", "extension_version": "0.5.0"}
{"type": "browser_status", "connected": false, "signed_in": false, "can_start": true}
```

`can_start` is true when the device answers `browser_start` (section 4.4). With `connected` false, no `reason` and `can_start` true, Firefox is closed and the plugin may start it.

The device sends it right after the `welcome` (and the `resume` on version 3), and again whenever the extension connects, goes away or its sign-in changes. `signed_in` and `user` are the device's view of the hub's answer to its `browser_auth` (section 13). The hub rewrites every `browser_status` from its own view: while the hub has not accepted the browser's credential, it sets `connected` and `signed_in` to false and `reason` to why, so a plugin that does not know `signed_in` offers no browser either. When the extension itself is not connected, `reason` is empty. The plugin keeps the last one per connection for `check_fn`, `browser_status` and `GET /devices` (the Device object gains `"capabilities"` and `"browser"`).

### 4.4 browser_start (plugin to device)

```json
{"type": "browser_start", "id": "5a1e...", "timeout": 90}
```

The device starts Firefox when the extension is not connected (section 5.1), waits until the extension is connected and the hub accepted its sign-in, and answers with one `browser_result` with the same `id`: `{"started": bool, "signed_in": true, "user"}`, or `refused` (`browser.start-firefox` is off), `failed` (Firefox could not start, for example with no graphical session; `error` says why), `timeout` (Firefox did not connect in time), `not_signed_in` (it connected but is not signed in) or `cancelled` (`kill`). A change of the sign-in does not cancel it; it waits for one. The hub forwards `browser_start` without a sign-in check: while Firefox is closed there is nothing to check, starting it gives no access to pages, and the exec leg could already start any program on the device.

## 5. Device side

- `hermes-device run` with `browser.enabled: true` listens on `<state dir>/browser.sock` (the state directory is `store.state-dir`, by default `hermes-device` in the user config directory). The directory is made mode 0700 and the socket 0600; on Linux it also checks the peer's uid with `SO_PEERCRED` and refuses other users. `browser.allow-hosts` lists loopback or private network host names the agent may open (section 6).
- `hermes-device browser install` (alias `install-host`) writes, for the current user and without root, the host manifest `hermes_device.json` (`"allowed_extensions": ["browser@hermes-device"]`, `"path"` = the running binary with symlinks resolved) and `hermes_device.socket` (the socket path for the configured state directory) into `~/.mozilla/native-messaging-hosts` (Linux) or `~/Library/Application Support/Mozilla/NativeMessagingHosts` (macOS). Run it again after moving the binary or changing `store.state-dir`. `hermes-device browser uninstall` removes both files.
- `browser.start-firefox` (default true; the sandbox image sets `HERMES_DEVICE_BROWSER_START_FIREFOX=false`) lets the agent start Firefox (section 5.1). `browser.firefox-command` (default `firefox`) is the command. `browser.confirm-hosts` adds sites that are handled like Salesforce (section 7.1); the plugin cannot change this list.
- `hermes-device browser extension --out <dir>` writes the extension's files, which the binary embeds, for loading as a temporary add-on.
- Firefox starts the binary as `<binary> <manifest path> <extension id>`. `main` recognizes that form, checks the extension id, finds the socket (`HERMES_DEVICE_BROWSER_SOCKET`, else the path in `hermes_device.socket` next to the manifest, else the default state directory), dials it and sends `host_hello` with the extension id and its pid. Then it copies native messaging frames both ways and enforces Firefox's 1 MiB limit towards the browser.
- The extension sends `hello` (its version and the browser's name and version). The broker answers with `config` (`allow_hosts`; `hub_url`, the daemon's `hub.url`, which the extension shows as a hint; and `device_id` and `device_name` once the exec leg has had a `welcome`, sent again when they change) and `auth_status`. The extension then sends `auth` (section 13). The broker accepts one extension connection; a newer one replaces the older, and the older gets `replaced` first. An extension that was replaced does not reconnect on its own; a click on its toolbar button makes it take over again. So two Firefox instances with the extension no longer take turns.
- Wire format on the socket: native messaging frames (32-bit little-endian length, then JSON). Requests are `{"type": "request", "id", "op", "args"}`, answers `{"type": "response", "id", "ok", "result" | "error", "code"}`.
- Sign-in messages: `{"type": "auth", "token": "hbt_..."}` (extension to daemon; an empty token means signed out; the broker ignores a token that does not start with `hbt_` or is longer than 256 bytes and forgets the token when the extension goes away), `{"type": "auth_status", "state", "user", "reason"}` (daemon to extension; `state` is `signed_out`, `checking`, `signed_in`, `refused` or `offline`), and `{"type": "sign_in"}` and `{"type": "sign_out"}` (daemon to extension: start the hub's sign-in window, or sign out, as the toolbar menu does; `hermes-device setup` sends `sign_in` through `POST /control/browser/sign-in` (docs/protocol.md section 5.5) once the extension connects signed out, and the end-to-end test sends both, since BiDi cannot open or script extension pages).

### 5.1 Starting Firefox

A scheduled job must work after the user closed Firefox. When the extension is not connected and the device offers it (`can_start`), any browser tool except `browser_status` sends `browser_start` first and then answers `started` ("Firefox was not running on device X, so it was started and is signed in now. Make the same browser call again."). The call is not run in the same step, so its approvals are asked against the running browser. `browser_status` only reports that Firefox is not running. The tools stay listed while Firefox is closed and the device can start it.

The daemon usually runs as a systemd user service, which has no display variables. To start Firefox in the user's graphical session it:

1. Takes its own environment. When that has neither `WAYLAND_DISPLAY` nor `DISPLAY`, it adds the session variables (`WAYLAND_DISPLAY`, `DISPLAY`, `XAUTHORITY`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`, `XDG_SESSION_TYPE`, `XDG_CURRENT_DESKTOP` and a few more) from `systemctl --user show-environment`. Desktops import them into the user manager at login (measured on this project's KDE Plasma desktop: the user manager has `WAYLAND_DISPLAY`, `DISPLAY`, `XAUTHORITY` and `DBUS_SESSION_BUS_ADDRESS`, the service's own environment does not). `XDG_RUNTIME_DIR` defaults to `/run/user/<uid>` and the bus to `$XDG_RUNTIME_DIR/bus` when they exist.
2. Checks that the display's socket exists (`$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY`, or `/tmp/.X11-unix/X<n>` for `:<n>`). If there is no display, or its socket is gone (the user logged out), it fails with "no graphical session" and says to sign in to the desktop.
3. Runs `systemd-run --user --scope --collect --unit=app-hermes-firefox-<random>.scope -- <firefox>` (plain `<firefox>` without systemd), in a new session, with standard streams on `/dev/null`. The scope keeps Firefox out of the daemon's cgroup, so restarting `hermes-device run` does not stop Firefox. Without `-P` or `--profile` Firefox opens the user's default profile, where the extension, its sign-in and the user's logins are. When Firefox already runs, the command opens a window there and exits. On macOS it runs `open -a Firefox`.
4. Starts Firefox at most once a minute (a second `browser_start` within a minute is refused), then waits until the extension connects and the hub accepts its stored credential (the extension keeps it in `storage.local`, so no new sign-in is needed), at most 90 seconds.

Not tested end to end: the test suite does not start a Firefox on the user's display. The environment, the command line and the device's `browser_start` handling have unit tests.

## 6. Extension

- Manifest V2 with a persistent background page. Firefox keeps Manifest V2 supported, and a persistent page keeps the native port open. Manifest V3 in Firefox makes host permissions optional at install, which `tabs.captureTab` needs (inference; check before moving).
- Permissions: `nativeMessaging`, `tabs`, `tabGroups`, `scripting`, `webRequest`, `webRequestBlocking`, `dns`, `identity` (the sign-in window), `storage` (the credential and the hub address), `<all_urls>`. Firefox shows these at install, and asks the user again when an update adds one (section 13.7).
- The toolbar button opens a menu (`popup.html`): the state, the hub address with a "Change" link to the options page (`options.html`), "Sign in to Hermes" or "Sign out", and "Use this Firefox" after another Firefox took over. Its title is "Sign in to Hermes" while the extension holds no credential.
- The extension refuses every request with `refused` and "Firefox is not signed in to Hermes" unless the daemon reported `signed_in` for its current credential. The daemon and the hub refuse such requests as well (section 13.4).
- No content scripts are registered, nothing listens for page messages, and there is no `externally_connectable` or web accessible resource. Page code runs only through `scripting.executeScript` during an operation, in the isolated world, in one frame or in all frames of the tab. Agent scripts (`eval`) run through `tabs.executeScript` with `code` in the same isolated world (section 7.2). No new permission was needed for frames, keys or scripts, so updating the extension asks the user nothing.
- It acts only on tabs that are in its tab group now (tabs it opened, and tabs the user moved in; a tab dragged out is no longer the agent's). Anything else gets `refused`. Firefox puts tabs opened from a grouped tab (a link with a target, `window.open`, the user's middle click) into the same group (source, `Tabbrowser.sys.mjs`), so those become agent tabs too. Planned: count only tabs the agent opened or the user granted with a context menu item, since another extension can also move tabs into the group.
- It reads, snapshots and screenshots only tabs that show an `http` or `https` page, so going back or redirecting to `file:` or `about:` does not expose those pages. Measured: `read_text` on a user's tab is refused, and so is `navigate` to `file:`.
- It blocks every request from an agent tab (navigations, redirects, links, forms, subresources, WebSockets) to loopback, RFC 1918, link-local, CGNAT (100.64/10, which Tailscale uses), unique-local IPv6 and `.local`, `.localhost`, `.internal` and `.home.arpa` names, unless the host is in `browser.allow-hosts`. Host names are resolved with `dns.resolve`, so public names that point at private addresses are also caught. Without this, an injected instruction could use the user's browser against local services, such as a router or Hermes's own loopback-trusted web server. This needs the `webRequest`, `webRequestBlocking` and `dns` permissions. A name the resolver cannot resolve is blocked. IPv4-mapped and NAT64 IPv6 literals are checked as the IPv4 address they hold (measured: `http://[::ffff:127.0.0.1]/` is refused). DNS rebinding between the check and the connection is still possible (inference). Requests a page's service worker makes have no tab (`tabId` -1) and are not checked (suspected; not tested). `tab_open` puts the tab in the group while it is still blank, so the first request is checked.
- It navigates only to `http` and `https` URLs. `about:`, `file:` and `moz-extension:` are out of reach, and Firefox itself keeps extensions off the domains in `extensions.webextensions.restrictedDomains` (addons.mozilla.org, accounts.firefox.com and others; source, `greprefs.js`).
- It checks a `guard` right before each click and type, in the page (section 7).

### 6.1 Loading and signing

Release and Beta Firefox install only add-ons that Mozilla signed; `xpinstall.signatures.required` has no effect there (Mozilla's documentation; not tested here). Each release tag publishes the extension signed by addons.mozilla.org (AMO) as a self-distributed ("unlisted") add-on. It is not listed on AMO.

`hermes-device setup` opens Firefox at the `.xpi` of the release that matches the binary (the latest release for a development build) when no Firefox profile has the extension, and GitHub serves the file as `application/x-xpinstall`, so Firefox offers to install it (measured: the content type; the Firefox prompt is an inference, not tested). To install it by hand:

1. Open the latest release at https://github.com/USA-RedDragon/hermes-device/releases/latest and download `hermes-device-firefox-<version>.xpi`.
2. In Firefox, open `about:addons`, choose the gear menu, then "Install Add-on From File...", and pick the file. You can also drag the file onto a Firefox window.

Updates: the manifest's `browser_specific_settings.gecko.update_url` is `https://github.com/USA-RedDragon/hermes-device/releases/latest/download/updates.json`. Firefox checks it about once a day (the default `extensions.update.interval`) and installs a newer version from the `update_link` in that file, after it checks the file's `update_hash` (SHA-256). GitHub answers that URL with a redirect to the latest release's asset. Firefox follows redirects for update manifests: `AddonUpdateChecker.sys.mjs` loads the manifest with an XHR and its `CertUtils.BadCertHandler` lets every redirect through, after a certificate check when `extensions.update.requireBuiltInCerts` is on (source, mozilla-firefox `toolkit/mozapps/extensions/internal/AddonUpdateChecker.sys.mjs` and `toolkit/modules/CertUtils.sys.mjs`; not yet tested with a real update). To check now, choose "Check for Updates" in the gear menu of `about:addons`.

What the Release workflow does (`.github/workflows/release.yaml`, job `firefox-extension`, after GoReleaser published the release):

1. It builds the extension with `hermes-device browser extension --out DIR --ext-version X --hub-url-default URL`, where X is the tag without its leading `v` and URL is the repository variable `HUB_DEFAULT_URL` (left out when the variable is not set). Only the `version` line of `manifest.json` and the `DEFAULT_HUB_URL` line of `defaults.js` differ from the files in `extension/firefox`.
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
3. Optional: set the default hub address of released extensions with `gh variable set HUB_DEFAULT_URL --body https://hermes.example.com`. It is a repository variable, not a secret: anyone can read it in the released extension. It must be `https`, without a user name, password, query or fragment; the build fails otherwise. Without it, the extension asks each user for the address. Users can change it in the extension's options either way.
4. Tag a release. The first upload creates the add-on on AMO under the id `browser@hermes-device`. That id stays fixed: the host manifest's `allowed_extensions` names it.

AMO runs automated validation. Unlisted add-ons are signed without a review by hand, but Mozilla may review them later (Mozilla's Extension Workshop documentation). AMO requires a new `version` for every upload, and `data_collection_permissions` in the manifest (present: `"required": ["none"]`). The extension has no minified or generated code, so no source code upload is needed.

For development, load the extension as a temporary add-on instead:

1. `hermes-device browser extension --out ~/hermes-browser-extension`. A release binary writes its own version into the manifest; `--ext-version` sets another, and `--hub-url-default` sets the default hub address (`http` is accepted only for `localhost`, `127.0.0.1` and `[::1]`).
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
| Any click or type on a page with a password, payment or one-time code field in any frame | Ask every time |
| Key press (`browser_press`): Enter or Space on a submit control or Enter in a form field | As a submit |
| Hover, scroll, wait | Allowed |
| Run a read-only script (`browser_eval`) on a site | Ask once per site per session (`rule_key` `browser:script:<site>`, the site of the frame it runs in) |
| Run a script that changes data (`browser_eval` with `allow_writes`) | Ask once per site per session (`rule_key` `browser:script-write:<site>`); covers read-only scripts there too |
| Any click, Enter or Space key press, or `browser_type` with `submit` on Salesforce (section 7.1) | Ask every time |
| A script that changes data on Salesforce | Refused; no approval can allow it |
| Any script on a page with a password, payment or one-time code field | Ask every time |
| Downloads, file uploads, permission prompts, browser dialogs | Not offered |

A site is the host name in lower case without a leading `www.`. It is not the registrable domain: `accounts.example.com` and `example.com` are separate sites. That asks more often, but an approval never covers a host the user did not see.

How the plugin asks:

1. Its `pre_tool_call` hook runs for `browser_navigate`, `browser_tabs`, `browser_click`, `browser_type`, `browser_press` and `browser_eval`. It first drops any grant left from an earlier call with the same arguments (one the user denied). For a click or type it sends `inspect` for the target ref; for a navigate or a new tab it sends `inspect` with the URL (and the tab), so the site it asks about is the host the browser itself parses from the URL, not the plugin's reading of it. When that `inspect` fails, it asks about the raw URL and approves no host, so the extension refuses to open another site. `inspect` returns the page's host, the kinds of secret fields on the page (`sensitive`: `password` for `type=password` or a password `autocomplete`; `payment` for `cc-*` `autocomplete`, card, CVC or expiry field names, or an iframe of a known payment provider; `one_time_code` for `autocomplete="one-time-code"` or OTP, 2FA or verification code field names) and what the ref is (`submit` with the host of the button's `formaction` or its form's action, for a submit button around the ref, a label's control, or a button outside its form through `form=`; else `link` with its host; `field`; or `other`).
2. For a submit or a new site it returns `{"action": "approve", "message": ..., "rule_key": ...}`. Hermes sends that to its approval gate, which asks wherever the user talks to the agent (CLI prompt, web UI, Telegram buttons, the ACP editor) and offers once, session, always and deny. "Session" remembers the rule key for the session and "always" for good, in `command_allowlist` (source, `hermes_cli/plugins.py:1987-2016`, `tools/approval.py:996-1068`, `412-425`). The gate approves at once in yolo mode or with `approvals.mode: off`, and denies when no human can answer (cron, `-q`, webhooks) unless `approvals.cron_mode`, `single_query_mode` or `unattended_mode` say approve.
3. On a page with secret fields the hook asks nothing. The handler calls `request_elicitation_consent` (source, `tools/approval_prompt.py:296-376`) without a `remember_key`, so the user can only allow this one call or deny it. Yolo and `approvals.mode: off` do not skip it, and it denies when no human can answer.
4. The hook hands the handler what was approved, and the handler sends it as the `guard` of the `click`, `type`, `navigate` or `tab_open`. The extension classifies the element and the page again in the page before it acts, and again after the synthetic pointer and mouse events and right before the activation (page handlers of those events can rewrite links and forms). It refuses (`refused`) a submit to a host not in `submit_hosts`, a link or navigation to another site not in `visit_hosts`, and any action on a page with secret fields without `sensitive`. So a page that changed between the hook and the action cannot slip a submit, a new site or a secret field past the user, and a handler call without the hook (or after a failed `inspect`) can do only what needs no approval.

The hook's `inspect` runs inside Hermes's hook timeout (`plugins.hook_callback_timeout`, default 30 seconds; the plugin gives `inspect` 15). When it fails, a navigate still asks, and a click or type goes ahead with an empty guard.

Known gaps:

- Navigation that a page starts by itself after a click (a script that sets `location`, a redirect, a `target` link) is not classified. The extension's private network block still applies to requests from agent tabs.
- Script-driven submits are not seen: a `type=button` or `role=button` control that posts with `fetch`, as most single-page apps do, counts as a plain click and needs no approval. Only a page with secret fields then asks.
- Sensitive field detection is heuristic. It covers every frame and shadow tree the extension can reach.
- Planned: the user's block list (banks, the password manager) in the device config, so a compromised pod cannot remove it.

### 7.1 Salesforce (protected sites)

On Salesforce every click needs the user's OK each time. A site is protected when its host is, or ends in `.` and, one of `force.com`, `salesforce.com`, `visualforce.com`, `cloudforce.com`, `salesforce-setup.com`, `salesforce-sites.com`, `documentforce.com` and `force-user-content.com` (Salesforce's own domains for Lightning, My Domain, Visualforce, Setup, Sites and files), or one of the device's `browser.confirm-hosts` (with its subdomains). The check uses the tab's host and the host of the frame the action runs in. Experience Cloud sites on `*.my.site.com` and customer domains are not covered by default; add them to `browser.confirm-hosts`.

On a protected site:

- `browser_click`, `browser_press` with Enter or Space, and `browser_type` with `submit` ask every time, with the element's text in the question ("The agent wants to click the element "Start Session Timer" on ... (Salesforce)"). The handler asks through `request_elicitation_consent` without a remember key, so the user can only allow this one call or deny it, as on pages with secret fields. No "session" or "always" answer and no `command_allowlist` entry covers it. Yolo and `approvals.mode: off` do not skip it.
- With no user present (section 7.3) such a call fails at once with a message that it needs the user each time.
- Typing without submit, other keys (Tab, Escape, arrows), hover, scroll, snapshots, text and screenshots need nothing more than on other sites.
- `browser_eval` with `allow_writes` is refused by the plugin and by the extension. Read-only scripts use the normal approval (`browser:script:<site>`, "always" allowed), so a scheduled job can read. A read-only script cannot send anything but GET and HEAD requests to the site (section 7.2).

The extension enforces the same list (the plugin's and the device config's): it refuses a click, an activating key press or a submit on a protected site without `guard.confirmed`, and every script with `allow_writes`.

### 7.2 Scripts (browser_eval)

`browser_eval` runs agent JavaScript in an agent tab, as the body of an async function, and returns the value it returns, as JSON. Only the agent's tool call starts it: the op comes over native messaging from the daemon, and pages have no way to reach the extension (section 8).

Where it runs: in the extension's isolated content script world of the frame, through `tabs.executeScript` with `code`, which the page's Content Security Policy does not block. It sees the page's DOM, not the page's JavaScript objects. `fetch` and `XMLHttpRequest` in the script are the page's own (`content.fetch`, `content.XMLHttpRequest`), so requests go out as the page's, with its cookies, origin and same-origin rules, as a script in the developer console would (measured: a GET of a local JSON API returns its value, and an approved PUT reaches the server with its headers and body). Values put on `window` stay for later scripts in the same page (measured). Before agent code runs, the extension API (`browser`, `chrome`) is removed from that world, so a script cannot read the hub credential in `storage.local` or message the extension (measured: `typeof browser` is "undefined", also through `globalThis`, and `browser.storage` cannot be reached); the extension's helpers keep only `runtime.getFrameId`. Same-origin frames' documents are reachable from the script (measured: `iframe.contentDocument`), cross-origin ones are not.

Approval: the site of the frame the script runs in (section 7 table). The question shows the first 400 characters of the script. A script in a cross-origin frame needs that frame's site (measured: refused with only the page's approval).

Limits the extension sets before the script runs, kept until the agent loads a page in the tab again with `browser_navigate`. That navigation first loads `about:blank` (no request), so the page that ran the script is gone before the limits end, and the new page loads without them:

- Requests from the tab may go only to the sites of the page and of the frame the script ran in. Everything else is cancelled: the page's fetch, images, beacons, script-driven navigations, and tabs the page opens (they inherit the limits). Requests from content scripts with the extension's own `fetch` and `XMLHttpRequest` carry no tab and no origin (measured: `tabId` -1, no `originUrl`); while any tab holds limits, all such requests are cancelled (measured: none of seven ways to reach another site from a script got through). This also cancels such requests of other extensions while a tab that ran a script is open.
- A read-only script (no `allow_writes`): the tab may send only GET and HEAD requests and may not open WebSockets (measured: a PUT fails with NetworkError and the server gets nothing). This covers what the script does through the page too, such as clicking a button whose handler posts. The page's own writes are cancelled as well until the next navigation, so a Salesforce page that ran a read-only script stops saving anything.
- Writes are tracked per site: a script with `allow_writes` lifts the read-only limit only for the site the user approved writes on. Every other site in the tab's list (the top page's, another frame's) stays read-only (measured: after an approved write on the page, a POST to a frame's site is refused).
- A script in a frame of a tab that already ran a script is refused until the next navigation, and the top frame's address is taken from the browser, not from the page's world, because an earlier script could have changed what that world reports.
- A tab opened from another tab (it has an opener) may not run scripts, since a script could navigate its opener, which has no limits. Agent tabs that `browser_navigate` opens have no opener.
- Scripts with `allow_writes` are refused on pages with password, payment or one-time code fields; read-only scripts there ask every time.
- Clicks, typing and key presses are refused in the tab, because the script could have changed the extension's helpers in that world. Scroll, hover, snapshots, text and screenshots still work.
- `browser_back` is refused in a tab that ever ran a script, so a page that ran agent code is not restored from the history cache without its limits.

Writes and "always": `browser:script-write:<site>` is a separate approval, so the user grants it knowingly. "Always" for `browser:script:<site>` covers only read-only scripts; "always" for `browser:script-write:<site>` covers writes and reads there. Both are entries `plugin_rule:browser:script:<site>` and `plugin_rule:browser:script-write:<site>` in Hermes's `command_allowlist`; remove the entry (Hermes picks up the change at once) to revoke it.

Known gaps:

- The page's own code can see and change what the script does on the page (it shares the DOM, and the page's `fetch` can be replaced by the page). Approve scripts only on sites you trust.
- The script can read anything the page can, including cookies that are not HttpOnly, and returns it to the agent.
- A GET can change data on a badly built site; read-only means only GET and HEAD.
- Requests a page's service worker makes have no tab and an origin, so the limits do not cover them (suspected; not tested). A WebSocket the page opened before the script stays open.
- A page that a script ran in can still be restored from the history cache by a page that calls `history.back()` itself after the agent navigated.
- Pages a script opens with `noopener` may get no opener id and then no limits; Firefox's popup blocker usually stops `window.open` from a script (inference; not tested).
- The block on requests from the extension's own `fetch` and `XMLHttpRequest` rests on how Firefox 157 reports them (no tab, no origin). If a later Firefox sets an origin there, scripts could reach other sites through them; the end-to-end test checks it on every run.
- Yolo mode and `approvals.mode: off` approve every `browser:visit`, `browser:submit`, `browser:script` and `browser:script-write` rule at once in an attended session (Hermes's gate); only the per-call confirmations (secret fields, Salesforce) still ask.
- Firefox add-on policies restrict running code that is not part of the add-on. These scripts are the user's agent's tool calls, not code the add-on loads, but AMO's reviewers may judge otherwise (inference; the add-on is unlisted and signed without a manual review today).

### 7.3 Runs with no user present

A scheduled job (cron), a one-shot query (`-q`) or a webhook session has no user to answer. The plugin checks this with Hermes's own test (`tools.approval_context._no_user_can_answer`) before it asks:

- An approval kind the user granted before ("always", or "session" in the same session) counts as granted, through Hermes's `tools.approval.is_approved` for `plugin_rule:<rule_key>`. The call then runs with no prompt.
- Anything else is blocked at once, in `pre_tool_call`, with a message that names the rule (`plugin_rule:browser:visit:<site>` and so on) and says the user must grant it once with "always" or add it to `command_allowlist`. This holds whatever `approvals.cron_mode` says: browser approvals are not granted by default in an unattended run.
- Actions that ask every time (pages with secret fields, clicks on Salesforce) are blocked at once with a message that they need the user each time.

To prepare a scheduled job: run it once in a chat and answer "always" for each site and kind it needs, or add the `plugin_rule:browser:...` entries to `command_allowlist` in the Hermes config. A config managed in git keeps them across rebuilds. Create the job in a chat whose browser device (section 3.2) is the computer the job should use, or set it later with `browser_use_device` and `job_id`. The job's commands run in the sandbox; only its browser calls go to that computer, which starts Firefox when it is closed (section 5.1).

The user can stop everything at any time: close the Hermes tab group, sign Firefox out in the toolbar menu or remove it under Settings, Computers on the hub, disable the extension, or stop `hermes-device run`.

## 8. Security analysis

Who can drive the browser:

- Anyone, only while Firefox is signed in to the hub as the user who owns the device, and the hub checks that on every operation (section 13). A browser signed in as another hub user, or not signed in, is refused by the hub, the daemon and the extension.
- Web pages: no. Native messaging is reachable only from the extension's privileged pages. The extension has no page-facing message channel. Content script refs are invisible to the page.
- Other users on the machine: no. The socket is 0600 in a 0700 directory, and the broker checks the peer uid on Linux. There is no TCP port.
- Other processes of the same user: yes, by connecting to the socket and pretending to be a host, or by editing the manifest. They can already read the Firefox profile (with the browser credential in the extension's storage) and the device token, so this is not a new boundary. The sign-in does not defend against them; it makes the link between the browser and a hub user explicit and revocable instead of following from the OS account. The broker accepts only `host_hello` for the expected extension id, which stops accidents, not attacks.
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
- `internal/browser/launch`: starting Firefox in the user's graphical session (section 5.1).
- `extension/firefox`: the extension (`manifest.json`, `background.js`), embedded in the binary. The `firefox-extension` job in `.github/workflows/release.yaml` signs and publishes it.
- `internal/device`: the `browser` capability, `browser`, `browser_result` and `browser_status` on the exec leg; `cmd/run.go` starts the broker when `browser.enabled` is on; `cmd/browser.go` is `hermes-device browser`.
- `plugin/device/browser.py`: the `device-browser` toolset and its `pre_tool_call` hook. `plugin/device/core.py` and `dashboard/plugin_api.py`: capability negotiation and browser calls on the exec leg.
- Unit tests: `internal/device/browser_test.go` (also `browser_start`), `internal/browser/*/..._test.go` (also the launch environment and command line), and `plugin/device/tests/test_browser.py` (the tools, the approvals, Salesforce, unattended runs, starting Firefox, hiding `browser_vault` and the negotiation with a fake device and browser, through a fresh Hermes `ToolRegistry`).
- `internal/browser/bidi` and `internal/browser/firefoxtest`: an end-to-end test that signs the extension in through a fake hub (`signin_test.go`: the consent page, the redirect that `browser.identity` catches, the PKCE code exchange, and sign-out with revocation), builds the binary, writes the host manifest into a fresh `HOME`, starts headless Firefox on a fresh profile with `--no-remote`, loads the extension as a temporary add-on over BiDi (`webExtension.install`; temporary add-ons skip signing), and drives it through the broker. When `hermes` is on `PATH` it then runs the real device client against the broker, with `plugin/device/tests/test_browser_e2e.py` on the other end of the exec leg in the Hermes runtime, and drives Firefox through the plugin's tools and approval hook.

Run the end-to-end test (it needs the network for example.com):

```sh
HERMES_BROWSER_E2E=1 go test -count=1 -v -run TestFirefoxEndToEnd ./internal/browser/firefoxtest/
```

It first checks that the extension refuses `tabs_list` before it is signed in, has the daemon start the sign-in (BiDi cannot open extension pages, so the toolbar menu is not clicked), approves the fake hub's consent page in the window `browser.identity` opens, and checks that the credential reaches the daemon. Measured: Firefox loads the authorize URL twice, once as the `checkRedirected` probe and once in the window. At the end it signs out and checks that the fake hub got the revocation and the daemon dropped the credential. It opens example.com in a new tab in the Hermes group, checks the title, the text, the snapshot and a PNG screenshot, checks that a link to another site is refused without approval and followed with it, fills a form on a local test server and checks that its submit is refused without approval and works with it, checks that opening a new site without approval is refused, checks the private network block (also for IPv4-mapped IPv6 literals) and the refusals, checks that a submit button inside a link and one outside its form are submits, and checks that every action on a page with password and one-time code fields needs confirmation. `frames_test.go` then checks, on local servers on 127.0.0.1, 127.0.0.2 and 127.0.0.3: a page with a same-origin and a cross-origin iframe and an open shadow root (snapshot, clicks in both frames and in the shadow tree, text of all of them, paged text), scrolling by page, to the bottom and to a text, key presses (a character, Tab, Escape, an unknown key), hover, waiting for late text and a selector, a full-page screenshot, and scripts: no approval, a GET fetch, state kept on `window`, a script without `return`, a script error, a PUT refused in a read-only script and without the write approval, an approved PUT that reaches the server, seven ways to reach another site from a script (none arrive), clicks and back refused after a script, a script in the cross-origin frame, and a protected site from `browser.confirm-hosts` (clicks and Enter refused without confirmation, Tab allowed, writes refused, reads allowed). The plugin part does the same through `browser_navigate`, `browser_snapshot`, `browser_vision`, `browser_click`, `browser_back`, `browser_type` and `browser_tabs`, and frames, keys, scrolling and scripts (a read, a write the hook did not approve, an approved write, and a blocked script with no user present) through `browser_snapshot`, `browser_press`, `browser_scroll` and `browser_eval`. BiDi is used only to install the unsigned extension in the test; the browser control itself goes through the extension.

## 11. Enable it

On the server: the device plugin of this release, with the toolset on (the default; `plugins.entries.device.settings.browser: false` turns it off). To hide Hermes's `browser_vault`, also set `plugins.entries.device.granted_capabilities: [tools.override]` (section 3.1). In a profile that also enables the camofox plugin, only one of the two gets the tool names.

On the computer, `hermes-device setup` does steps 1 to 5 (README, section Setup). By hand:

1. Set `browser.enabled: true` in the hermes-device config (and `browser.allow-hosts` for any LAN or loopback host the agent may open).
2. Run `hermes-device browser install` as the user who runs Firefox.
3. Restart `hermes-device run`.
4. Install the signed extension from a release (section 6.1).
5. Sign it in: the toolbar button's title is "Sign in to Hermes". Open its menu, set the hub address if asked, choose "Sign in to Hermes", sign in on the hub as the account that paired this computer and choose Allow. The title then shows "Hermes: connected as <user>".
6. Bind a session to this device with `/device <device> <dir>`. `browser_status` in that session shows whether the agent can use the browser.

## 12. Open risks

- The measurements come from a Firefox started with `--remote-debugging-port` (needed to load the unsigned extension in the test), which applies the Remote Agent's recommended preferences, among them the popup blocker off. Behavior in the user's own Firefox can differ, so the first manual test in a normal profile is still due.
- Synthetic input: `isTrusted` is false for every click and key. Sites that check it, canvas editors and some anti-bot flows will not work. Firefox gives WebExtensions no debugger API. If this blocks real use, the BiDi dynamic start mode (2.2) is the path, once Firefox ships it.
- Screenshots of background tabs: measured headless only. In a headed Firefox, `tabs.captureTab` of a tab that was never shown may return a blank or stale image (inference). The fix is to activate the tab in its own window briefly, or to open the Hermes group in a separate window.
- Signing: every release signs the extension on AMO (section 6.1). AMO validation rules can change and fail the signing step, and Mozilla may still review the unlisted add-on by hand.
- Snap and Flatpak Firefox (section 9).
- Scripts: the gaps in section 7.2, and the add-on policy question there.
- Starting Firefox is not tested end to end (section 5.1).
- Cancelled operations are not stopped in the browser (section 4.1).
- The approval rules have the gaps listed in section 7.
- A Firefox container for agent tabs would limit the agent to sites the user chose to sign in to there, which shrinks what a compromised pod or an injected instruction can reach. It costs the user one sign-in per site. Worth trying before release.
- The extension keeps its group and tab ids in memory: after a Firefox restart it creates a new Hermes group, and old Hermes tabs are no longer the agent's.
- The browser credential sits in the extension's `storage.local` in the Firefox profile, readable by the user's own processes (section 13.6).

## 13. Hub sign-in

Firefox must be signed in to hermes-hub as the hub user who owns the device before the agent can use it. Before this, the only link between the extension and a hub user was that Firefox could start the native host and reach the daemon's socket as the same OS user.

### 13.1 Flow

The extension uses `browser.identity.launchWebAuthFlow` with the hub as the authorization server and a PKCE (S256) code exchange.

1. The user chooses "Sign in to Hermes" in the toolbar menu. The extension needs the daemon to be connected, because the daemon tells it the device id and name (`config`).
2. The extension makes a random PKCE verifier and `state`, and keeps a random browser instance id in `storage.local`. It opens `<hub>/browser/authorize` with `response_type=code`, `client_id=browser@hermes-device`, `redirect_uri` (Firefox's `identity.getRedirectURL()`: `https://<SHA-1 of the extension id>.extensions.allizom.org/`, which `protocol.BrowserRedirectURL` computes), `state`, `code_challenge`, `code_challenge_method=S256`, `device_id`, `instance` and `browser` (such as `Firefox 157.0.1`).
3. Firefox first loads that URL without a window and follows redirects with the user's cookies (source, Firefox 157 `parent/ext-identity.js`, `checkRedirected`). The hub always answers with a page, never with a redirect to the extension, so Firefox then opens a window on it. GET has no side effects.
4. The hub sends a user without a hub session through its normal Authentik login and back. It shows a consent page that names the hub user, the computer (name and host name) and the browser, with Allow and Do not allow. It refuses, without redirecting, a wrong `client_id` or `redirect_uri`, a device it does not know, and a device of another hub user ("This computer is connected to Hermes as another account").
5. Allow posts the form with the session's CSRF token and a same-origin check. The hub stores a one-time code (its hash, bound to the user, device, instance, browser, challenge and redirect URI, valid 2 minutes) and redirects with 303 to `redirect_uri?code=...&state=...`. Firefox catches the request to the redirect URL in the window, cancels it and closes the window; the request never leaves the browser.
6. The extension checks `state` and posts `{grant_type, code, code_verifier, redirect_uri, device_id, instance}` to `<hub>/api/browser/token`. The hub checks the verifier against the challenge and that the device still belongs to the user, and answers `{token, session_id, user, device_id, device_name}`.
7. The extension stores the token, the hub address, the user and the device in `storage.local` and sends `auth` to the daemon.

Why not a device-code flow: it needs no redirect, but the user copies a code, the extension polls, and a code shown on one screen can be approved by someone tricked into it. `launchWebAuthFlow` is built into Firefox for MV2 extensions, keeps the code inside the browser, and binds it to this extension's redirect URL and the PKCE verifier. Its cost is the `identity` permission.

### 13.2 Credential

The hub issues one long-lived, revocable token per (hub user, device, browser instance): `hbt_` and 43 base64url characters (32 random bytes). The hub stores only its SHA-256. A new sign-in of the same instance on the same device replaces the earlier token.

The token ends when the user removes the browser under Settings, Computers, when the extension signs out (it calls `POST /api/browser/revoke` with the token as a bearer), when the device is removed, or after 90 days without use. A short-lived access token with a refresh token was not chosen: the hub checks the token in its store on every browser operation anyway (section 13.4), so revocation is immediate without short lifetimes, and the only party that presents the token is the device's daemon over its own authenticated exec leg. A refresh token would add rotation state to the extension and the daemon without closing a gap: whoever can read the refresh token from the profile can also read the device token.

### 13.3 Exec leg messages

The device presents the credential on the exec leg; the hub, not the plugin, answers. These messages are part of the `browser` capability.

```json
{"type": "browser_auth", "token": "hbt_..."}
{"type": "browser_auth_result", "ok": true, "user": "alice", "session_id": "..."}
{"type": "browser_auth_result", "ok": false, "code": "wrong_user", "error": "Firefox is signed in to Hermes with another account than the one this computer belongs to. ..."}
```

| `code` | Meaning |
|---|---|
| `signed_out` | The token was empty: the extension is signed out or not connected |
| `invalid` | The hub does not know the token, or it was revoked or expired |
| `wrong_user` | The token belongs to another hub user than the device's owner |
| `other_device` | The token was issued for another device (for example before `hermes-device login` paired this computer again) |
| `unavailable` | The hub could not check the token; the device sends it again after <!-- docgen:const pkg=internal/device name=AuthRetry print=config -->`30s`<!-- /docgen:const --> |

- The device sends `browser_auth` after the `welcome` (and the `resume`), and whenever the extension sends another token. The hub answers each with one `browser_auth_result` and never forwards `browser_auth` to the plugin. Later, when the hub drops the sign-in (revoked, device removed, expired), it sends an unsolicited `browser_auth_result` with `ok: false`.
- The hub drops a `browser_auth_result` that comes from the plugin.
- A device keeps the hub's answer for its current token. While answers are due, it counts as not signed in. A hub that does not answer (a release from before this change) leaves the device not signed in, and the status says the hub may need an update.

### 13.4 Enforcement

At the hub, on each exec leg:

- The hub reads the type of every message both ways with `protocol.MessageType`, and drops a message that is not a single JSON object, that has a member name twice, that has a member other than `type` whose name equals `type` without regard to case, or whose `type` is not a string. The device reads its messages with the same function. Go's struct decoding matches names without regard to case and keeps the first value when a later one is `null`, while Python keeps the last value of a name; without these rules, `{"type": "browser", "type": null}` would be a `browser` request to the device and a message without a type to a lenient hub.
- For a `browser` request from the plugin, the hub looks the leg's browser session up in its store again (not revoked, not expired, same device, same user as the device's owner). If it does not hold, the hub answers the plugin itself with `browser_result` code `not_signed_in` and does not forward the request.
- The hub rewrites `browser_status` (section 4.3).

On the device: `hermes-device run` refuses every `browser` request with `not_signed_in` unless the hub accepted the extension's current token on this leg, and cancels the requests in flight (answering `cancelled`) when the hub drops the sign-in or the extension sends another token. The extension may still finish an operation it already started (section 4.1). The extension refuses every request unless the daemon reported `signed_in`, and agent tabs may not open the hub it signs in to or the `extensions.allizom.org` redirect host, so the agent cannot approve a sign-in for itself in its own tabs.

A `browser` request that passed the hub's store check just before a revocation still reaches the device; the device cancels it when the hub's drop arrives.

A browser of another hub user cannot drive the device: the consent page refuses to issue a token for a device the user does not own, and the hub refuses a token whose user is not the device's owner (`wrong_user`) or whose device is another (`other_device`).

### 13.5 Revocation

Settings, Computers on the hub lists each computer's signed-in browsers (browser, when signed in, last used) with Remove. Remove revokes the token in the store. Every replica checks the store before each browser operation, so the next operation is refused on every replica at once. The replica that served Remove also drops the sign-in from the device's leg at once and tells the device and the plugin; another replica does so within `device.recheck_interval` (default 10 seconds), and at the next operation. "Sign out" in the extension revokes the token at the hub and sends an empty token to the daemon, which the hub answers with `signed_out` at once. When the hub cannot be reached, the extension still forgets the token and tells the user to remove the browser on the hub.

### 13.6 Hub address

The extension's own setting decides where it signs in. Released extensions carry a default from the repository variable `HUB_DEFAULT_URL` (section 6.1); otherwise the user enters it. The options page shows the daemon's `hub.url` as a hint and offers to use it. The address must be `https` (`http` only for `localhost`, `127.0.0.1` and `[::1]`, for tests), without a user name, password, query or fragment; trailing slashes are removed. Changing it signs the extension out and revokes the old token at the old hub when that hub can be reached. An extension signed in at another hub than the daemon's gets `invalid` from the daemon's hub: the hub that issued it is not the one that checks it.

The token sits in the extension's `storage.local`, inside the Firefox profile. A process of the same OS user can read it, as it can read the device token; the token is useful only together with that device's exec leg, because the hub accepts it only from the device it was issued for.

The sign-in is a consent and revocation step, not a boundary against code that already runs as the user. A command the agent runs on the device (the exec leg) runs as the same OS user as Firefox and the daemon: it can read the token from the profile, connect to the broker socket, or drive Firefox by other means. The sign-in decides which hub user's agent may use the browser through the extension, and lets the user end that at any time.

### 13.7 Upgrade and rollout

- Exec leg commands keep working in every order: an older hub forwards `browser_auth` to the plugin, which ignores unknown types, and an older device sends none.
- Browser control stops until all three parts are new and the user signs in: a new hub refuses browser operations of a device that presents no credential, and a new daemon refuses them until the hub accepted one. An older plugin reads only `connected`, which the hub sets to false while the browser is not signed in.
- Order: the hub first (it adds migration 7 and only refuses; it can roll back without data loss), then `hermes-device` and its plugin, then the extension. The extension updates itself from the next signed release; because it asks for the new `identity` and `storage` permissions, Firefox asks the user to accept them before it installs the update (Mozilla's documentation; not tested here). Then each user signs Firefox in once.
- A paired device needs nothing new. If the computer is paired again (`hermes-device login`), its device id changes and the old sign-in reports `other_device`: sign in again.
