# Install on TrueNAS SCALE

TrueNAS SCALE 24.10 (Electric Eel) and later run apps with Docker Compose. Talaria installs as a custom app from
YAML, with the all-in-one image ([design](all-in-one.md)). Tested on TrueNAS SCALE 25.10.

1. **Datasets**: add two datasets with the **Apps** preset, for example `tank/apps/talaria/hermes` and
   `tank/apps/talaria/talaria`.
2. **Apps**, **Discover Apps**, the menu (three dots) next to **Custom App**, **Install via YAML**.

   ![The Discover Apps page of TrueNAS, with the Custom App button and the menu next to it at the top right](images/truenas/discover.png)

3. Name it `talaria` and paste the file below. Change the lines marked `CHANGE`: your TrueNAS address in
   `TALARIA_PUBLIC_URL`, and the dataset paths. To try Talaria without a provider, also set `TALARIA_DEMO` to
   `"true"` (as in the picture).

   ![The Install via YAML form: the name talaria and the compose file pasted in the Custom Config box](images/truenas/install-yaml.png)

4. **Save**. The first start downloads about 1.2 GB; the app shows **Running** under **Apps, Installed** within a
   minute or two.

   ![The Installed apps page: talaria is Running, and Workloads lists port 8080 and the talaria container](images/truenas/app-running.png)

5. Open `http://<TrueNAS address>:8080` right away (custom apps have no **Web UI** button). The setup page makes the
   first account, an admin; the first person who opens it gets that account. Add other people under Settings,
   Users.

   ![The Talaria setup page with the user name alice and a display name filled in](images/truenas/setup.png)

## Pick a model in Hermes

The model is Hermes's own setting; until you pick one, a chat answers that Hermes is not connected to any AI
provider. Open a shell in the container: **Apps**, **Installed**, `talaria`, then under **Workloads**,
**Containers**, the shell icon (the first icon next to `talaria – Running`). That shell runs as root; run Hermes as
its own user:

```bash
runuser -u hermes -- hermes model
```

From **System, Shell** or SSH instead:
`sudo docker exec -it -u hermes $(sudo docker ps -qf name=talaria) hermes model`.

Pick a provider and give it what it asks for. With `TALARIA_DEMO` on, `talaria-demo` is near the end of the list
(arrow keys, Enter, then Enter for its one model). For Ollama, see [Ollama as a TrueNAS app](#ollama-as-a-truenas-app).
The next new chat uses the model; no restart needed.

![hermes model in the TrueNAS container shell, with talaria-demo selected near the end of the provider list](images/truenas/hermes-model-demo.png)

![A chat in Talaria where the demo model answers "This is the Talaria demo model"](images/truenas/chat-demo.png)

`PUID` and `PGID` 568 are the TrueNAS `apps` user, which owns datasets with the Apps preset.

<!-- docgen:code file=deploy/aio/truenas/compose.yaml -->
```yaml
# TrueNAS SCALE (24.10 and later): Apps, Discover Apps, the menu next to Custom App, Install via YAML. Paste this
# file, change the lines marked CHANGE, and save. See https://usa-reddragon.github.io/talaria/deploy/truenas.html
services:
  talaria:
    image: ghcr.io/usa-reddragon/talaria-aio:latest
    restart: unless-stopped
    stop_grace_period: 2m
    ports:
      - "8080:8080"
    environment:
      # CHANGE: the address you open Talaria at: http://<TrueNAS address>:8080, or https://... behind a proxy
      TALARIA_PUBLIC_URL: http://192.168.1.10:8080
      # "true": a stand-in model to try Talaria without a provider (pick talaria-demo in hermes model). The model
      # and provider are Hermes settings: see the TrueNAS page of the docs
      TALARIA_DEMO: "false"
      TZ: UTC
      # The "apps" user of TrueNAS owns the Hermes data
      PUID: "568"
      PGID: "568"
    volumes:
      # CHANGE: datasets on your pool. Create them first (Datasets, Add Dataset, preset Apps)
      - /mnt/tank/apps/talaria/hermes:/opt/data
      - /mnt/tank/apps/talaria/talaria:/talaria
```
<!-- /docgen:code -->

## Ollama as a TrueNAS app

1. **Apps**, **Discover Apps**, search for **Ollama**, **Install**. Under **Additional Environment Variables**, add
   `OLLAMA_CONTEXT_LENGTH` = `65536` (Ollama's setting). Note the API port (the app's default is `30068`). Under
   **Resources Configuration**, the default limit of 2 CPUs and 4096 MB is too small for a 64K window: give it the
   CPUs you can spare and at least 8192 MB for a 3B model. Without a GPU, `OLLAMA_KV_CACHE_TYPE` = `q8_0` halves
   the memory the window takes.

   ![The Ollama install form with OLLAMA_CONTEXT_LENGTH 65536 and OLLAMA_KV_CACHE_TYPE q8_0 added](images/truenas/ollama-install.png)

2. Download a model that handles tool calls and a 64K window: open the **ollama** app's container shell (as for
   Talaria above) and run `ollama pull`. `llama3.1:8b` suits a GPU or 16 GB to spare; `llama3.2:3b` (2 GB) runs on
   a CPU but slowly.

   ```bash
   ollama pull llama3.2:3b
   ```

   ![ollama pull llama3.2:3b in the Ollama container shell, then ollama show with a 131072 context length and the tools capability](images/truenas/ollama-pull.png)

3. Point Hermes at it: `runuser -u hermes -- hermes model` in the Talaria shell, **Custom endpoint** (right after
   `talaria-demo` and **Local models**), API base URL `http://<TrueNAS address>:30068/v1`, then Enter for the rest:
   no API key, auto-detect, the model it found, an empty context length (Hermes asks Ollama), the display name and
   the reasoning effort.

   ![hermes model with the custom endpoint http://10.0.2.15:30068/v1: Verified endpoint, Detected model llama3.2:3b, Default model set to llama3.2:3b](images/truenas/hermes-model-ollama.png)

![A Talaria chat on llama3.2:3b: it ran uname -r with a command (shown as "Ran a command") and answered "The kernel version is 6.12.113-production+truenas"](images/truenas/chat-ollama.png)

On 4 CPU cores, that first answer took about ten minutes (Hermes sends about 13K tokens of instructions and
tools). Use a GPU or a provider for daily use.

More in [Choosing models and providers](models.md#ollama).

## A TrueNAS catalog entry (not done yet)

The TrueNAS apps catalog (github.com/truenas/apps) takes an app as `ix-dev/community/<name>/` with `app.yaml`,
`questions.yaml` (the install form) and `templates/docker-compose.yaml` rendered by their Python library. A Talaria
entry would ask for the public URL, the model settings and two storage locations, and render the same single
service as the file above. It can use the published all-in-one image.
