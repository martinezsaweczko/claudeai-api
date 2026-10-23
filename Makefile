# Makefile for building and managing claudeai-api
# Set LDFlags for versioning and build information
VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
APP_NAME := claudeai-api
LDFLAGS := -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME) -X main.appName=$(APP_NAME)

imageTag := claudeai-api
# swag runs from the version pinned in go.mod (tools/tools.go), so local and CI output match.
SWAG := go run github.com/swaggo/swag/cmd/swag

.PHONY: build binary check check-swagger ci clean container container-run coverage coverage-total run swagger test vulncheck

# Coverage profile. The generated swagger package (docs/) is excluded because
# it is swag output, not authored code.
COVERAGE_PROFILE := coverage.out
COVERAGE_EXCLUDE := /docs/

# swag emits an unformatted docs.go; gofmt keeps it identical to what CI checks.
swagger:
	$(SWAG) init -g cmd/main.go -o docs
	gofmt -w docs/docs.go

check-swagger:
	@tmp="$$(mktemp -d)/docs"; mkdir -p "$$tmp"; trap 'rm -rf "$$(dirname "$$tmp")"' EXIT; \
	$(SWAG) init -g cmd/main.go -o "$$tmp" && gofmt -w "$$tmp/docs.go" && \
	diff -r docs "$$tmp" || (echo "Swagger docs are out of date. Run 'make swagger' and commit the changes." && exit 1)

build: clean check check-swagger
	go build -ldflags "$(LDFLAGS)" -o build/main cmd/main.go

binary: swagger
	go build -ldflags "$(LDFLAGS)" -o build/main cmd/main.go

check:
	go vet ./...
	go fmt ./...
	go mod tidy

test:
	go test -race -count=1 ./...

# Runs the suite once with the race detector, writes $(COVERAGE_PROFILE) without
# the generated docs package and prints the total.
coverage:
	go test -race -count=1 -covermode=atomic -coverprofile=$(COVERAGE_PROFILE).tmp ./...
	@grep -v '$(COVERAGE_EXCLUDE)' $(COVERAGE_PROFILE).tmp > $(COVERAGE_PROFILE)
	@rm -f $(COVERAGE_PROFILE).tmp
	@go tool cover -func=$(COVERAGE_PROFILE) | tail -n 1

# Prints only the number (for example 71.4) from an existing profile; run
# `make coverage` first. CI uses this to build the badge.
coverage-total:
	@go tool cover -func=$(COVERAGE_PROFILE) | awk '/^total:/ { sub(/%/, "", $$NF); print $$NF }'

clean:
	rm -rf build

# Local run on loopback with the defaults (127.0.0.1:46564).
# Needs the `claude` CLI on PATH; export CLAUDE_CODE_OAUTH_TOKEN first (see README).
run:
	go run ./cmd

vulncheck:
	govulncheck ./...

# Runs the checks of .github/workflows/ci.yml locally without modifying any
# file. Not covered here: the container build. Update this target when a CI
# step is added or removed.
ci: vulncheck check-swagger
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:" && gofmt -l . && exit 1)
	go mod tidy -diff
	go build ./...
	go test -race -count=1 ./...

# Pin the Claude Code CLI with `make container CLAUDE_VERSION=x.y.z` (default: Containerfile).
container:
	podman build --format docker -f Containerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		$(if $(CLAUDE_VERSION),--build-arg CLAUDE_VERSION=$(CLAUDE_VERSION)) \
		-t $(imageTag):$(VERSION) \
		-t $(imageTag):latest \
		.

# Runs the image on 127.0.0.1:46564 only. Put CLAUDE_CODE_OAUTH_TOKEN (and
# optionally CLAUDEAI_API_KEYS) in ./.env (git-ignored); see compose.example.yaml.
container-run:
	podman run --rm --name claudeai-api -p 127.0.0.1:46564:46564 \
		--env-file .env $(imageTag):latest
