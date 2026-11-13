# Upgrade and back up Talaria

## Know when to upgrade

Admins see a banner in the hub when a new Talaria is out: a new hub, a new agent release, or a computer that runs an
older `talaria`. **How to update** shows the steps for your install type. Members never see it. The hub reads the
public release list once a day and sends nothing about your install. To turn the check off, set `updates.check`
to false in the hub's settings (`HUB_UPDATES_CHECK=false`).

## Upgrade

- Compose: get the new files (Step 1 of [the quick start](compose.md), into the same folder; your `.env` stays),
  then `docker compose up -d --wait`. Running replies finish first: the agent gets 2 minutes to
  stop.
- Helm: `helm upgrade talaria ./talaria -n talaria --reuse-values` with the new chart. The agent updates without a
  gap (see [Helm](helm.md#zero-downtime-updates)).
- All-in-one: pull the new image and recreate the container (Unraid: **Check for Updates**, then **Apply
  Update**).

Keys and secrets never change on an upgrade.

## Back up

### From Settings (one file)

Admins can open **Settings, Backup** and click **Download backup**. Talaria packs one zip file:

| In the file | What it holds |
| ----------- | ------------- |
| `hub/hub.db` | The hub's data: accounts, paired computers, Telegram links, saved prompts and settings, with the Telegram token set in Settings. Left out when the hub keeps its data in PostgreSQL: back that up with `pg_dump`. |
| `hub/keys.yaml` | The hub's keys: the session secret (it also opens the Telegram token above), the key that signs the hub's messages to computers, and the Web Push key. |
| `agent/agent-backup.zip` | The agent's data: chats, memory, skills, agent settings and model keys. |
| `README.txt` | What is in the file and how to restore it. |

Not in it: files on your computers and the sandbox's work folder. The file holds keys: keep it somewhere
private. Packing a large agent takes a few minutes; Talaria keeps running meanwhile.

To restore it on a new Compose install:

1. Start the new install once, so it makes its volumes, then stop it with `docker compose down`.
2. Put the keys back: write the values of `hub/keys.yaml` into the `secrets` volume's `generated/` files
   (`session_secret` into `session-secret`, `device_signing_key` into `hub-signing-seed`, `vapid_private_key` into
   `vapid-key`). The file quotes each value; leave the quotes and the line break out. From the folder that holds
   the unzipped backup:

   ```bash
   docker run --rm -v talaria_secrets:/v -v "$PWD":/b:ro alpine sh -c '
   g() { sed -n "s/^$1: *//p" /b/hub/keys.yaml | tr -d "\"\n"; }
   g session_secret > /v/generated/session-secret
   g device_signing_key > /v/generated/hub-signing-seed
   g vapid_private_key > /v/generated/vapid-key'
   ```

3. Put the hub's data back: copy `hub/hub.db` over `talaria-hub.db` in the `hub-data` volume, and delete any
   `talaria-hub.db-wal` and `talaria-hub.db-shm` next to it:

   ```bash
   docker run --rm -v talaria_hub-data:/h -v "$PWD":/b:ro alpine sh -c \
     'rm -f /h/talaria-hub.db-wal /h/talaria-hub.db-shm; cp /b/hub/hub.db /h/talaria-hub.db'
   ```

4. Start it with `docker compose up -d --wait`.
5. Put the agent's data back. The import refuses to run while the agent is up, so stop it first, and run the
   import in a one-off container:

   ```bash
   docker compose stop hermes
   docker compose cp agent/agent-backup.zip hermes:/opt/data/restore.zip
   docker compose run --rm --no-deps -u 10000 --entrypoint /opt/hermes/.venv/bin/hermes hermes \
     import /opt/data/restore.zip --force
   docker compose up -d --wait
   ```

On Helm, put the keys into the `<release>-keys` Secret and the files into the PVCs the same way.

### By hand

What to keep:

| Install | Where |
| ------- | ----- |
| Compose | Volumes `talaria_hermes-data`, `talaria_hub-data`, `talaria_secrets`, `talaria_sandbox-work`, and your `.env` |
| Helm | PVCs `<release>-hermes`, `<release>-hub`, `<release>-sandbox`, and the Secret `<release>-keys` |
| All-in-one | The `/opt/data` and `/talaria` folders |

The agent's data holds the chats, memory and agent settings; the hub data holds paired computers, Telegram
links, drafts and saved prompts. Stop Talaria for a consistent copy of the SQLite files, or copy them with
`sqlite3 <file> .backup`.

A compose backup with the stack stopped:

```bash
docker compose stop
for v in hermes-data hub-data secrets sandbox-work; do
  docker run --rm -v talaria_$v:/v -v "$PWD":/b alpine tar -czf /b/talaria-$v.tgz -C /v .
done
docker compose start
```

For Helm, snapshot the PVCs with your storage's snapshots, or copy the SQLite files out of the pods, and keep the
`<release>-keys` Secret:

```bash
kubectl -n talaria get secret talaria-keys -o yaml > talaria-keys.yaml   # keep this file private
```

Keep the keys with the data. The keys sign the hub's device assertions, make Web Push work and protect the
sessions. Restoring data without the matching keys means every computer has to pair again and every browser has
to turn notifications on again.

## Restore

Compose, from the backup above, into a stopped stack:

```bash
docker compose down
for v in hermes-data hub-data secrets sandbox-work; do
  docker volume create talaria_$v
  docker run --rm -v talaria_$v:/v -v "$PWD":/b alpine sh -c "cd /v && tar -xzf /b/talaria-$v.tgz"
done
docker compose up -d --wait
```

All-in-one: stop the container, put the two folders back, start it.

## Before you upgrade

- Back up first, and read the [release notes](https://github.com/USA-RedDragon/talaria/releases): the hub's
  releases are tagged `hub/vX.Y.Z`, the agent image's `hermes/v<release>-p<n>`, the all-in-one image's
  `aio/vX.Y.Z`, and the `talaria` program's `vX.Y.Z`.
- The install files pin exact image digests. A new copy of the install files brings new pins, so the upgrade is
  the files, not `docker compose pull`. [Images](images.md) lists what is pinned now.
- Upgrade the server first, then your computers. The `talaria` program on a computer updates itself, and a
  release or two apart still works.
- The new hub migrates its database on the first start. Going back to an older hub release after that needs the
  backup you took before.

## Check it worked

```bash
docker compose ps          # every service healthy
docker compose logs hub    # no errors after start
```

Open the hub, send a message, and check in Settings, Computers that your computers are online again.
