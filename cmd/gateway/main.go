package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nireo/vire/gateway"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	registry := flag.String("registry", "models.json", "path to the model registry")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := gateway.NewServer(*addr, *registry)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("gateway listening on %s (registry: %s)", *addr, *registry)
	if err := server.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
