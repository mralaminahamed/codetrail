# syntax=docker/dockerfile:1
#
# Build from the repository root: docker build -f infra/gateway.Dockerfile .

FROM golang:1.27@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/gateway ./apps/gateway/cmd

# The -debian13 suffix, not the bare static:nonroot alias: upstream warns the
# alias currently resolves here and will move, and an image whose base changes
# under a digest-free tag is a base nobody reviewed.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

# Every certificate in this bundle is a self-signed Amazon RDS root; none chains
# to a publicly trusted CA, so the Mozilla store distroless ships cannot
# validate an RDS server certificate and sslmode=verify-full fails at boot with
# an error about the certificate rather than about the setting.
#
# --chmod because ADD writes 0600 root-owned and the runtime user is 65532.
# --checksum because BuildKit invalidates a remote ADD on the URL and never on
# content: unpinned means frozen at whatever the bundle held on the first build,
# silently, for the life of the layer cache. Pinned, an AWS append breaks the
# build, which is a prompt to review it.
ADD --chmod=644 \
    --checksum=sha256:e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3 \
    https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
    /etc/ssl/certs/rds-global-bundle.pem

COPY --from=build /out/gateway /gateway

# Nothing else. No GO*, no GIT_*, no *_PROXY: closed by omission, mirroring
# symbols/policy.go, because an image is a second place the environment gets
# configured and no Go assertion can see it.
ENV PORT=8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/gateway"]
