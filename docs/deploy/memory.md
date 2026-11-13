# Memory

The agent keeps three kinds of memory, and the hub shows them on the Memory page.

| Kind | What it is | Needs |
| ---- | ---------- | ----- |
| Notes | The agent's own notes (`MEMORY.md`) | Nothing, on by default |
| About you | The agent's profile of you (`USER.md`) | Nothing, on by default |
| Learned | Facts that [Honcho](https://github.com/plastic-labs/honcho) drew from your chats | Honcho, optional |

Notes and About you are plain text files in the agent's data folder. You can read, edit and delete entries on the Memory page. A meter shows how much of the size limit you used. A chat keeps the memory it loaded when it started, so changes apply to new chats.

Everyone who shares one agent shares one memory. An install here has one agent, so on a hub with several people, they all see and change the same notes. Give each person their own agent if that matters.

## Honcho (optional)

Honcho reads your chats in the background, writes down facts about you and the work, and lets the agent look them up later. It is the heavy optional part: it runs its own API, a worker, a database and an embedding model.

Plan for these resources:

- A small local embedding model, which the compose profile starts: about 1 GB of RAM.
- The API, worker and database: roughly another 1.5 to 2 GB.
- Time: the first facts show up after a few chats, not at once.

A bigger local embedding server needs far more RAM (about 9 GB on a CPU). A hosted embeddings API costs a little and uses no RAM.

### Compose

In `.env`, add the profiles and the Honcho address:

```sh
COMPOSE_PROFILES=honcho,honcho-embeddings
TALARIA_HONCHO_URL=http://honcho:8000
```

Then `docker compose up -d --wait`. The compose file validates with these profiles, but the Honcho services were not started end to end in the install tests, so expect to read their logs (`docker compose logs honcho`) if something is off. Honcho uses the same model as your chats for what it learns. Set `TALARIA_HONCHO_MODEL` to use another one.

To use a hosted embeddings API instead of the small local model, leave out `honcho-embeddings` and set:

```sh
TALARIA_EMBEDDINGS_BASE_URL=https://api.openai.com/v1
TALARIA_EMBEDDINGS_MODEL=text-embedding-3-small
TALARIA_EMBEDDINGS_API_KEY=sk-...
TALARIA_EMBEDDINGS_DIMENSIONS=1536
```

The dimensions must match the model, and cannot change once Honcho stored facts. Picking a different model later means starting Honcho's data over.

### Helm and the all-in-one image

The chart and the all-in-one image do not include Honcho. Run Honcho yourself, and point the agent and the hub at it. This is untested here.

## The Learned tab

On the Memory page, the Learned tab lists facts Honcho drew from your chats. You can search them, filter them, delete one or several, and open the chat a fact came from. It first shows only facts Honcho deduced over time; "Show all" brings back the facts from single messages. Honcho often stores a fact twice, once for each view, and the page shows it once, marked "Both views".

## Project notes

When the project-memory plugin is present, the Notes tab also has a picker for per-project notes, keyed by the project's git remote. This plugin is not part of the install files here, so the picker stays hidden.
