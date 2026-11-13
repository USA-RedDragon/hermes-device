# Install on TrueNAS SCALE

TrueNAS SCALE 24.10 (Electric Eel) and later run apps with Docker Compose. Talaria installs as a custom app from
YAML, with the all-in-one image ([design](all-in-one.md)).

1. **Datasets**: add two datasets with the **Apps** preset, for example `tank/apps/talaria/hermes` and
   `tank/apps/talaria/talaria`.
2. **Apps**, **Discover Apps**, the menu next to **Custom App**, **Install via YAML**.
3. Name it `talaria` and paste the file below. Change the lines marked `CHANGE`: your TrueNAS address in
   `TALARIA_PUBLIC_URL`, and the dataset paths.
4. **Save**. The first start downloads about 1.2 GB.
5. Open `http://<TrueNAS address>:8080` (or the app's **Web UI** button) right away. The setup page makes the
   first account, an admin; the first person who opens it gets that account. Add other people under Settings,
   Users.

Then pick a model in Hermes (Hermes's own setting), from **System, Shell** or SSH:

```bash
sudo docker exec -it -u hermes $(sudo docker ps -qf name=talaria) hermes model
```

Pick a provider and give it what it asks for. To look around without a provider, set `TALARIA_DEMO` to `"true"`
(**Edit** on the app) and pick `talaria-demo`. For Ollama, see [Ollama as a TrueNAS app](#ollama-as-a-truenas-app).

`PUID` and `PGID` 568 are the TrueNAS `apps` user, which owns datasets with the Apps preset.

<!-- docgen:code file=deploy/aio/truenas/compose.yaml -->
```yaml
# TrueNAS SCALE (24.10 and later): Apps, Discover Apps, the menu next to Custom App, Install via YAML. Paste this
# file, change the four lines marked CHANGE, and save. See https://usa-reddragon.github.io/talaria/deploy/truenas.html
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

1. **Apps**, **Discover Apps**, search for **Ollama**, **Install**. Note its web port (the app's default is
   `30068`) and add the environment variable `OLLAMA_CONTEXT_LENGTH` = `65536` (Ollama's setting).
2. Download a model that handles tool calls and a 64K window, from **System, Shell**:

   ```bash
   sudo docker exec -it $(sudo docker ps -qf name=ollama) ollama pull llama3.1:8b
   ```

3. Point Hermes at it: `hermes model` as above, **Custom endpoint**, API base URL
   `http://<TrueNAS address>:30068/v1`, no API key, the model it lists, and an empty context length.

More in [Choosing models and providers](models.md#ollama).

## A TrueNAS catalog entry (not done yet)

The TrueNAS apps catalog (github.com/truenas/apps) takes an app as `ix-dev/community/<name>/` with `app.yaml`,
`questions.yaml` (the install form) and `templates/docker-compose.yaml` rendered by their Python library. A Talaria
entry would ask for the public URL, the model settings and two storage locations, and render the same single
service as the file above. It needs a published all-in-one image first.
