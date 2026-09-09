// Command example-app runs a small application that stands in for the real
// service under test, so the compose stack of SDD section 3.1 runs the whole
// pipeline without an image from another repository.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/exampleapp"
	"github.com/andersonten/simcitadel/internal/health"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	healthCheck := flag.Bool("health", false, "Probe the local health endpoint and exit")
	flag.Parse()

	if *healthCheck {
		if err := health.ProbeListenAddrPath(context.Background(), listenAddress(), readinessPath); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("example-app failed", "error", err)
		os.Exit(1)
	}
}

// readinessPath is the path the app serves and the path the example config names
// under app.readinessPath. The two must agree, and a scenario proves they do.
const readinessPath = "/healthz"

func listenAddress() string {
	if addr := os.Getenv("EXAMPLE_APP_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := exampleapp.Config{
		AMQPURL:            config.MustString("EXAMPLE_APP_AMQP_URL"),
		UpstreamGraphQLURL: config.MustString("EXAMPLE_APP_UPSTREAM_GRAPHQL_URL"),
		InboundExchange:    config.OptString("EXAMPLE_APP_INBOUND_EXCHANGE", "Journal.Article"),
		InboundQueue:       config.OptString("EXAMPLE_APP_INBOUND_QUEUE", "example-app.article-events"),
		InboundRoutingKey:  config.OptString("EXAMPLE_APP_INBOUND_ROUTING_KEY", "Journal.Article-Submitted.#"),
		InboundAdditionalRoutingKeys: config.OptStringSlice(
			"EXAMPLE_APP_INBOUND_ADDITIONAL_ROUTING_KEYS", ",", nil),
		OutboundExchange: config.OptString("EXAMPLE_APP_OUTBOUND_EXCHANGE",
			"public.orchestration-engine.Manuscript.v1"),
		ListenAddr: listenAddress(),
	}
	dsn := config.MustString("EXAMPLE_APP_DB_DSN")

	sqlDB, err := db.Open(ctx, "postgres", dsn)
	if err != nil {
		return fmt.Errorf("open the application database: %w", err)
	}
	defer sqlDB.Close()

	app := exampleapp.New(cfg, sqlDB, logger)

	// The migration runs before the HTTP server reports ready, and Debezium waits
	// on that readiness. Debezium resolves table.include.list against the live
	// catalog when it starts and exits rather than wait, so the outbox table must
	// exist first.
	if err := app.MigrateWithRetry(ctx); err != nil {
		return err
	}

	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: app.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Error("http server shutdown failed", "error", shutdownErr)
		}
	}()

	go func() {
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("http server failed", "error", serveErr)
			stop()
		}
	}()

	logger.Info("example-app starting",
		"addr", cfg.ListenAddr,
		"inboundExchange", cfg.InboundExchange,
		"outboundExchange", cfg.OutboundExchange,
		"upstream", cfg.UpstreamGraphQLURL)

	conn, err := amqp.Dial(ctx, cfg.AMQPURL)
	if err != nil {
		return fmt.Errorf("dial the broker: %w", err)
	}
	defer conn.Close()

	return app.Consume(ctx, conn)
}
