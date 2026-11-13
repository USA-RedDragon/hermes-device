# Telegram

Talaria can run a Telegram bot, so you can chat with your agent, approve its actions and get notices from your phone. It is optional.

You need:

- A Telegram account.
- A bot token from [@BotFather](https://t.me/BotFather).
- HTTPS for your hub, if you want the links in Telegram to open your hub. Chatting works without it.

## Make a bot

1. In Telegram, open a chat with @BotFather and send `/newbot`.
2. Pick a name and a user name ending in `bot`, for example `alice_talaria_bot`.
3. BotFather replies with a token that looks like `123456789:AA...`. Keep it secret: whoever has it controls the bot.

## Turn it on

**In the hub (any install):** sign in as an admin, open **Settings, Telegram**, paste the token, click **Test**,
then **Save and start**. The bot starts at once, without a restart. The hub keeps the token in its own database,
sealed with a key from its secrets; nobody can read it back in Settings. **Stop** turns the bot off again.

A token set by the install (below) wins over one set in Settings, and Settings then shows the bot read-only.

**Compose:** put the token in `.env` and apply:

```sh
TALARIA_TELEGRAM_TOKEN=123456789:AA...
```

```sh
docker compose up -d
```

**Helm:**

```sh
kubectl -n talaria create secret generic talaria-telegram --from-literal=token=123456789:AA...
helm upgrade talaria ./talaria -n talaria --reuse-values --set telegram.existingSecret=talaria-telegram
```

**All-in-one:** set `TALARIA_TELEGRAM_TOKEN` on the container.

## Link your account

1. Open your bot in Telegram and send `/link`.
2. The bot sends a link. Open it, log in to the hub if asked, and confirm. Only that confirmation links the accounts. A link works once and expires after 10 minutes.

   ![The Link Telegram page: it asks whether to link the Telegram account that sent /link with the hub user, and has a Link account button](images/telegram-link.png)

   ![After linking: the page says the Telegram account is now linked](images/telegram-linked.png)
3. Send the bot a message. It goes to your agent, and the reply streams back.

In the hub, Settings, Telegram shows whether your account is linked and how replies show their steps.

![Settings, Telegram: the account is not linked yet, and replies show their steps collapsed](images/settings-telegram.png)

Each person on your hub links their own Telegram account. People who are not linked get nothing from the bot.

## What you can do

| Command | What it does |
| ------- | ------------ |
| `/new` | Start a new chat |
| `/stop` | Stop the running turn |
| `/model` | Show the chat's model, with buttons to switch |
| `/status` | Show the model and how full the context is |
| `/prompts` | Your saved prompts, one tap to send |
| `/sessions`, `/backends` | List chats and backends |
| `/devices`, `/where`, `/move` | List your computers, see where this chat runs, move it |
| `/link`, `/unlink` | Link or unlink your account |
| `/help` | The command list |

When the agent wants to run something that needs your approval, the bot shows the command with buttons: Once, This chat, Always and Deny. You can also send photos, files, voice notes and PDFs to the bot, and they go to the chat as attachments.

## A topic for each chat

You can have one topic per chat, so replies of different chats never mix. Either:

- Turn on topic mode for the bot in the @BotFather Mini App. Then your private chat with the bot gets a topic per chat. Or
- Make a private group, turn on Topics in the group settings, add the bot, make it an admin that can manage topics and delete messages, and send `/link` in the group. Every member sees every topic, so keep the group to yourself.

## Things to know

- Only one process can read a bot's messages at a time. Do not run the same token in two places.
- Telegram limits how often a bot can send. Long turns update one status message in place instead of sending many.
- The bot answers files larger than 20 MB with a note, because Telegram does not let bots download them.
- For the bot's links to open the right address, set the public URL to the address you use in the browser.

[Notifications](notifications.md) can also reach you through Telegram.
