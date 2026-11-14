# Update, move or remove a computer

## Update

The daemon updates itself. When the service runs `talaria run` and `update.auto` is on (the default), it looks for a new stable release shortly after it starts and then every `update.interval` (6 hours by default). It installs a release with the same checks as the `update` command, waits until no command, browser operation or chat is running on this computer, and restarts. It never installs a pre-release or an older release.

To update by hand:

```sh
talaria update            # the latest release
talaria update 0.25.2     # a specific release
talaria update --check    # download and verify, install nothing
```

`update` checks the archive against the release's SHA-256 file, verifies its build record (a Sigstore-signed attestation from this repository's release workflow), runs the new program once to check it, replaces `~/.talaria/bin/talaria` and restarts the service. It refuses a downgrade without `--force`, and a release whose attestation does not verify. [Releases and verification](../releases.md) has the details.

On an offline computer, or when you update by your own means, turn the self-update off:

```yaml
update:
  auto: false
```

Commands you run (`chat`, `acp`, `setup`, `login`) print one line on stderr once a day when a newer release exists. Set `TALARIA_NO_UPDATE_NOTICE=1` to hide it.

Keep the program and your server close in version. The agent plugin and the program negotiate their protocol, so a release or two apart still works, but upgrade the server first, then your computers.

## Change the settings of the service

The unit that setup writes has no memory or task limit. Releases before the one that dropped them wrote `MemoryMax=2G` and `TasksMax=512`; run `talaria setup` again to replace such a unit with the current one. To limit the service, put `MemoryMax` in a drop-in file (`systemctl --user edit talaria`). `talaria update` does not change the unit. Run `talaria setup` again to get the unit of a newer release. A unit you edited is saved as a backup first, and drop-in files in `talaria.service.d` are left alone.

## Move to another hub

Run setup against the new address, with a new config:

```sh
talaria setup --overwrite-config https://new-hub.example.com
```

The old config is saved first. Then remove the old computer on the old hub (Settings, Computers).

## Remove a computer

1. On the hub, go to Settings, then Computers, and remove the computer. Its token stops working at once.
2. On the computer:

   ```sh
   talaria setup --uninstall
   ```

   This stops and removes the service, removes the native messaging host and the stored token, puts back any config or service file that setup replaced, removes the lines setup added to your shell files (the backups stay), removes talaria's cache and, on macOS, the service's log `~/Library/Logs/talaria.log`, and removes the program. A config file you changed since setup is kept, and so are config backups.
3. Remove the Talaria extension in Firefox (`about:addons`).

If the uninstall cannot stop the service, it keeps the program, the config and the token, so you can stop the service and run it again.
