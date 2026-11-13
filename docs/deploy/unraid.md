# Install on Unraid

Talaria on Unraid is one container from the all-in-one image ([design](all-in-one.md)). Tested on Unraid 7.4.

## Before you start

On a new Unraid server, two things are off until you turn them on:

- **The array is started.** On **Main**, assign your disks, click **Start**, and if Unraid lists them as
  Unmountable, tick **Yes, I want to do this** and click **Format**. Docker keeps its files on the array (or a
  pool), so it cannot start before.

  ![The Main page of Unraid with two array disks and a cache pool, all formatted and started](images/unraid/array-started.png)

- **Docker is enabled.** **Settings**, **Docker**, set **Enable Docker** to **Yes**, **Apply**. A **Docker** tab
  appears in the menu.

  ![Settings, Docker, with Enable Docker set to Yes before Apply](images/unraid/docker-enable.png)

## Install

1. Get the template. Unraid removed the **Template repositories** field from the Docker tab in 6.10, so there is
   no URL to add there. Instead, download the template onto the server. In the Unraid terminal (the **>_** icon at
   the top right of the Unraid page; it runs as root. Not a container's **Console**, which cannot reach
   `/boot`), run:

   ```bash
   mkdir -p /boot/config/plugins/dockerMan/templates-user
   curl -fL -o /boot/config/plugins/dockerMan/templates-user/my-Talaria.xml \
     https://raw.githubusercontent.com/USA-RedDragon/talaria/main/deploy/aio/unraid/talaria.xml
   ```

   ![The Unraid terminal after the mkdir and curl commands, with curl showing 100% of the template downloaded](images/unraid/terminal-curl.png)

   Do this once. Unraid saves your settings into the same file, so downloading it again later resets them.
2. On the **Docker** tab, click **Add Container** and pick **Talaria** under **User templates** in the
   **Template** list.

   ![Add Container with the Talaria template picked: name Talaria, repository ghcr.io/usa-reddragon/talaria-aio:latest](images/unraid/add-container.png)

3. Set **Public URL** to `http://<your Unraid address>:8080`, such as `http://192.168.1.10:8080`. Always open
   Talaria at exactly this address: at another one (the server's name instead of its IP, say), it refuses to log
   you in.
4. To look around without a model provider, set **Demo model** to `true`.

   ![The template's settings: Public URL http://10.0.2.15:8080, the two data folders under /mnt/user/appdata/talaria, and Demo model true](images/unraid/template-settings.png)

5. **Apply**. The first start downloads about 1 GB (3.3 GB unpacked) and takes a minute or two.
6. On the **Docker** tab, turn the Talaria row's **Autostart** on, so Talaria starts again after the server
   restarts. Unraid leaves it off for a new container.

   ![The Docker tab with Talaria started, up-to-date, and Autostart on](images/unraid/autostart.png)

7. Right after the start, click the Talaria icon and pick **WebUI** (or open your Public URL). The setup page
   makes the first account, an admin: the first person who opens it gets that account, so do it before the port
   is reachable from outside your network. Add other people under Settings, Users.

   ![The Talaria icon's menu on the Docker tab: WebUI, Console, Stop, Pause, Restart, Logs, Edit, Remove](images/unraid/container-menu.png)

   ![The Talaria setup page with the user name alice and a display name filled in](images/unraid/setup.png)

8. Connect a model. This step is required: until you do it, the chat area says Talaria has no model yet.
   Talaria includes its own agent, the part that talks to models, so you need nothing else.

   As the admin, click **Connect a model** in the chat area (it shows until a model is connected; later it is
   in Settings, Models) and pick where the model runs: a server of your own (Ollama, LM Studio, vLLM, llama.cpp),
   a provider with an API key, or a subscription sign-in. Talaria checks the address or key through the agent, lets
   you pick the model new chats start with, and keeps nothing itself: the agent stores the provider and key.
   [Choosing models and providers](models.md) walks through each kind.

   Or, in the Unraid terminal (the **>_** icon at the top right), run
   the agent's own setup command in the container:

   ```bash
   docker exec -it -u hermes Talaria hermes model
   ```

   You can also open the container's **Console** (its menu on the **Docker** tab); that shell is root, so run
   `runuser -u hermes -- hermes model` there.

   You will see a list of providers; move with the arrow keys. It opens at the provider in use, if any. Pick one
   and give it what it asks for (an API key, a sign-in, or an address). With the demo model on, `talaria-demo` is
   near the end of the list (Enter, Enter for its one model, Enter for the reasoning effort). For Ollama, LM Studio
   or vLLM, pick **Custom endpoint**, near the end, right after **Local models** (see
   [Ollama on the same server](#ollama-on-the-same-server)). It asks for a display name at the end: it becomes
   the group your models show under in the chat's model menu.

   ![hermes model in the Unraid terminal, with talaria-demo selected near the end of the provider list](images/unraid/hermes-model-demo.png)

   Then open a new chat (or reload the page) and pick the model in the chat's model menu. No restart is needed,
   and models you pull into Ollama later appear on their own. Details: [Choosing models and providers](models.md).

   ![A Talaria chat on talaria-demo: run: uname -sr && whoami && pwd ran a command in the sandbox and shows Linux 6.18.60-Unraid](images/unraid/chat-demo.png)

Data is in `/mnt/user/appdata/talaria`: `hermes` (chats, memory) and `talaria` (hub database, keys, the
sandbox's files). Back up both.

The time zone follows Unraid's own (**Settings**, **Date and Time**).

Stopping, restarting or updating the container interrupts a reply that is running; the chat says so, and you
send the message again. Talaria stops in a few seconds, well within Unraid's **Docker Stop Timeout** (Settings,
Docker; 10 seconds by default).

## Update and change settings

The template pulls `talaria-aio:latest`, so Unraid's own update check covers it:

- **Update:** on the **Docker** tab, click **Check for Updates** at the bottom. When a new image is out, the
  Talaria row's **Version** column says **apply update**. Click it, then **Yes, update it!**. Unraid pulls the new
  image and recreates the container with the same settings; chats, memory, accounts and the model setup stay,
  because they live in `/mnt/user/appdata/talaria`.

  ![The Docker tab after Check for Updates: the Talaria row says apply update, ollama says up-to-date](images/unraid/update-ready.png)

  ![Unraid asks Are you sure? Update container: Talaria, with Yes, update it! and Cancel](images/unraid/apply-update.png)

- **Change a setting** (Public URL, demo model, Telegram token): click the Talaria icon on the **Docker** tab,
  pick **Edit**, change it, **Apply**. Unraid recreates the container; your data stays.

  ![Edit on the Talaria container, with Demo model changed to false](images/unraid/edit.png)

Both need the container to be made from the template. A container started by hand with `docker run` shows as
**3rd Party** on the Docker tab, with no **WebUI**, **Edit** or update check in its menu. To move it onto the
template:

![A container started by hand: Version and Autostart say 3rd Party, and its menu has only Console, Stop, Pause, Restart, Logs and Remove](images/unraid/third-party.png)

1. Note where its data lives, in the Unraid terminal:

   ```bash
   docker inspect Talaria --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'
   ```

   ![docker inspect in the Unraid terminal listing /mnt/user/appdata/talaria/talaria -> /talaria and /mnt/user/appdata/talaria/hermes -> /opt/data](images/unraid/inspect-mounts.png)

2. Get the template (step 1 of the install above).
3. Remove the old container: its icon, **Remove**. This removes the container only; the folders from step 1 stay.
4. **Add Container**, pick **Talaria** under **User templates**, set **Hermes data** and **Talaria data** to the
   folders from step 1 (the ones mounted at `/opt/data` and `/talaria`), the same **Public URL**, and **Apply**.
   Turn **Autostart** on.

Chats, accounts and the model setup come back as they were.

## Ollama on the same server

Ollama is its own container; The agent reaches it by the server's address.

1. Install Ollama. On the **Docker** tab, **Add Container**, with **Name** `ollama` (the commands below use it)
   and **Repository** `ollama/ollama`. Then, with **Add another Path, Port, Variable, Label or Device**, add:

   - a **Port**: container port and host port `11434`;
   - a **Path**: container path `/root/.ollama`, host path `/mnt/user/appdata/ollama` (the models);
   - a **Variable** `OLLAMA_CONTEXT_LENGTH` = `65536` (Ollama's setting: the window it serves to clients that
     do not ask for one; its default is a few thousand tokens, and the agent's prompts get cut);
   - without a GPU, a **Variable** `OLLAMA_KV_CACHE_TYPE` = `q8_0`. It halves the memory the window takes: with
     the default, a 3B model with a 64K window needed about 9 GB and was killed on a 10 GB server.

   **Apply**; the image is about 3 GB. (With the Community Applications plugin, **Apps** has an Ollama template
   too; the **Apps** tab offers to install the plugin the first time you open it.)

   ![The ollama container's settings: repository ollama/ollama, port 11434, models in /mnt/user/appdata/ollama, OLLAMA_CONTEXT_LENGTH 65536 and OLLAMA_KV_CACHE_TYPE q8_0](images/unraid/ollama-container.png)

2. Download a model that handles tool calls and a 64K window, in the Unraid terminal. `llama3.1:8b` suits a GPU
   or 16 GB to spare; `llama3.2:3b` (2 GB) runs on a CPU, slowly.

   ```bash
   docker exec -it ollama ollama pull llama3.2:3b
   ```

   ![ollama pull llama3.2:3b in the Unraid terminal, then ollama show with a 131072 context length and the tools capability](images/unraid/ollama-pull.png)

3. Point the agent at it: `docker exec -it -u hermes Talaria hermes model`, **Custom endpoint**, API base URL
   `http://<your Unraid address>:11434/v1` (such as `http://192.168.1.10:11434/v1`), then: no API key, Enter for
   auto-detect, Enter to use the model it found, context length `65536` (the window you gave Ollama; left empty,
   the agent takes the model's own maximum, 131072 for llama3.2), a display name such as `Ollama`, and Enter for the
   reasoning effort.

   ![hermes model with the custom endpoint http://10.0.2.15:11434/v1: Verified endpoint, Detected model llama3.2:3b, context length 65536, display name Ollama, Default model set to llama3.2:3b](images/unraid/hermes-model-ollama.png)

![A Talaria chat on llama3.2:3b: it ran uname -r (shown as Ran a command) and answered The kernel version is 6.18.60-Unraid](images/unraid/chat-ollama.png)

On 4 CPU cores, that first answer took about ten minutes (the agent sends about 13K tokens of instructions and
tools). Use a GPU or a provider for daily use.

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
  <Overview>Talaria: a web app and Telegram bot for your own AI agent, with a sandbox for the agent's commands and your own computers paired in. One container: hub, agent and sandbox. Open the web UI right after the first start: the first visitor makes the admin account.</Overview>
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
  <Config Name="Demo model" Target="TALARIA_DEMO" Default="false" Description="true: a stand-in model that needs no API key, to try Talaria (then pick talaria-demo with: docker exec -it -u hermes Talaria hermes model). Connect a real model in Talaria with Connect a model, see the docs" Type="Variable" Display="always" Required="false" Mask="false">false</Config>
  <Config Name="Web search" Target="TALARIA_SEARXNG" Default="true" Description="true: the agent searches the web through its own SearXNG inside this container (no API key, not reachable from outside). false: off, or use another search provider set up in the agent" Type="Variable" Display="advanced" Required="false" Mask="false">true</Config>
  <Config Name="Your hub user" Target="TALARIA_USER" Default="" Description="Optional: the hub account of the person this agent works for, once it exists. Names that person in memory, and owns chats that are on no computer" Type="Variable" Display="advanced" Required="false" Mask="false"/>
  <Config Name="Telegram bot token" Target="TALARIA_TELEGRAM_TOKEN" Default="" Description="Optional, from @BotFather" Type="Variable" Display="advanced" Required="false" Mask="true"/>
  <Config Name="PUID" Target="PUID" Default="99" Description="Owner of the agent's data folder" Type="Variable" Display="advanced" Required="false" Mask="false">99</Config>
  <Config Name="PGID" Target="PGID" Default="100" Description="Group of the agent's data folder" Type="Variable" Display="advanced" Required="false" Mask="false">100</Config>
  <Config Name="Install type" Target="TALARIA_INSTALL" Default="unraid" Description="Tells Talaria it runs on Unraid, for the commands it shows. Leave it as is" Type="Variable" Display="always-hide" Required="false" Mask="false">unraid</Config>
</Container>
```
<!-- /docgen:code -->
