FROM scratch

# this pulls directly from the upstream image, which already has ca-certificates:
COPY --from=alpine:latest /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/hermes-device /
USER 65534:65534
EXPOSE 9130
ENTRYPOINT ["/hermes-device"]
