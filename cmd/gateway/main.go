package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nireo/vire/gateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := gateway.NewServer(":8080", "models.json")
	if err != nil {
		log.Fatal(err)
	}
	if err := server.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
