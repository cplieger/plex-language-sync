# check=error=true
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
# GOTOOLCHAIN=auto: a Renovate dep bump requiring a newer Go downloads that toolchain
# instead of failing the build (org convention, go.md/ci-cd.md); still reproducible
# because go.mod pins the toolchain version. `local` would hard-fail such a build.
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /plex-language-sync .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

COPY --chmod=755 --from=builder /plex-language-sync /plex-language-sync
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/plex-language-sync", "health"]
ENTRYPOINT ["/plex-language-sync"]
