feat(gateway): scope session.list to an audience judged by plugins

## What

`session.list` on the canonical gateway takes a new optional param, `audience` (a non-empty string). When it is set, the listing keeps only the sessions that `filter_session_list` plugin hooks keep for that audience. Without it, nothing changes: no hook runs and the listing is the same as today.

A new plugin hook, `filter_session_list(audience, session_ids, **kwargs)`, returns the ids that audience may see, or `None` for an audience the plugin does not judge. If several callbacks judge an audience, a row stays only if all of them keep it.

`runtime.describe` advertises `session-list-audience-v1`.

## Why

Some deployments put a front end between many callers and one gateway, and that front end signs in to the gateway with a single login. For example, a relay lets each of a user's remote computers run the TUI against one shared gateway, and a plugin records which sessions belong to which computer. The gateway sees one principal (often an operator), so `session.list` returns every session of the profile to every computer. The front end then has to refuse `picker` (the first prompt and last reply of every chat), `workspace` (chats by folder, also another computer's) and `title`, and the TUI's session picker cannot work through it.

Only the plugin knows which sessions belong to which caller, so Hermes core cannot do the filter itself. Filtering the answer in the front end does not work well either. `limit` and the stored feed's paging run before the front end sees the rows, so a scoped page comes back short or empty. `title` and `workspace` lookups would also need to be rebuilt outside the gateway. With `audience`, the gateway applies the filter inside the listing, after every other filter and before the page is cut to `limit`.

## Behavior

- Live feed: the hook gets the ids that pass the source, hidden and `workspace` filters, in one call. The page is then cut to `limit`, and the picker fields are added only for rows that were kept.
- Stored feed (`scope: "stored"`): candidates go to the hook in batches of at least 50, and the page holds up to `limit` kept rows. Without `audience` the loop reads exactly as many candidates as before.
- `title` lookup: the one resolved session is judged. A title outside the audience returns no rows.
- `include_hidden`, `workspace`, `picker`, `title` and `scope` all stay inside the audience. No param widens it.
- Fails closed: when no callback judges the audience (none answered, or each one raised or timed out), the request fails with `invalid_params`, `field: "audience"`. A scoped listing never falls back to the unscoped one. The hook is timeout-bounded (`plugins.hook_callback_timeout`), so a hung plugin cannot stall the listing.
- `audience` narrows the result. It is not an identity, so any client that may list may send it.

## Config and defaults

No new config. The param is optional and the hook has no default consumer, so behavior stays the same for every existing client.

## Files

- `gateway/session_listing.py`: `visible_to`, plus the stored feed split into a candidate generator so it can filter in batches.
- `gateway/session_picker_fields.py`: `live_picker_page` takes `audience`.
- `gateway/session_controls.py`: validates `audience`, passes it to both feeds and to the title lookup, and adds the capability flag.
- `hermes_cli/plugins.py`, `hermes_cli/plugins_dispatch.py`: the hook name, added to the timeout-bounded set.
- `tui_gateway/contracts/canonical_sessions.py` and the regenerated `apps/shared` contract files.
- Docs: `website/docs/user-guide/features/hooks.md` (table row and section) and `website/docs/developer-guide/gateway-local-sessions.md`.

## Tests

`tests/gateway/test_session_list_audience.py`:

- Live feed: without `audience`, both sessions are listed and the hook is not called. With `audience`, only the kept session is listed, also with `picker`, `workspace` (its own folder and another folder), `include_hidden` and `title` (its own title and another session's title).
- Stored feed: the unscoped order and `limit` stay the same. A scoped page with `limit=1` returns the kept row even though two newer rows come before it, with one hook call. `workspace`, `include_hidden` and `picker` stay scoped.
- Refusals: an audience no hook judges is refused on the live, stored, `title` and `picker`/`workspace` paths. A raising callback fails closed. Malformed values (`""`, a number, a list) return `invalid_params`.

The neighboring listing, picker, contract and hook test files pass unchanged.
