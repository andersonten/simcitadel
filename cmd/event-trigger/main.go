package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/health"
	"github.com/andersonten/simcitadel/internal/trigger"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	listenAddr := config.OptString("EVENT_TRIGGER_LISTEN_ADDR", ":8080")

	healthCheck := flag.Bool("health", false, "Probe the local health endpoint and exit")
	flag.Parse()

	if *healthCheck {
		if err := health.ProbeListenAddr(context.Background(), listenAddr); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	amqpURL := config.MustURL("EVENT_TRIGGER_AMQP_URL")

	confirmTimeout := config.OptDuration("EVENT_TRIGGER_PUBLISH_CONFIRM_TIMEOUT", 5*time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, err := citadelamqp.Dial(ctx, amqpURL.String())
	if err != nil {
		slog.Error("connect to AMQP broker", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		slog.Error("open AMQP channel", "error", err)
		os.Exit(1)
	}

	pub, err := citadelamqp.NewPublisher(ch, confirmTimeout)
	if err != nil {
		slog.Error("create publisher", "error", err)
		os.Exit(1)
	}

	budget := trigger.NewBudget(config.OptInt("EVENT_TRIGGER_PUBLISH_BUDGET", 100))
	srv := trigger.NewServer(pub, budget, func() bool { return !conn.IsClosed() })

	httpServer := &http.Server{Addr: listenAddr, Handler: srv.Handler()}

	go func() {
		slog.Info("start HTTP server", "addr", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
}
