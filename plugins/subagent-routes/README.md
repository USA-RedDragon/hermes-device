# subagent-routes

Registers the `model_map` delegation backend with a per-chat choice. It reads the same
`delegation.backends.model_map.options` as Hermes's advanced-delegation plugin (disable that plugin:
`hermes plugins disable advanced-delegation`), so `delegate_task` keeps the same per-task `option`.
Which routes exist, which models they use and how they are described all come from your config; the
plugin names no model or provider.

## Config

```yaml
delegation:
  backend: model_map
  backends:
    model_map:
      default: standard          # route a task without `option` uses (else the chat's own model)
      guidance:                  # optional; how the model should pick in Auto, in your words
        picker: "cheap routes for volume work, strong ones for hard judgment"
        note: "cheap routes for volume work, strong ones for hard judgment"
        description: "Prefer cheap routes for volume work and strong ones for hard judgment."
      options:
        - name: cheap            # required: name, description, model, and provider or base_url
          title: Cheap model     # shown in the hub's picker (default: the model)
          description: "Fast and cheap. Simple single-step work."
          provider: my-provider
          model: model-a
        - name: strong
          title: Strong model
          description: "Hard judgment only."
          provider: my-provider
          model: model-b
          helper: true           # also offered as "<title> when stuck": one helper at a time
          auto: false            # Auto never picks it; only a pin to this route does
          via: My provider, pay per token   # picker line: "... through <via>."
          # line / emphasis replace the whole picker line and its bold part
          # also passed to the child: base_url, api_key, api_mode, reasoning_effort, service_tier, fallback_providers

plugins:
  entries:
    subagent-routes:
      settings:
        wait_guidance: true      # tell subagents (each turn) and chats (once) to wait without sleep polling
        sleep_guard: true        # refuse a foreground `terminal` command that waits by sleeping
```

Route names `auto`, `none` and anything starting with `help-` are reserved.

## Per-chat choice

The hub sets it through `GET/PUT /api/plugins/subagent-routes/sessions/<session id>`: `auto`, a route name
(every subagent runs there), `help-<route>` (subagents run on the chat's own model; the model may start one
helper on that route), or `none` (subagents refused). Choices are kept in `$HERMES_HOME/subagent-routes/choices.json`
and follow a chat across context compression. The chat's next turn gets one line saying the rule.

## Without the guidance keys

Auto then says "from the route descriptions", and the `delegate_task` description has no preference sentence.
