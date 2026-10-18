# syntax=docker/dockerfile:1
# Built for linux/amd64 only (DS220+ is an Intel J4025):
#   docker buildx build --platform linux/amd64 -t ghcr.io/azthrun/dreambig-nas-file-api .

# --- build: one static binary ---
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/nas-file-api ./cmd/nas-file-api

# --- runtime: the binary alone, no shell ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nas-file-api /nas-file-api
# The binary is root-owned and world-readable, so any UID/GID can run it but none can modify it.
# Default to distroless's unprivileged "nonroot" user; compose overrides with the owner's UID/GID.
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/nas-file-api", "-healthcheck"]
ENTRYPOINT ["/nas-file-api"]
