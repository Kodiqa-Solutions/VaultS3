# Stage 1: Build frontend
# Pin to the native build platform: the dashboard output is static, arch-independent
# HTML/CSS/JS, so there is no reason to build it under QEMU emulation for arm64. This
# also avoids the npm optional native-dependency resolution bug (e.g. lightningcss) that
# breaks emulated arm64 installs.
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.26-alpine AS builder
WORKDIR /app
# Build version (e.g. v4.2.12), injected by CI so the dashboard and `-version` show it.
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./internal/dashboard/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /vaults3 ./cmd/vaults3

# Stage 3: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    addgroup -g 1000 vaults3 && \
    adduser -D -u 1000 -G vaults3 vaults3 && \
    mkdir -p /data /metadata /etc/vaults3 /home/vaults3 && \
    chown -R vaults3:vaults3 /data /metadata /etc/vaults3 /home/vaults3

COPY --from=builder /vaults3 /usr/local/bin/vaults3
COPY configs/vaults3.yaml /etc/vaults3/vaults3.yaml

# A working directory the runtime user can write to. Without one every relative
# path resolves at /, which vaults3 (uid 1000) cannot create in, so anything
# using the built-in ./data, ./metadata or ./logs defaults failed with
# "permission denied". The server itself is unaffected either way: the image
# points it at the absolute /data and /metadata below.
WORKDIR /home/vaults3

ENV VAULTS3_ACCESS_KEY=""
ENV VAULTS3_SECRET_KEY=""
ENV VAULTS3_DATA_DIR="/data"
ENV VAULTS3_METADATA_DIR="/metadata"

EXPOSE 9000

USER vaults3

# The server probes itself. Using busybox wget here pulled a CVE into every
# deployment for a check the binary can do on its own, with no upstream fix
# coming, and it is also what a distroless image cannot provide at all.
# The subcommand reads the same config as the server, so a changed port, a
# reverse-proxy base path or TLS are all followed rather than assumed.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["vaults3", "healthcheck", "-config", "/etc/vaults3/vaults3.yaml"]

ENTRYPOINT ["vaults3"]
CMD ["-config", "/etc/vaults3/vaults3.yaml"]
