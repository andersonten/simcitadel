# The builder tag must satisfy the go directive in go.mod.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /event-capture ./cmd/event-capture

FROM scratch
COPY --from=build /event-capture /event-capture
# The image holds no shell, so the health check re-executes the service binary
# with -health. That invocation probes the local endpoint and exits 0 or 1.
HEALTHCHECK --interval=5s --timeout=5s --retries=5 --start-period=5s \
  CMD ["/event-capture", "-health"]
ENTRYPOINT ["/event-capture"]
