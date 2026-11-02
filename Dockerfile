# --- build stage: compile a static binary ---
# Runs on the build machine's own arch and cross-compiles for the target, so
# `--platform linux/amd64` on an arm64 Mac doesn't need slow emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/importer-api .

# --- runtime stage: tiny image, no shell, runs as non-root ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/importer-api /importer-api
EXPOSE 8080
# Numeric UID (distroless "nonroot"): the kubelet can only verify runAsNonRoot
# from a number, since it can't look up user names inside the image.
USER 65532:65532
ENTRYPOINT ["/importer-api"]
