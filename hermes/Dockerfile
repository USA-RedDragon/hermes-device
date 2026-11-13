# syntax=docker/dockerfile:1

# The upstream image. Both stages and HERMES_BASE_IMAGE use it, so one Renovate bump moves all three.
ARG HERMES_BASE=docker.io/nousresearch/hermes-agent:v0.23.1@sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08

# Patches are text, so they apply once on the build platform; the image for each
# platform only gets the patched files copied on top of the upstream image.
FROM --platform=$BUILDPLATFORM ${HERMES_BASE} AS patch
COPY scripts/apply-patches.sh /usr/local/bin/apply-patches
COPY patches/ /patches/
RUN apply-patches --image /opt/hermes /patches /out

FROM ${HERMES_BASE}
ARG HERMES_BASE
# The upstream image this one patches, with its digest: clients that need the matching upstream build read it
ENV HERMES_BASE_IMAGE=${HERMES_BASE}
COPY --from=patch /out/ /opt/hermes/
# Upstream's MIT license and the "built on Hermes Agent" notice
COPY LICENSE NOTICE /usr/share/doc/talaria-hermes/
