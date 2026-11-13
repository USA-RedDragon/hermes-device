# Install Talaria on TrueNAS SCALE

TrueNAS SCALE 24.10 (Electric Eel) and later run apps with Docker Compose. Talaria installs as a custom app from
YAML, with the all-in-one image ([design](all-in-one.md)).

1. **Datasets**: add two datasets with the **Apps** preset, for example `tank/apps/talaria/hermes` and
   `tank/apps/talaria/talaria`.
2. **Apps**, **Discover Apps**, the menu next to **Custom App**, **Install via YAML**.
3. Name it `talaria` and paste the file below. Change the lines marked `CHANGE`: your TrueNAS address in
   `TALARIA_PUBLIC_URL`, and the dataset paths.
4. **Save**. The first start downloads about 1.2 GB.
5. Open the app's **Logs** and find `Open:`, `Log in as:` and `Password:`. Open the address and log in.

For real use, set `TALARIA_MODEL_BASE_URL`, `TALARIA_MODEL` and `TALARIA_MODEL_API_KEY`, and `TALARIA_DEMO` to
`"false"` (**Edit** on the app).

`PUID` and `PGID` 568 are the TrueNAS `apps` user, which owns datasets with the Apps preset.

<!-- docgen:code file=aio/truenas/compose.yaml -->
```yaml
# TrueNAS SCALE (24.10 and later): Apps, Discover Apps, the menu next to Custom App, Install via YAML. Paste this
# file, change the four lines marked CHANGE, and save. See docs/truenas.md
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
      # A stand-in model to try Talaria. For real use set the three TALARIA_MODEL settings and make this false
      TALARIA_DEMO: "true"
      TALARIA_MODEL_BASE_URL: ""
      TALARIA_MODEL: ""
      TALARIA_MODEL_API_KEY: ""
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

## A TrueNAS catalog entry (not done yet)

The TrueNAS apps catalog (github.com/truenas/apps) takes an app as `ix-dev/community/<name>/` with `app.yaml`,
`questions.yaml` (the install form) and `templates/docker-compose.yaml` rendered by their Python library. A Talaria
entry would ask for the public URL, the model settings and two storage locations, and render the same single
service as the file above. It needs a published all-in-one image first.
