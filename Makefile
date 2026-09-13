.PHONY: fmt fmt-check test test-race lint check build run dev smoke

fmt:
	go fmt ./...

fmt-check:
	@files="$$(gofmt -l .)" || exit $$?; if [ -n "$$files" ]; then printf 'Run make fmt:\n%s\n' "$$files"; exit 1; fi

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	go vet ./...

check: fmt-check test-race lint

build:
	go build ./...

run:
	go run ./cmd/gateway

dev:
	@bash scripts/dev.sh

smoke:
	@bash scripts/smoke.sh
