# Upgrade and back up Talaria

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
