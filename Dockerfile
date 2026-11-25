# syntax=docker/dockerfile:1
# artex-unified-dockerfile-v1
# --target source builds the application from source.
# The default prebuilt target packages dist/<TARGETARCH>/artex.
# Both targets share the former Dockerfile.source runtime.

FROM node:22-alpine AS fe
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci
COPY web/ ./
RUN npm run build:static

FROM golang:1.26-alpine AS gobuild
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p server/webui/dist
COPY --from=fe /src/web/out/ server/webui/dist/
RUN CGO_ENABLED=0 go build -tags embedui -trimpath -ldflags "-s -w" -o /out/artex ./cmd/artex

FROM python:3.12-slim-bookworm AS runtime
ARG TARGETARCH
ARG APT_MIRROR=
ARG NPM_REGISTRY=https://registry.npmjs.org
ARG NODE_DIST_BASE=https://nodejs.org/dist
ARG NODE_VERSION=22.14.0
ARG PLAYWRIGHT_DOWNLOAD_HOST=
RUN if [ -n "$APT_MIRROR" ]; then \
      sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    fi \
    && apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      build-essential \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
RUN set -e; \
    case "$TARGETARCH" in \
      amd64) NODE_ARCH=x64 ;; \
      arm64) NODE_ARCH=arm64 ;; \
      *) echo "Unsupported Node architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL -o "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" "${NODE_DIST_BASE}/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz"; \
    curl -fsSL "${NODE_DIST_BASE}/v${NODE_VERSION}/SHASUMS256.txt" \
      | grep "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" | sha256sum -c -; \
    tar -xzf "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz" -C /usr/local --strip-components=1; \
    rm -f "node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.gz"
RUN npm config set registry "$NPM_REGISTRY" \
    && npm install -g @playwright/mcp@0.0.80 @playwright/cli@0.1.19 playwright@1.63.0 \
    && playwright-cli --help \
    && if [ -n "$PLAYWRIGHT_DOWNLOAD_HOST" ]; then \
         PLAYWRIGHT_DOWNLOAD_HOST="$PLAYWRIGHT_DOWNLOAD_HOST" playwright install --with-deps chromium; \
       else \
         playwright install --with-deps chromium; \
       fi \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY start.sh /app/start.sh
RUN chmod +x /app/start.sh
COPY skills/ /app/skills/
COPY packaging/tools-manifest.json /app/packaging/tools-manifest.json
ENV ARTEX_TOOLS_MANIFEST=/app/packaging/tools-manifest.json
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

FROM runtime AS source
COPY --from=gobuild /out/artex /app/artex
RUN chmod +x /app/artex

FROM runtime AS prebuilt
ARG TARGETARCH
COPY dist/${TARGETARCH}/artex /app/artex
RUN chmod +x /app/artex
