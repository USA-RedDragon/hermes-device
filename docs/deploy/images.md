# Container images

Every install uses these images, pinned by digest. The compose file, the Helm chart and the all-in-one
Dockerfile pin the same versions (`deploy/hack/check-sync.sh` checks it in CI, and `deploy/hack/pin.sh` moves a
pin in all of them), and the chart's `appVersion` is the hub release it pins.

<!-- docgen:output exec="grep -oE '(ghcr\.io|docker\.io)/[a-z0-9./_-]+:[A-Za-z0-9._-]+' deploy/compose/compose.yaml | sort -u" -->
```text
docker.io/library/caddy:2.10-alpine
docker.io/ollama/ollama:0.40.16
docker.io/pgvector/pgvector:0.8.1-pg17
docker.io/searxng/searxng:2026.11.13-8a53a6aa5
ghcr.io/huggingface/text-embeddings-inference:cpu-1.9.4
ghcr.io/plastic-labs/honcho:v3.6.0
ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p41
ghcr.io/usa-reddragon/talaria-hub:v0.26.86
ghcr.io/usa-reddragon/talaria-sandbox:v0.26.0
ghcr.io/usa-reddragon/talaria:v0.26.0
```
<!-- /docgen:output -->

| Image | What it is | Public |
| ----- | ---------- | ------ |
| `ghcr.io/usa-reddragon/talaria-hub` | The hub | Yes |
| `ghcr.io/usa-reddragon/talaria-hermes` | Talaria Hermes: Hermes Agent with the Talaria patches | Yes |
| `ghcr.io/usa-reddragon/talaria` | The `talaria` binary; the chart and compose copy the device plugin out of it | Yes |
| `ghcr.io/usa-reddragon/talaria-sandbox` | The sandbox | Yes |
| `docker.io/library/caddy` | The web front of compose (HTTPS, and holding requests while the agent starts); the all-in-one image carries its binary. The Helm chart does not use it | Yes |
| `docker.io/searxng/searxng` | SearXNG, the agent's web search ([Web search](reference.md#web-search)) | Yes |
| `ghcr.io/usa-reddragon/talaria-aio` | The all-in-one image (Unraid, TrueNAS, `docker run`) | Yes |

Optional parts (compose profiles): `docker.io/ollama/ollama`, `ghcr.io/plastic-labs/honcho`,
`docker.io/pgvector/pgvector`, `ghcr.io/huggingface/text-embeddings-inference`.

Every image is public; no install needs a registry login.

Talaria Hermes and the all-in-one image are built on Hermes Agent by Nous Research (MIT). Not affiliated with or
endorsed by Nous Research. Both carry the licenses and this notice in `/usr/share/doc/talaria-hermes/` (and
`/usr/share/doc/talaria-aio/` in the all-in-one image).

Both are signed keyless with cosign. Verify with:

```bash
cosign verify ghcr.io/usa-reddragon/talaria-hermes:<tag> \
  --certificate-identity-regexp '^https://github.com/USA-RedDragon/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify ghcr.io/usa-reddragon/talaria-aio:<tag> \
  --certificate-identity-regexp '^https://github.com/USA-RedDragon/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Another Hermes image

Point `TALARIA_HERMES_IMAGE` (compose) or `images.hermes` (Helm) at another image, such as one you built, a
mirror, or stock Hermes (`docker.io/nousresearch/hermes-agent:v0.23.1`). With stock Hermes, Talaria works with
fewer features: no zero-downtime updates, no per-device session lists, and other fixes are missing. Set
`hermes.rolling.enabled=false` in Helm with stock Hermes. This combination is not tested.
