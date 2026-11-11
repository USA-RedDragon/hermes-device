# feat(compression): summarise listed or larger-window chats on their own main model

## What

Two new keys under `auxiliary.compression`:

- `main_model_providers` (list, default `[]`): a chat whose current main provider is in this list sends its compression summary to its own main model on its own endpoint, instead of the configured compression route.
- `main_model_when_larger` (bool, default `false`): the same for any chat whose main context window is larger than the compression model's window, as measured by the feasibility probe.

A chat that summarises on its own main model keeps the trigger from its own window and `compression.threshold`. The feasibility probe does not lower it to the compression model's window, and a ceiling or "auto-lowered" notice installed by an earlier probe is dropped. Every other chat behaves exactly as today.

## Why

`auxiliary.compression` is one route for every chat. A common setup is a small local model as the summariser for local chats, plus a hosted large-window model that some chats switch to. Today the hosted chats either summarise on the small model, and the probe lowers their trigger to the small model's window (a 1M-window chat compacts at about 131K instead of about 500K), or the operator points compression at the main model and the local chats lose their cheap summariser. These keys let one profile keep both.

## Example

```yaml
auxiliary:
  compression:
    provider: custom
    base_url: http://localhost:11434/v1
    model: qwen3:4b
    context_length: 131072
    main_model_providers: [anthropic]
```

A chat on `anthropic` with a 1M window compacts at its normal threshold (500K at `threshold: 0.50`) and summarises on its own model. A chat on the local endpoint keeps `qwen3:4b`.

## How

- New sibling `agent/conversation_compression_route.py` holds the decision (`summarises_on_main_model`) and the probe hook (`keeps_main_model_summariser`). `_aux_inherits_main_route` moved there from `agent/conversation_compression.py` so the facade does not grow.
- `check_compression_model_feasibility` skips the aux probe for a listed provider, and after measuring the compression model's window it skips the clamp when the chat summarises on its main model.
- `SummaryDispatchMixin._apply_summary_route` pins the main runtime (the same authoritative route the aux-failure fallback uses, #123362) when the chat summarises on its main model. Both summary entry points (full compaction and micro-compaction) use it.
- The decision reads the live config and the compressor's current provider and window on every summary, so a chat switched to another model mid-session (`/model`, fallback, primary restore) follows its new route; those paths already re-run the probe.
- The new keys are part of the feasibility latch's route identity, so editing them on a running gateway re-probes a warm agent.

## Defaults

Both keys are off by default (`[]` and `false`), so behavior is unchanged unless an operator sets them. Added to `DEFAULT_CONFIG`, `cli-config.yaml.example` and the configuration docs.

## Tests

New `tests/agent/test_compression_main_model_route.py`:

- defaults keep the aux route and the aux clamp;
- a listed provider keeps its own threshold (including a non-default `compression.threshold`), does not resolve the aux client, and sends the summary on the main route;
- an unlisted provider keeps the aux route;
- `main_model_when_larger` compares the main window with the measured aux window (larger: main route, equal: aux route; unmeasured: aux route);
- a mid-session switch from an unlisted to a listed provider and back follows the current provider for both the threshold and the summary route;
- listing a provider later releases an installed clamp and its notice on the next probe.

Existing compression, feasibility, auxiliary routing and config test files pass unchanged.
