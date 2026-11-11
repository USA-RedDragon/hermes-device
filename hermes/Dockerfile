# syntax=docker/dockerfile:1

# Patches are text, so they apply once on the build platform; the image for each
# platform only gets the patched files copied on top of the upstream image.
FROM --platform=$BUILDPLATFORM docker.io/nousresearch/hermes-agent:v0.23.1@sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08 AS patch
COPY scripts/apply-patches.sh /usr/local/bin/apply-patches
COPY patches/ /patches/
RUN apply-patches --image /opt/hermes /patches /out

FROM docker.io/nousresearch/hermes-agent:v0.23.1@sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08
COPY --from=patch /out/ /opt/hermes/
