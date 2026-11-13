# Choose a model and provider

Talaria includes its own Hermes, the agent that talks to models. You do not need a separate Hermes, and an
existing Hermes install is not used. Models and providers are settings of the Hermes inside Talaria. You connect
them once, after you make your account: from Talaria's own page (Connect a model), or with Hermes's own setup
command, `hermes model`, run in Talaria's container. The name you give a connection becomes its group in the chat's
model menu, where you pick the model for each chat.

Hermes keeps what you set (and any API key you give it) in its data folder, so it survives upgrades. Talaria keeps
none of it. Until you connect a model, the chat area says "Talaria has no model yet" and a new chat cannot start.

## Connect a model from Talaria

Admins of the hub (the account made on the setup page, and anyone made an admin in Settings, Users; with a login
proxy or OIDC, the people in `auth.admins`) see **Connect a model** in the chat area while no model is connected,
and a Models tab in Settings at any time. Everyone else sees "No model is set up yet" and a Check again button.

With more than one backend, `models.admins` (environment variable `HUB_MODELS_ADMINS`) says who connects models to
which backend's Hermes, as `user=backend` pairs, the same form as `claude.admins`: `alice=main,bob=family`. Once it
is set, only those pairs may, hub admins included; empty (the default) leaves it to the hub's admins on every
backend they use.

Connect a model asks where the model runs:

- **On my own computer or network**: Ollama, LM Studio, vLLM, llama.cpp, or any server that speaks the OpenAI
  API. Give it a name and the address, ending in `/v1` for Ollama and LM Studio, then **Test address**: Hermes
  calls the address from its own container and lists the models it finds. Pick the one new chats start with, then
  **Save and use**. From a container, `localhost` is the container itself, so use the address of the machine that
  runs the server (see [Ollama](#ollama)). Advanced holds the API type, the context window and whether the model
  list follows the server.
- **A provider, with an API key**: OpenRouter, Anthropic, OpenAI, Google AI Studio, DeepSeek, xAI, or any of the
  others under **All providers**. Paste the key and **Check and connect**. Hermes checks keys for OpenRouter,
  OpenAI, xAI and Google AI Studio with the provider before it saves them; a rejected key is not saved. Keys of
  the other providers are saved but not checked: if the first reply fails with an authentication error, edit the
  key in Settings, Models. A backend whose Hermes runs Claude subscription accounts (the Claude accounts plugin)
  does not offer an Anthropic API key here, so the two do not compete.
- **A subscription account**: ChatGPT or Codex, Nous Portal, xAI Grok or MiniMax. Talaria shows a code and a link
  to the provider's page, where you sign in and enter the code; it waits until you approve. A code works for 15
  minutes; **Get a new code** starts again.

Then pick the model new chats start with (Hermes suggests one), or skip and pick in a chat. After that, every
model of the provider is in the chat's model menu, which an admin can also use to open Connect a model again.

**Settings, Models** lists what Hermes has connected. Each row can be edited (a key field left empty keeps the
saved key; Talaria never shows a key, only Hermes's masked preview), tested again, made the default for new chats,
or removed. Removing the default asks which model to use instead (a model Hermes calls high-priced asks for a yes
first, as Make default does); removing the only one brings back the Connect a model card. A default Hermes did not
change is reported as such, never as done.

### Models the deployment sets

A deployment can set models outside Hermes's own files: keys in the container's environment (Compose `.env`,
Kubernetes secrets), and with Helm or Flux a managed Hermes config (`HERMES_MANAGED_DIR`) that sets the default
model and servers. Hermes reads those over its own settings, so a change made in Talaria would be overridden, come
back after a restart, or (a key saved over one from the environment) hide the deployment's key for good.

With `dashboard.managed_read_only: true` in Hermes's config (Talaria's kits set it), Talaria shows these read-only:
servers from the managed config are marked **Set in the deployment config**, keys from the environment **Set in the
deployment**, and neither has Edit or Remove. When the managed config sets the default model, Settings, Models says
where, Make default is gone, and Connect a model skips the pick of a default: new chats keep starting with the
deployment's model, and what you connect is in the chat's model menu. Hermes refuses any change to these with a
message that says where they are set, and writes nothing. Change them where the deployment sets them.

Anthropic account sign-in, Qwen sign-in, AWS Bedrock and Google Vertex AI cannot be set up from the page: use
`hermes model` on the server, below.

## Set a model on the server

`hermes model` does the same from a terminal, and covers every provider Hermes knows. Run it in the Hermes
container, as the `hermes` user:

| Install | Command |
| ------- | ------- |
| Compose (in the folder with `compose.yaml`) | `docker compose exec -u hermes hermes hermes model` |
| All-in-one, Unraid (container named `Talaria`) | `docker exec -it -u hermes Talaria hermes model` |
| TrueNAS (app named `talaria`) | `sudo docker exec -it -u hermes $(sudo docker ps -qf name=talaria) hermes model` |
| Helm | `kubectl -n talaria exec -it deploy/talaria-hermes -c gateway -- hermes model` |

What you will see: a list of providers, which you move through with the arrow keys. Pick one and give it what it
asks for (an API key, a sign-in, or an address), then pick the model. For Ollama, LM Studio or vLLM, pick **Custom
endpoint**, the last entry (see [Ollama](#ollama)). At the end Hermes asks for a display name: that name is the
group your models show under in the chat's model menu.

What happens next: open a new chat (or reload the page) and pick the model in the chat's model menu. No restart is
needed. Models you pull into Ollama later appear on their own. Run `hermes model` again at any time to add another
connection or switch. Inside a chat, `/model` switches between providers you already set up.

Which providers Hermes offers, what each needs and how to set them in Hermes's `config.yaml` instead: Hermes
Agent's [LLM and model providers](https://hermes-agent.nousresearch.com/docs/integrations/providers) page. Hermes
needs a model with a context window of at least 64,000 tokens.

## Ollama

From Talaria: Connect a model, **On my own computer or network**, address `http://<Ollama's address>:11434/v1`
(`http://ollama:11434/v1` for the compose `ollama` profile), Test address, pick the model, Save and use. If the
model's window is under 64,000 tokens, Talaria says so and lets you pick another.

With `hermes model`, Ollama is Hermes's **Custom endpoint**:

1. Run `hermes model`, pick **Custom endpoint** (the last entry of the list).
2. API base URL: `http://<Ollama's address>:11434/v1`. From a container, `localhost` is the container itself:
   use the address of the machine that runs Ollama (such as `http://192.168.1.10:11434/v1`; Ollama there must
   listen on that address, `OLLAMA_HOST=0.0.0.0:11434`), or `http://ollama:11434/v1` for the compose `ollama`
   profile. Hermes checks the address at once and says how many models it sees.
3. Leave the API key empty and press Enter for the API compatibility mode (auto-detect). Hermes lists Ollama's
   models by number: type the number of yours. Press Enter for the context length (Hermes asks Ollama for the
   model's window and sends it with every request), the display name and the reasoning effort.
4. Hermes prints `Default model set to: <model> (via <address>)`. Start a new chat; the model's name shows above
   the chat.

Ollama's own settings matter too (they are Ollama's, not Talaria's):

- Pick a model that handles tool calls and a 64K window, such as `llama3.1:8b` or `gpt-oss:20b`. `qwen3` (40K) and
  `qwen2.5` (32K) in Ollama are too small for Hermes. Small CPU models (`llama3.2:3b`) work but are slow
  and often write tool calls as plain text.
- `OLLAMA_CONTEXT_LENGTH=65536` (or more) on the Ollama container sets the window it serves to clients that do not
  ask for one; the compose `ollama` profile sets it. Hermes asks for the window itself, so this is a safety net.
- On Unraid, a container on a custom `br0` network cannot reach one on the host's bridge (and the other way round)
  unless **Settings, Docker, Host access to custom networks** is on. Using the server's address and Ollama's
  published port 11434 works in the default bridge setup.

Compose can start Ollama for you: add `ollama` to `COMPOSE_PROFILES` and set `TALARIA_OLLAMA_PULL` to a model to
download on start (see `.env`).

## The demo model

For trying Talaria without any provider. It needs no key and no download, and it answers with a canned reply. Send
`run: <command>` and it makes the agent run that command in the sandbox, so you can see the whole path work.

Turn it on, then pick it in `hermes model` (it is listed as `talaria-demo`, near the end), or set it without the picker:

```bash
hermes config set model.provider custom:talaria-demo
hermes config set model.default talaria-demo
```

(in the Hermes container, like `hermes model` above). To turn it on:

- Compose: `COMPOSE_PROFILES=demo` in `.env`.
- Helm: `--set demo.enabled=true`.
- All-in-one, Unraid, TrueNAS: `TALARIA_DEMO=true`.

It does not understand language. Pick a real model when you are done looking around.

## Subscriptions

Talaria can show how much of a ChatGPT subscription you used, when Hermes is signed in to ChatGPT with its own
`openai-codex` provider (`hermes model`, OpenAI). That sign-in is a Hermes feature; it is untested in these
installs.

## Provider keys from the environment

Hermes also reads provider keys from environment variables, such as `OPENROUTER_API_KEY` (the names are on Hermes's
providers page). `hermes model` saves keys you type in Hermes's own `.env` in its data folder, which is usually
simpler. To pass them from outside instead:

- Compose: a `compose.override.yaml` with the variables under `services: hermes: environment:`.
- Helm: `hermes.env`, and Hermes settings in `hermes.config`.
- All-in-one: variables on the container.
