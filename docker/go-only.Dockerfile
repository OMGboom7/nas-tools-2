FROM node:22-alpine AS frontend-builder
WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.24-alpine AS backend-builder
WORKDIR /build/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nas-tools-go ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 911 nt \
    && adduser -S -u 911 -G nt -h /app nt
WORKDIR /app
COPY --from=frontend-builder /build/frontend/dist /app/frontend/dist
COPY --from=backend-builder /out/nas-tools-go /usr/local/bin/nas-tools-go
COPY web/backend/user.sites.bin /app/user.sites.bin
COPY config/default-category.yaml /app/default-category.yaml
ENV NASTOOL_GO_ADDRESS="0.0.0.0:3000" \
    NASTOOL_DISABLE_LEGACY="true" \
    NASTOOL_CONFIG="/config/config.yaml" \
    NASTOOL_FRONTEND_DIST="/app/frontend/dist" \
    NASTOOL_SITE_CATALOG="/app/user.sites.bin" \
    NASTOOL_DEFAULT_CATEGORY="/app/default-category.yaml" \
    TZ="Asia/Shanghai"
USER 911:911
EXPOSE 3000
VOLUME ["/config"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:3000/api/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/nas-tools-go"]
