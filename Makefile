.PHONY: fmt test lint check build run

fmt:
	go fmt ./...

test:
	go test ./...

lint:
	go vet ./...

check: fmt test lint

build:
	go build ./...

run:
	go run ./cmd/gateway
