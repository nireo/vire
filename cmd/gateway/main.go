package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nireo/vire/gateway"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	registry := flag.String("registry", "models.json", "path to the model registry")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:9091", "private metrics listen address (empty disables listener)")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := gateway.NewServerWithOptions(*addr, *registry, gateway.Options{
		MetricsAddr: *metricsAddr,
		Logger:      logger,
	})
	if err != nil {
		logger.Error("gateway initialization failed", "error", err)
		os.Exit(1)
	}
	if err := server.Run(ctx); err != nil {
		logger.Error("gateway stopped with error", "error", err)
		os.Exit(1)
	}
}
