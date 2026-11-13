# Install on Unraid

Talaria on Unraid is one container from the all-in-one image ([design](all-in-one.md)).

1. Get the template. Unraid removed the **Template repositories** field from the Docker tab in 6.10, so there is
   no URL to add there. Instead, download the template onto the server. In the Unraid terminal (the **>_** icon at
   the top right of the Unraid page; it runs as root. Not a container's **Console**, which cannot reach
   `/boot`), run:

   ```bash
   mkdir -p /boot/config/plugins/dockerMan/templates-user
   curl -fL -o /boot/config/plugins/dockerMan/templates-user/my-Talaria.xml \
     https://raw.githubusercontent.com/USA-RedDragon/talaria/main/deploy/aio/unraid/talaria.xml
   ```

   Then, on the **Docker** tab, click **Add Container** and pick **Talaria** under **User templates** in the
   **Template** list. To update the template later, run the same `curl` again.
2. Set **Public URL** to `http://<your Unraid address>:8080`, such as `http://192.168.1.10:8080`.
3. To look around without a model provider, set **Demo model** to `true`.
4. **Apply**. The first start downloads about 1.2 GB.
5. Right after the start, open **WebUI** in the container's menu (or `http://192.168.1.10:8080`). The setup page
   makes the first account, an admin: the first person who opens it gets that account, so do it before the port
   is reachable from outside your network. Add other people under Settings, Users.
6. Connect a model. This step is required: until you do it, the chat area says Talaria has no model yet.
   Talaria includes its own Hermes, the agent that talks to models, so you do not need a separate Hermes, and an
   existing one is not used.

   As the admin, click **Connect a model** in the chat area (it shows until a model is connected; later it is
   in Settings, Models) and pick where the model runs: a server of your own (Ollama, LM Studio, vLLM, llama.cpp),
   a provider with an API key, or a subscription sign-in. Talaria checks the address or key through Hermes, lets
   you pick the model new chats start with, and keeps nothing itself: Hermes stores the provider and key.
   [Choosing models and providers](models.md) walks through each kind.

   Or, in the Unraid terminal (the **>_** icon at the top right), run
   Hermes's own setup command in the container:

   ```bash
   docker exec -it -u hermes Talaria hermes model
   ```

   You can also open the container's **Console** (its menu on the **Docker** tab); that shell is root, so run
   `runuser -u hermes -- hermes model` there.

   You will see a list of providers; move with the arrow keys. Pick one and give it what it asks for (an API
   key, a sign-in, or an address), or `talaria-demo` with the demo model on. For Ollama, LM Studio or vLLM, pick
   **Custom endpoint**, the last entry (see [Ollama on the same server](#ollama-on-the-same-server)). Hermes
   asks for a display name at the end: it becomes the group your models show under in the chat's model menu.

   Then open a new chat (or reload the page) and pick the model in the chat's model menu. No restart is needed,
   and models you pull into Ollama later appear on their own. Details: [Choosing models and providers](models.md).

Data is in `/mnt/user/appdata/talaria`: `hermes` (chats, memory) and `talaria` (hub database, keys, the
sandbox's files). Back up both.

Stopping, restarting or updating the container interrupts a reply that is running; the chat says so, and you
send the message again. Talaria stops in a few seconds, well within Unraid's **Docker Stop Timeout** (Settings,
Docker; 10 seconds by default).

## Update and change settings

The template pulls `talaria-aio:latest`, so Unraid's own update check covers it:

- **Update:** on the **Docker** tab, click **Check for Updates** at the bottom. When the Talaria row says
  **update ready**, click it and pick **Apply Update**. Unraid pulls the new image and recreates the container
  with the same settings; chats, memory, accounts and the model setup stay, because they live in
  `/mnt/user/appdata/talaria`.
- **Change a setting** (Public URL, Telegram token, time zone): click the Talaria icon on the **Docker** tab,
  pick **Edit**, change it, **Apply**.

Both need the container to be made from the template. A container started by hand with `docker run` shows as
**3rd Party** on the Docker tab, with no **Edit** and an update status of **not available**. To move it onto the
template:

1. Note where its data lives, in the Unraid terminal:

   ```bash
   docker inspect Talaria --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'
   ```

2. Get the template (step 1 of the install above).
3. Remove the old container: its icon, **Remove**. This removes the container only; the folders from step 1 stay.
4. **Add Container**, pick **Talaria** under **User templates**, set **Hermes data** and **Talaria data** to the
   folders from step 1 (the ones mounted at `/opt/data` and `/talaria`), the same **Public URL**, and **Apply**.

Chats, accounts and the model setup come back as they were.

## Ollama on the same server

Ollama is its own container; Hermes reaches it by the server's address.

1. Install Ollama: in **Apps**, search for Ollama and install the `ollama/ollama` template, or on the **Docker**
   tab add a container with repository `ollama/ollama`, port 11434 and a path for `/root/.ollama`.
2. Add a variable to the Ollama container: key `OLLAMA_CONTEXT_LENGTH`, value `65536` (Ollama's setting: the
   window it serves to clients that do not ask for one).
3. Download a model that handles tool calls and a 64K window, in the Unraid terminal:

   ```bash
   docker exec -it ollama ollama pull llama3.1:8b
   ```

4. Point Hermes at it: `docker exec -it -u hermes Talaria hermes model`, pick **Custom endpoint**, API base URL
   `http://<your Unraid address>:11434/v1` (such as `http://192.168.1.10:11434/v1`), no API key, the model it
   lists, and an empty context length (Hermes asks Ollama).

Both containers on the default bridge network reach each other this way. A container on a custom `br0` network
cannot reach the server's own address unless **Settings, Docker, Host access to custom networks** is on. More in
[Choosing models and providers](models.md#ollama).

## Template

<!-- docgen:code file=deploy/aio/unraid/talaria.xml -->
```
<?xml version="1.0"?>
<!-- Unraid template for the Talaria all-in-one image. See https://usa-reddragon.github.io/talaria/deploy/unraid.html -->
<Container version="2">
  <Name>Talaria</Name>
  <Repository>ghcr.io/usa-reddragon/talaria-aio:latest</Repository>
  <Registry>https://github.com/users/USA-RedDragon/packages/container/package/talaria-aio</Registry>
  <Network>bridge</Network>
  <Privileged>false</Privileged>
  <Support>https://github.com/USA-RedDragon/talaria/issues</Support>
  <Project>https://github.com/USA-RedDragon/talaria</Project>
  <Overview>Talaria: a web app and Telegram bot for Hermes Agent, with a sandbox for the agent's commands and your own computers paired in. One container: hub, agent and sandbox. Open the web UI right after the first start: the first visitor makes the admin account.</Overview>
  <Category>AI: Productivity: Tools:</Category>
  <WebUI>http://[IP]:[PORT:8080]/</WebUI>
  <TemplateURL>https://raw.githubusercontent.com/USA-RedDragon/talaria/main/deploy/aio/unraid/talaria.xml</TemplateURL>
  <Icon>https://raw.githubusercontent.com/USA-RedDragon/talaria/main/hub/internal/web/static/icon-512.png</Icon>
  <Shell>bash</Shell>
  <DonateText/>
  <DonateLink/>
  <Config Name="Web port" Target="8080" Default="8080" Mode="tcp" Description="Port of the web app" Type="Port" Display="always" Required="true" Mask="false">8080</Config>
  <Config Name="Public URL" Target="TALARIA_PUBLIC_URL" Default="" Description="The address you open Talaria at, such as http://192.168.1.10:8080 (this server's address and the web port) or https://talaria.example.com behind a reverse proxy. Pages and push notifications need HTTPS." Type="Variable" Display="always" Required="true" Mask="false"/>
  <Config Name="Hermes data" Target="/opt/data" Default="/mnt/user/appdata/talaria/hermes" Mode="rw" Description="Chats, memory and agent settings" Type="Path" Display="always" Required="true" Mask="false">/mnt/user/appdata/talaria/hermes</Config>
  <Config Name="Talaria data" Target="/talaria" Default="/mnt/user/appdata/talaria/talaria" Mode="rw" Description="Hub database, keys, and the sandbox's files" Type="Path" Display="always" Required="true" Mask="false">/mnt/user/appdata/talaria/talaria</Config>
  <Config Name="Demo model" Target="TALARIA_DEMO" Default="false" Description="true: a stand-in model that needs no API key, to try Talaria (pick talaria-demo with: docker exec -it -u hermes Talaria hermes model). The model and provider are Hermes settings, see the docs" Type="Variable" Display="always" Required="false" Mask="false">false</Config>
  <Config Name="Your hub user" Target="TALARIA_USER" Default="" Description="Optional: the hub account of the person this agent works for, once it exists. Names that person in memory, and owns chats that are on no computer" Type="Variable" Display="advanced" Required="false" Mask="false"/>
  <Config Name="Telegram bot token" Target="TALARIA_TELEGRAM_TOKEN" Default="" Description="Optional, from @BotFather" Type="Variable" Display="advanced" Required="false" Mask="true"/>
  <Config Name="PUID" Target="PUID" Default="99" Description="Owner of the Hermes data folder" Type="Variable" Display="advanced" Required="false" Mask="false">99</Config>
  <Config Name="PGID" Target="PGID" Default="100" Description="Group of the Hermes data folder" Type="Variable" Display="advanced" Required="false" Mask="false">100</Config>
</Container>
```
<!-- /docgen:code -->
