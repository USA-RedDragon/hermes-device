# syntax=docker/dockerfile:1
# Talaria all-in-one: the hub, Hermes and the sandbox in one container, for Unraid, TrueNAS and
# anyone who wants a single container. Build from the repository root:
#   docker build -f deploy/aio/Dockerfile -t talaria-aio deploy
# Design and trade-offs: https://usa-reddragon.github.io/talaria/deploy/all-in-one.html
# Built on Hermes Agent by Nous Research (MIT). Not affiliated with or endorsed by Nous Research.

ARG HERMES_IMAGE=ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p38@sha256:d3633b3a9dfd1a952be13eb77a8700b297efb918f80be63dcf0594a0a780d5d4
ARG HUB_IMAGE=ghcr.io/usa-reddragon/talaria-hub:v0.26.78@sha256:1cae8ef5c8ac4139250912b3c7294e2e7d132f71f0850b38b10295673ba84059
ARG DEVICE_IMAGE=ghcr.io/usa-reddragon/talaria:v0.26.0@sha256:c882110cb0ca0c44abac701aef5413655b2d7faebbcb871160d14dbd17c6e22c
# SearXNG, the agent's web search: its code and package versions come from this image
ARG SEARXNG_IMAGE=docker.io/searxng/searxng:2026.11.13-8a53a6aa5@sha256:82b222dd1854c43241874874fbc985006556230804dc819ef2a639eeb574d0e2
ARG CADDY_IMAGE=docker.io/library/caddy:2.10-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d

FROM ${HUB_IMAGE} AS hub
FROM ${DEVICE_IMAGE} AS device
FROM ${CADDY_IMAGE} AS caddy
FROM ${SEARXNG_IMAGE} AS searxng-upstream

# SearXNG's Python packages, the exact set and versions of the upstream image (which is built on musl), installed
# for this image's Python into a venv of their own
FROM ${HERMES_IMAGE} AS searxng
USER root
COPY --from=searxng-upstream /usr/local/searxng/.venv/lib/ /tmp/upstream-lib/
RUN set -eu; \
    find /tmp/upstream-lib -maxdepth 3 -name '*.dist-info' -printf '%f\n' \
      | sed -E 's/\.dist-info$//; s/-([^-]+)$/==\1/' | sort > /tmp/requirements.txt; \
    python3 -m venv /opt/searxng/.venv; \
    /opt/searxng/.venv/bin/pip install --no-cache-dir --disable-pip-version-check --no-deps --only-binary=:all: \
      -r /tmp/requirements.txt; \
    /opt/searxng/.venv/bin/pip uninstall -y -q pip; \
    find /opt/searxng/.venv/lib -type f -name '*.so*' -exec strip --strip-unneeded {} +; \
    find /opt/searxng/.venv -type d -name __pycache__ -prune -exec rm -rf {} +; \
    python3 -m compileall -q -j 0 --invalidation-mode=unchecked-hash /opt/searxng/.venv/lib

FROM ${HERMES_IMAGE}
# poppler-utils reads attached PDFs for the hub; agent is the sandbox's user
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 2568 agent && useradd -u 2568 -g 2568 -d /talaria/work -s /bin/bash -M agent \
 && groupadd -g 977 searxng && useradd -u 977 -g 977 -d /opt/searxng -s /usr/sbin/nologin -M searxng
# The three binaries are static
COPY --from=hub /talaria-hub /usr/local/bin/talaria-hub
COPY --from=device /talaria /usr/local/bin/talaria
COPY --from=caddy /usr/bin/caddy /usr/local/bin/caddy
# SearXNG: its venv and code, and Talaria's settings for it (deploy/searxng)
COPY --from=searxng /opt/searxng/.venv /opt/searxng/.venv
COPY --from=searxng-upstream /usr/local/searxng/searx /opt/searxng/searx
COPY searxng/settings.yml /etc/searxng/settings.yml
COPY compose/files/init.sh compose/files/hermes-init.py compose/files/demo-llm.py /usr/share/talaria/
COPY aio/rootfs/ /
# Licenses and the "built on Hermes Agent" notice
COPY aio/licenses/ /usr/share/doc/talaria-aio/
ENV TALARIA_PORT=8080 \
    TALARIA_PLUGIN_SRC=/run/talaria/plugin-src
VOLUME ["/opt/data", "/talaria"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=3m \
  CMD curl -fsS -o /dev/null http://127.0.0.1:9120/api/health && curl -fsS -o /dev/null http://127.0.0.1:8081/healthz || exit 1
# The gateway is the container's main program; the Talaria services run under s6 next to it
CMD ["gateway", "run"]
