# AppLab — a platform for deploying an application by uploading its source.

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

# The full gate: everything CI should run before a change is acceptable.
.PHONY: check
check: fmt-check vet test

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

# Start the image and ask it to work. Skips without docker.
#
# Separate from `check` because it needs a container runtime, which the Go checks
# deliberately do not. CI runs it on every change — see image.yml.
.PHONY: image-smoke
image-smoke:
	./hack/image-smoke.sh $(IMAGE):$(TAG)

# Render the chart at the values an install would use. Needs helm, which is not
# required to build or test the Go code — only to check the chart's templates.
.PHONY: helm-template
helm-template:
	helm template applab charts/applab --namespace applab-system \
		--set "auth.key=$${APPLAB_KEY:-replace-me}" \
		--set apps.baseDomain=$${BASE_DOMAIN:-apps.example.com} \
		--set build.registry=$${REGISTRY:-registry.example.com/apps}

.PHONY: helm-lint
helm-lint:
	helm lint charts/applab

# Package the chart into a Helm repository directory. Needs helm; the same
# script CI runs, so a local publish and a published one cannot diverge.
#   make chart-package VERSION=0.1.0-dev PAGES=./pages
#
# The version defaults to what this commit would publish — the same script CI
# runs — rather than a literal here, so a chart packaged by hand cannot be
# stamped with a version the image is not published under.
.PHONY: chart-package
chart-package: PAGES ?= ./pages
chart-package:
	@mkdir -p $(PAGES)
	./hack/package-chart.sh $${VERSION:-$$(./hack/chart-version.sh)} $${APP_VERSION:-$$(git rev-parse --short HEAD)} $(PAGES) $${REPO_URL:-https://www.chenshaowen.com/applab}

# Render the chart and assert what was wrong before. Skips if helm is absent.
# Separate from `check` because helm is a chart-only dependency: requiring it to
# run the Go tests would make the common case need an install it does not.
.PHONY: helm-check
helm-check:
	./hack/helm-check.sh

# Render this repository's documentation into the static site published
# alongside the chart. Needs no helm and no cluster: it reads the markdown and
# writes HTML, and fails on a link that would be dead on the site.
#   make docs PAGES=./pages
.PHONY: docs
docs: PAGES ?= ./pages
docs:
	@mkdir -p $(PAGES)
	go run ./cmd/gendocs -dest $(PAGES) \
		-repo $${REPO_URL:-https://github.com/shaowenchen/applab} \
		-branch $${REPO_BRANCH:-master}

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Targets without a description:"
	@grep -E '^[a-zA-Z_-]+:$$' $(MAKEFILE_LIST) | sed 's/:$$//' | sed 's/^/  /'
