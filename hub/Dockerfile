FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/hermes-hub /hermes-hub
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/hermes-hub"]
