# Choosing models and providers

Hermes does the talking to the model. Talaria's installs set up one model for you, and you can add others in Hermes later. This page covers the four choices: the demo model, a hosted API, local models with Ollama, and subscriptions.

## Demo model

For trying Talaria. It needs no key and no download, and it answers with a canned reply. Send `run: <command>` and it makes the agent run that command in the sandbox, so you can see the whole path work.

- Compose: `COMPOSE_PROFILES=demo`, the default in `.env`.
- Helm: `--set demo.enabled=true`.
- All-in-one: `TALARIA_DEMO=true`.

It does not understand language. Turn it off when you set a real model.

## A hosted API

Any service with an OpenAI-compatible API works: OpenRouter, OpenAI, DeepInfra, Together, Groq, and others. You need the API's base URL, a model name and a key.

| Service | Base URL | Model name looks like |
| ------- | -------- | --------------------- |
| OpenRouter | `https://openrouter.ai/api/v1` | `anthropic/claude-sonnet-4.5` |
| OpenAI | `https://api.openai.com/v1` | `gpt-5-mini` |
| DeepInfra | `https://api.deepinfra.com/v1/openai` | the model's name on DeepInfra |

Model names change often. Take the current one from the service's own list.

**Compose:** edit `.env`, take `demo` out of `COMPOSE_PROFILES`, and set:

```sh
COMPOSE_PROFILES=
TALARIA_MODEL_BASE_URL=https://openrouter.ai/api/v1
TALARIA_MODEL=anthropic/claude-sonnet-4.5
TALARIA_MODEL_API_KEY=sk-or-...
```

Then `docker compose up -d --wait` and `docker compose rm -sf demo-llm`. New chats use the new model.

**Helm:** put the key in a Secret and point the chart at it:

```sh
kubectl -n talaria create secret generic talaria-model --from-literal=api-key=sk-or-...
helm upgrade talaria ./talaria -n talaria --reuse-values \
  --set demo.enabled=false \
  --set model.baseUrl=https://openrouter.ai/api/v1 \
  --set model.name=anthropic/claude-sonnet-4.5 \
  --set model.existingSecret=talaria-model
```

**All-in-one:** set the same `TALARIA_MODEL_*` variables on the container.

The model must handle a context window of at least 64,000 tokens. Hermes refuses smaller ones and says so in the chat. If your service reports a different window, set `TALARIA_MODEL_CONTEXT` (compose) or `model.contextLength` (Helm).

## Local models with Ollama

Compose can start [Ollama](https://ollama.com) for you:

```sh
COMPOSE_PROFILES=ollama
TALARIA_OLLAMA_PULL=llama3.2:3b
TALARIA_MODEL=llama3.2:3b
TALARIA_MODEL_BASE_URL=http://ollama:11434/v1
```

The first start downloads the model. Pick one that handles a 64K window or more: Ollama's `qwen3` models have 40K, so Hermes refuses them.

Know what to expect:

- Without a GPU, local models are slow and weak at using tools. On a test machine with 6 CPU cores and no GPU, a 1B model took 80 seconds to answer, used 5.5 GB of RAM and wrote a tool call as plain text instead of calling the tool.
- Hermes sends a long prompt with every turn, so a small local model spends most of its time reading it.
- If you already run Ollama on another machine with a GPU, leave the `ollama` profile off and set `TALARIA_MODEL_BASE_URL` to `http://<that machine>:11434/v1`.

## Subscriptions

Talaria can show how much of a ChatGPT subscription you used, when Hermes is signed in to ChatGPT with its own `openai-codex` provider. That sign-in is a Hermes feature, not a Talaria one, and the install files here do not set it up. It is untested in these installs.

## Several models, and other providers

The installs set one model. Hermes itself can use many providers and switches between them in a chat (the model picker in the web app lists what Hermes offers). To add providers beyond the one in `.env`:

- Helm: put Hermes settings in `hermes.config` and provider keys in `hermes.env`.
- Compose: add environment variables to the `hermes` service in a `compose.override.yaml`.

Which variable name a provider reads is in [Hermes Agent's docs](https://github.com/NousResearch/hermes-agent). This path is not covered by the install tests.
