# The builder tag must satisfy the go directive in go.mod.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /example-app ./cmd/example-app

FROM scratch
# The app calls WireMock over HTTP and Postgres over TCP, and neither needs a
# certificate store in a local stack. A consuming repository that points this at a
# TLS endpoint has to add ca-certificates.
COPY --from=build /example-app /example-app
# The image holds no shell, so the health check re-executes the service binary
# with -health. That invocation probes the local endpoint and exits 0 or 1.
# The app answers 503 until the schema exists and the consumer runs, so Docker
# reports healthy only when the app can do its job. Debezium waits on this.
HEALTHCHECK --interval=5s --timeout=5s --retries=10 --start-period=10s \
  CMD ["/example-app", "-health"]
ENTRYPOINT ["/example-app"]
