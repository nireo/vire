.PHONY: fmt fmt-check test test-race lint check build run dev smoke test-vllm test-vllm-runner

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

# Opt-in: starts real inference and may download model weights.
test-vllm:
	python3 scripts/integration_vllm.py $(VLLM_TEST_ARGS)

# Test the integration runner itself without downloading or loading a model.
test-vllm-runner:
	python3 -B -m unittest discover -s scripts -p 'test_integration_vllm.py'
