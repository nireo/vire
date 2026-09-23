.PHONY: fmt fmt-check test test-race lint check build run dev smoke web-dev web-build local-up local-down test-vllm test-vllm-runner sqlc-generate sqlc-check

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

sqlc-generate:
	sqlc generate

sqlc-check:
	sqlc compile
	sqlc diff

build:
	go build ./...

run:
	go run ./cmd/gateway

dev:
	@bash scripts/dev.sh

smoke:
	@bash scripts/smoke.sh

web-dev:
	cd web && pnpm dev

web-build:
	cd web && pnpm build

# Real vLLM, disposable Postgres, gateway, and Vite portal.
local-up:
	python3 scripts/local_stack.py up

local-down:
	python3 scripts/local_stack.py down

# Opt-in: starts real inference and may download model weights.
test-vllm:
	python3 scripts/integration_vllm.py $(VLLM_TEST_ARGS)

# Test the integration runner itself without downloading or loading a model.
test-vllm-runner:
	python3 -B -m unittest discover -s scripts -p 'test_integration_vllm.py'
