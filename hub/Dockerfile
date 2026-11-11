FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

# poppler-utils reads attached PDFs; the URW fonts stand in for the 14
# standard PDF fonts, which PDFs often do not embed.
RUN apk add --no-cache poppler-utils font-urw-base35

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/talaria-hub /
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/talaria-hub"]
