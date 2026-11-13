# Pick an install

Talaria is a web app and Telegram bot for your own AI agent, built on [Hermes Agent](https://github.com/NousResearch/hermes-agent). You run it on your own server. The agent works in a sandbox on the server, or on your own computers once you pair them.

Talaria includes its own agent, the part that talks to models, so you need nothing else. You connect models once, from Talaria's own page (Connect a model, for admins) or with the agent's own setup command (`hermes model`, run inside Talaria's container), and then pick them in the chat's model menu. Every install guide has that as a numbered step right after you make your account.

A Talaria install has four parts, and every install path starts all of them:

- **The hub:** the web app, the Telegram bot and the place your computers connect to.
- **The agent:** the part that talks to models and does the work, with the Talaria plugin.
- **The sandbox:** a small container where the agent runs commands for chats that are not on one of your computers.
- **Accounts:** the hub's own. The first person who opens Talaria makes the admin account, and adds the others.

Every key and secret is made for you on the first start. You do not write any config file to try it.

## Pick a path

| You have | Use | Page |
| -------- | --- | ---- |
| A Linux machine with Docker | Docker Compose | [Docker Compose](compose.md) |
| A Kubernetes cluster | The Helm chart | [Kubernetes](helm.md) |
| Unraid | The all-in-one container template | [Unraid](unraid.md) |
| TrueNAS SCALE 24.10 or later | The all-in-one container as a custom app | [TrueNAS](truenas.md) |
| Any Docker host, only to look around | The all-in-one container | [The all-in-one image](all-in-one.md) |

If you are not sure, use Docker Compose. It is the path with the most testing.

## What you will see

The setup page that makes the first account, the first chat with the demo model, and an approval, which the agent asks for before it runs something risky:

![The setup page: your name, a display name and a password make the first account, an admin](images/setup.png)

![A first chat: the demo model answers "This is the Talaria demo model", and the line under the title shows the model and the context size](images/chat-reply.png)

![An approval: Talaria asks to run a command, with Allow, Deny, Allow in this chat and Always allow](images/approval.png)

## What you need

- 2 GB of free RAM and 10 GB of free disk, for one person, with a hosted model. The agent image alone is about 1.1 GB to download.
- A model to talk to. To look around first, the demo model needs nothing and answers with a canned reply. For real work, see [Choosing models and providers](models.md).
- For other people to use it: a name for the server and HTTPS. See [Reach it from other computers](compose.md#reach-it-from-other-computers).

## The first fifteen minutes

1. Start Talaria on the machine itself ([Docker Compose](compose.md), steps 1 to 3). It takes a few minutes, mostly the download.
2. Open the address it prints right away and make your account on the setup page: the first visitor becomes the admin.
3. Connect a model: click **Connect a model** in the chat area (or run `hermes model` in Talaria's container; the exact command is a numbered step in your install guide, and [Choosing models and providers](models.md) has the details), or use the demo model to look around. Then pick it in the chat's model menu.
4. Send a message. Send `run: uname -a` to see the agent run a command in the sandbox.
5. Pair your own computer so the agent can work on your files: [Add a computer](../start/add-a-computer.md).
6. Optional: [Telegram](telegram.md), [Notifications](notifications.md), [Memory](memory.md).

## Where to look when something is wrong

[Troubleshooting](troubleshooting.md) lists the problems people hit first, with what to check.
