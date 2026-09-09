# The release image. GoReleaser builds the binary and passes it in the build
# context, one directory per platform. The compose stack builds from source
# instead, with cmd/event-capture/Dockerfile.
FROM scratch

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/event-capture /event-capture

# The image holds no shell, so the health check re-executes the service binary
# with -health. That invocation probes the local endpoint and exits 0 or 1.
HEALTHCHECK --interval=5s --timeout=5s --retries=5 --start-period=5s \
  CMD ["/event-capture", "-health"]

ENTRYPOINT ["/event-capture"]
