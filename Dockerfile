# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
# CGO is off because the SQLite driver is pure Go, so the binary is static and
# carries no libc dependency of its own.
#
# The runtime base is Alpine rather than distroless, and that is a deliberate
# trade. applab shells out to `git` for everything that touches a repository, and
# `git` is the one dependency that cannot be compiled in. A distroless base would
# mean copying git and each of its shared libraries by hand out of a musl-based
# build stage — musl binaries do not run against glibc, so the libraries have to
# match too — which is a long list of file copies that breaks silently when Alpine
# bumps a library version. Taking git from the same Alpine release as the runtime
# gets a correct install in one step, and the image stays small because the
# toolchain never reaches it.
FROM golang:1.26-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

# Dependencies first, so a source change does not refetch the module graph. This
# layer is keyed on the module files alone and is reused until they move.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/applab/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/applab/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/applab ./cmd/applab \
 && go build -trimpath \
      -ldflags "-s -w \
        -X github.com/shaowenchen/applab/internal/buildinfo.Version=${VERSION} \
        -X github.com/shaowenchen/applab/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/applab-cli ./cmd/applab-cli

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM alpine:3.21

# git is not optional: applab creates a repository per app, builds commits from
# uploaded archives, and serves clones and pushes through git's own http-backend.
# curl is what the *build job's* init container uses, not this container — it is
# installed here only so an operator can debug from inside a running pod.
RUN apk add --no-cache git ca-certificates tini \
 && rm -rf /var/cache/apk/*

# The distroless images set this; Alpine does not, and applab resolves the data
# directory and git's config relative to it.
ENV HOME=/home/applab

# A non-root user with a real home directory. git writes to HOME, and a process
# whose HOME does not exist fails in ways that have nothing to do with the
# operation being attempted.
RUN addgroup -g 1000 applab \
 && adduser -u 1000 -G applab -h /home/applab -D applab \
 && mkdir -p /data \
 && chown -R applab:applab /data /home/applab

COPY --from=builder /out/applab /usr/local/bin/applab
COPY --from=builder /out/applab-cli /usr/local/bin/applab-cli

ENV APPLAB_DATA_DIR=/data \
    APPLAB_LISTEN=:8080 \
    APPLAB_LOG_LEVEL=info

USER applab

EXPOSE 8080

# tini reaps zombies and forwards signals. Without it the applab process is PID 1,
# and the kernel ignores a signal like SIGTERM when no handler is installed — so a
# pod termination would wait out the grace period instead of draining, taking
# in-flight uploads with it.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/applab"]
