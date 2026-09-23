# applab — a platform for deploying an application by uploading its source.

BINARY      := applab
CLI_BINARY  := applab-cli
PKG         := github.com/shaowenchen/applab
CMD         := ./cmd/applab
CLI_CMD     := ./cmd/applab-cli

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.BuildTime=$(BUILT)

# CGO is off because the SQLite driver is pure Go. The payoff is a static
# binary that runs on a distroless base image, which is what keeps the runtime
# image small and free of a shell.
export CGO_ENABLED := 0

IMAGE ?= docker.io/shaowenchen/applab
TAG   ?= latest

.PHONY: all
all: fmt vet test build

.PHONY: build
build: build-server build-cli

.PHONY: build-server
build-server:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

# The CLI is a separate artifact because it is what a person installs on their
# own machine, while the server is what a cluster runs.
.PHONY: build-cli
build-cli:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(CLI_BINARY) $(CLI_CMD)

.PHONY: run
run:
	APPLAB_KEY=$${APPLAB_KEY:-dev-key} go run $(CMD)

.PHONY: test
test:
	go test ./... -count=1

.PHONY: test-race
test-race:
	go test ./... -count=1 -race

.PHONY: coverage
coverage:
	go test ./... -coverprofile=coverage.out -count=1
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -s -w .

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "these files need gofmt:"; echo "$$out"; exit 1; fi

# Regenerate api/llms.txt from the route table. Required whenever a route is
# added, removed or reworded: the committed copy is checked against the
# generated one by TestLlmsTxtMatchesCommittedFile.
.PHONY: llms
llms:
	go run ./cmd/genllms

.PHONY: llms-check
llms-check:
	go run ./cmd/genllms -check

# The full gate: everything CI should run before a change is acceptable.
.PHONY: check
check: fmt-check vet llms-check test

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin coverage.out

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(TAG) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE):$(TAG)

# Render the chart at the values an install would use. Needs helm, which is not
# required to build or test the Go code — only to check the chart's templates.
.PHONY: helm-template
helm-template:
	helm template applab charts/applab --namespace applab-system \
		--set "auth.keys[0]=$${APPLAB_KEY:-replace-me}" \
		--set apps.baseDomain=$${BASE_DOMAIN:-apps.example.com} \
		--set build.registry=$${REGISTRY:-registry.example.com/apps}

.PHONY: helm-lint
helm-lint:
	helm lint charts/applab

# Render the chart and assert what was wrong before. Skips if helm is absent.
# Separate from `check` because helm is a chart-only dependency: requiring it to
# run the Go tests would make the common case need an install it does not.
.PHONY: helm-check
helm-check:
	./hack/helm-check.sh

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Targets without a description:"
	@grep -E '^[a-zA-Z_-]+:$$' $(MAKEFILE_LIST) | sed 's/:$$//' | sed 's/^/  /'
