# syntax=docker/dockerfile:1
# Talaria all-in-one: the hub, Hermes and the sandbox in one container, for Unraid, TrueNAS and
# anyone who wants a single container. Build from the repository root:
#   docker build -f deploy/aio/Dockerfile -t talaria-aio deploy
# Design and trade-offs: https://usa-reddragon.github.io/talaria/deploy/all-in-one.html
# Built on Hermes Agent by Nous Research (MIT). Not affiliated with or endorsed by Nous Research.

ARG HERMES_IMAGE=ghcr.io/usa-reddragon/talaria-hermes:v0.23.1-p30@sha256:4a5d4e2affd196ac92037e2d3e1e3fefbb32ab74f5467f5b324d0e401187cc0a
ARG HUB_IMAGE=ghcr.io/usa-reddragon/talaria-hub:v0.26.54@sha256:832bd2db5b0aa144c28687990aca006cfda1a89268a73c1179b68e6fb4ea5504
ARG DEVICE_IMAGE=ghcr.io/usa-reddragon/hermes-device:v0.25.3@sha256:c19e36561ac6b34798eea6464dfbf5f974b44102d30c1e1f789abc88c3a7eeb0
ARG CADDY_IMAGE=docker.io/library/caddy:2.10-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d

FROM ${HUB_IMAGE} AS hub
FROM ${DEVICE_IMAGE} AS device
FROM ${CADDY_IMAGE} AS caddy

FROM ${HERMES_IMAGE}
# poppler-utils reads attached PDFs for the hub; agent is the sandbox's user
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 2568 agent && useradd -u 2568 -g 2568 -d /talaria/work -s /bin/bash -M agent
# All three are static binaries
COPY --from=hub /talaria-hub /usr/local/bin/talaria-hub
COPY --from=device /hermes-device /usr/local/bin/hermes-device
COPY --from=caddy /usr/bin/caddy /usr/local/bin/caddy
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
