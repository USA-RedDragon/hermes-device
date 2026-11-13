# Upgrade Hermes (for maintainers)

The Talaria hub and the talaria device plugin depend on Hermes internals: gateway protocol lines, client method names and the Python functions the plugin wraps. A Hermes release can break them. This page tells how to know before the upgrade, and in which order to roll it out.

## How the check works

1. Renovate opens a PR in talaria-hub and in talaria for each new Hermes release. Each PR changes `HERMES_VERSION` in `.github/workflows/hermes-compat.yaml`. These PRs open as soon as the release is out and never merge on their own.
2. The workflow "Hermes compatibility" runs on that PR:
   - talaria-hub, check "Hermes contract": clones the Hermes source at that tag and runs `TestContractAgainstSource` and `TestClientMethodsAgainstSource` with `HERMES_AGENT_SRC` and `HERMES_AGENT_VERSION` set.
   - talaria, check "Hermes plugin": runs `scripts/test-plugin.sh` (all plugin tests, contract tests included) inside `nousresearch/hermes-agent` at that tag.
3. Renovate opens the upgrade PR in home-cluster-flux 24 hours after the release (its minimum release age). That one PR changes every Hermes image in `apps/hermes` and `apps/hermes-kittykat`, and its body links to the two PRs above. It never merges on its own.

To check any release by hand, run the workflow from the Actions tab with the `version` input, or run the tests locally:

```sh
git clone --depth 1 --branch v0.23.1 https://github.com/NousResearch/hermes-agent <dir>
HERMES_AGENT_SRC=<dir> HERMES_AGENT_VERSION=v0.23.1 go test -count=1 -v -run 'AgainstSource$' ./internal/gateway/ ./internal/device/
```

## Before you merge the Flux PR

1. Both checks are green. A red check names the source lines or client methods that changed. Do not merge.
2. Read the Hermes release notes for changes to the gateway WebSocket protocol, the Webapp login, ACP, plugins and the file tools. The tests cover only the lines they pin.
3. If a check is red, fix the hub or the plugin first: update the pinned lines and the code that depends on them, set `gateway.HermesVersion` in the hub and the Hermes version notes in talaria to the new release, and release both. Then rebase the Renovate compatibility PRs so the checks run again.

## Rollout order

1. If the hub or the plugin changed, release them and roll them out first: talaria-hub in `apps/hermes-hub`, and the talaria image (`ghcr.io/usa-reddragon/talaria`; `hermes-device` up to v0.25.4) used by the `device-plugin` init container and the sandboxes. A version that still works with the old Hermes can go out ahead of it. A version that needs the new Hermes goes in the same Flux commit as the Hermes upgrade.
2. Merge the Hermes PR in home-cluster-flux. Watch both Hermes pods restart and become ready, and check that the hub can still open a chat in each.
3. Update Hermes on each computer that runs `talaria chat`. It refuses a local Hermes that differs from the server's unless given `--allow-version-mismatch`.
4. Merge the compatibility PRs in talaria-hub and talaria, so `HERMES_VERSION` records the release now in use.
