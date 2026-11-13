# Choose a model and provider

Talaria includes its own Hermes, the agent that talks to models. You do not need a separate Hermes, and an
existing Hermes install is not used. Models and providers are settings of the Hermes inside Talaria. You connect
them once, after you make your account, with Hermes's own setup command, `hermes model`, run in Talaria's
container (the commands are below). The display name you give a connection becomes its group in the chat's model
menu, where you pick the model for each chat.

Hermes keeps what you set (and any API key you give it) in its data folder, so it survives upgrades. Until you
connect a model, a new chat cannot start: it says Hermes is not connected to any AI provider, and the model menu
is empty.

## Run `hermes model`

Run it in the Hermes container, as the `hermes` user, from a terminal on the server:

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

[Ollama](https://ollama.com) is reached through Hermes's **Custom endpoint**:

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
