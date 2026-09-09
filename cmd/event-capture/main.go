package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/capture"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/health"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	healthCheck := flag.Bool("health", false, "Probe the local health endpoint and exit")
	flag.Parse()

	if *healthCheck {
		if err := health.ProbeListenAddr(context.Background(), listenAddress()); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := run(logger); err != nil {
		logger.Error("event-capture failed", "error", err)
		os.Exit(1)
	}
}

// listenAddress returns the address the HTTP server binds to.
func listenAddress() string {
	return config.OptString("EVENT_CAPTURE_LISTEN_ADDR", ":8080")
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	amqpURL := config.MustString("EVENT_CAPTURE_AMQP_URL")

	listenAddr := listenAddress()

	exchangeConfigs, err := parseExchanges(config.OptString("EVENT_CAPTURE_EXCHANGES", ""))
	if err != nil {
		return err
	}

	// SDD section 8.2 defaults the ring buffer to 1000 messages per exchange.
	bufferSize := config.OptInt("EVENT_CAPTURE_BUFFER_SIZE", 1000)

	bindTimeout := config.OptDuration("EVENT_CAPTURE_BIND_TIMEOUT", 120*time.Second)

	conn, err := amqp.Dial(ctx, amqpURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	bm := capture.NewBindingManager(conn, exchangeConfigs, bufferSize, bindTimeout)

	go func() {
		if startErr := bm.Start(ctx); startErr != nil {
			logger.Error("binding manager start failed", "error", startErr)
		}
	}()

	srv := capture.NewServer(bm.Buffer(), bm.Status, logger)
	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: srv.Handler(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Error("http server shutdown failed", "error", shutdownErr)
		}
	}()

	logger.Info("event-capture starting", "addr", listenAddr, "exchanges", len(exchangeConfigs))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func parseExchanges(raw string) ([]capture.ExchangeConfig, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	configs := make([]capture.ExchangeConfig, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// SDD section 8.1 defaults the declare mode to own, so a bare exchange
		// name is a complete entry.
		name, declare := part, "own"
		if colonIdx := strings.LastIndex(part, ":"); colonIdx >= 0 {
			name, declare = part[:colonIdx], part[colonIdx+1:]
		}
		if declare != "own" && declare != "passive" {
			return nil, fmt.Errorf("invalid declare mode %q for exchange %q: use \"own\" or \"passive\"", declare, name)
		}
		configs = append(configs, capture.ExchangeConfig{Name: name, Declare: declare})
	}
	return configs, nil
}
