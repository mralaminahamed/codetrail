# syntax=docker/dockerfile:1
#
# Build from the repository root: docker build -f infra/indexer.Dockerfile .

FROM golang:1.27@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/indexer ./apps/indexer/cmd

# Debian stable with a toolchain copied in, and explicitly not FROM golang:
# that image carries GOPATH, GOFLAGS and a populated module cache into the
# runtime layer, and symbols/policy.go closes those by omission. Policy.Env
# re-points GOMODCACHE per job so none of it is reachable, but the guarantee
# would then live in one line of Go rather than in an artifact anyone can read.
FROM debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132

# git is what the indexer forks. ca-certificates is what lets it verify a forge:
# Debian's git only Recommends it and --no-install-recommends drops it, so
# without this line every `git clone https://…` fails on the certificate while
# the image otherwise looks entirely healthy. It is also what creates
# /etc/ssl/certs — measured, dropping the package left the ADD below to create
# that directory itself, unreadable by uid 65532.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# GOROOT/go.env travels inside this directory carrying GOPROXY, GOSUMDB and
# GOTOOLCHAIN=auto, so the claim is not "nothing is baked in" — it is that
# Policy.Env's real environment variables outrank a defaults file. GOENV=off
# closes the *user* env file, not this one.
COPY --from=golang:1.27@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa \
     /usr/local/go /usr/local/go

# Exactly one go on PATH, and no golang-go from apt. policy.go's Validate
# refuses to run unless exec.LookPath("go") is the configured GoBin, so a second
# go is a worker that logs no_toolchain and writes an all-syntactic graph while
# `go version` works perfectly from a shell.
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin

# SCRATCH_DIR is where job trees and the go caches go, so the deployment can
# make one directory the only writable path. HOME reaches neither subprocess —
# clone.Run sets git's last and Policy.Env builds the compiler's from nothing —
# but it is the indexer process's own, and a non-existent /root under a
# read-only root filesystem is the wrong place to point it.
ENV HOME=/scratch \
    SCRATCH_DIR=/scratch \
    PROBE_PORT=9090

# See gateway.Dockerfile for why this is pinned and why the mode is explicit.
ADD --chmod=644 \
    --checksum=sha256:e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3 \
    https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
    /etc/ssl/certs/rds-global-bundle.pem

COPY --from=build /out/indexer /indexer
RUN install -d -o 65532 -g 65532 -m 0755 /scratch

EXPOSE 9090
USER 65532:65532
ENTRYPOINT ["/indexer"]
