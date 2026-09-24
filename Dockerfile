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
#
# The builder is pinned to $BUILDPLATFORM, and that is not a detail. Left
# unpinned, a multi-arch build pulls an image per target architecture and runs
# this whole stage — the module download and every compilation — under QEMU for
# each one that is not the runner's, which is slow enough to look like a hang: a
# Go build that takes twenty seconds natively takes many minutes emulated.
# Pinned, the stage runs natively and Go cross-compiles, which it does at full
# speed — CGO_ENABLED=0 below means there is no C toolchain to emulate, so the
# target architecture changes only which object files the compiler emits.
#
# The runtime stage below is deliberately not pinned: it installs
# target-architecture packages, so it has to run for the target. That is apk plus
# a few file operations, which is cheap where compiling a module graph is not.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Supplied by BuildKit from the build's --platform list. Declared so that a
# build without them (a plain `docker build`) still gets a working default
# rather than an empty GOOS.
ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETOS=linux
ARG TARGETARCH

ARG VERSION=dev
ARG COMMIT=unknown

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
#
# git-daemon is a separate package on Alpine, and it is the one that carries
# `git-http-backend` — Alpine splits git's binaries across subpackages rather
# than installing them with the main one. Without it applab starts, logs that it
# is up, and refuses at the first clone or push with "git-http-backend not found;
# it ships with git" — which is true of git as a whole and false of this package.
# It is the only thing installed here that nothing else pulls in, so it is the
# one that gets left out.
RUN apk add --no-cache git git-daemon ca-certificates tini \
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
