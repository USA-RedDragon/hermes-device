# fix(agent): scope model.ollama_num_ctx to self-hosted routes

## What does this PR do?

`model.ollama_num_ctx` describes the window a self-hosted server serves. Today it is applied to whatever route the agent runs on. A config that pins it for a local Ollama therefore also clamps every other provider in the same config:

- At construction, `_configure_ollama_num_ctx` takes the explicit value without looking at the route, and `_clamp_compressor_to_ollama_num_ctx` then shrinks the compressor window to it. With `model.ollama_num_ctx: 131072`, a chat on a 1M-window Claude model compresses as if it had 131,072 tokens, and `extra_body.options.num_ctx` is sent to that route.
- The `is_local_endpoint()` gate that some open PRs use does not catch every case. `process://` providers (`claude-subscription`, external-process providers) have an unqualified host, so `is_local_endpoint("process://claude-subscription")` is `True`.
- `_ollama_num_ctx` is resolved once. `/model`, fallback activation and the primary restore move a live agent to another route without re-resolving it. The old route's num_ctx keeps being sent and keeps feeding the minimum-context check and the reasoning-effort gate. A session that starts on a hosted model and is moved onto Ollama sends no num_ctx, so Ollama falls back to its small default.

## Changes

- `agent/agent_init_ollama.py`: the explicit `model.ollama_num_ctx` applies only when `_route_serves_num_ctx(agent)` is true. That means the base URL maps to no known hosted provider (Ollama Cloud included), and `detect_local_server_type()` recognizes the server as Ollama, llama.cpp, vLLM or LM Studio. This keeps the documented behavior that the key declares the served window of any local inference server (#87075 floor guidance). Auto-detection is unchanged. A new `refresh_ollama_num_ctx(agent)` re-reads config and re-resolves num_ctx, plus the compressor clamp, for the current route.
- `agent/agent_runtime_helpers.py` (`switch_model`) and `agent/route_binding.py` (`bind_route_entry`, used by fallback and per-turn routes) call `refresh_ollama_num_ctx` after they re-resolve the compressor.
- `agent/primary_runtime_snapshot.py` records `ollama_num_ctx` in both snapshot builders. `_apply_primary_runtime_fields` restores it, so a primary restore after a fallback puts the primary's num_ctx back without a new probe. A snapshot without the key keeps the current value.
- Docs: `website/docs/integrations/providers.md` and `cli-config.yaml.example` describe the scope.

No new config key. With one self-hosted route, behavior is unchanged. The positive server-type verdict is cached, and the probe runs only when the key is set. On a hosted URL that is not in the provider table, the key costs one cached probe per endpoint.

## How to test

`pytest tests/agent/test_ollama_num_ctx.py`. The new `TestNumCtxPinIsScopedToTheSelfHostedRoute` cases cover these routes:

- An Ollama route applies the pin. The wire carries `options.num_ctx` and the compressor is clamped.
- A hosted route in the same process and config keeps its 1M window and sends no `options`.
- `process://` and Ollama Cloud URLs are not num_ctx routes.
- `/model` from Ollama to hosted and back drops the pin, then restores it.
- `/model` from hosted onto Ollama picks the pin up.
- Fallback to a hosted model and the primary restore drop the pin, then restore it.

The existing `_build_agent` helper now stubs server detection, because the explicit pin depends on it.

Example config that triggered the bug:

```yaml
model:
  provider: custom
  base_url: http://ollama.example.internal:11434/v1
  default: qwen3:32b
  ollama_num_ctx: 131072
```

Then `/model claude-opus-5 --provider claude-subscription` (or any hosted provider). Before this change, the compressor window is 131072. After it, the window is the model's own (1,000,000).

## Related

Overlaps with the open #134389, #134145, #113832, #106756, #104725 and #99953. Those gate on `is_local_endpoint()`, which accepts `process://` routes. Most of them also cover only construction or only the wire. This PR covers construction, `/model`, fallback and restore together.
