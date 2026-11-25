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
RUN mkdir -p /out/licenses \
    && cp "$(go list -m -f '{{.Dir}}' github.com/longbridgeapp/opencc)/LICENSE" /out/licenses/opencc-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/liuzl/da)/LICENSE" /out/licenses/da-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/liuzl/cedar-go)/LICENSE.md" /out/licenses/cedar-go-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/minio/minio-go/v7)/LICENSE" /out/licenses/minio-go-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/minio/minio-go/v7)/NOTICE" /out/licenses/minio-go-NOTICE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/chromedp/chromedp)/LICENSE" /out/licenses/chromedp-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/chromedp/cdproto)/LICENSE" /out/licenses/cdproto-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/chromedp/sysutil)/LICENSE" /out/licenses/chromedp-sysutil-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/go-json-experiment/json)/LICENSE" /out/licenses/go-json-experiment-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/gobwas/ws)/LICENSE" /out/licenses/gobwas-ws-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/gobwas/httphead)/LICENSE" /out/licenses/gobwas-httphead-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/gobwas/pool)/LICENSE" /out/licenses/gobwas-pool-LICENSE

FROM alpine:3.24 AS ct2-builder
RUN apk add --no-cache build-base cmake ninja git ffmpeg-dev linux-headers
WORKDIR /build
RUN git init ct2 \
    && git -C ct2 remote add origin https://github.com/OpenNMT/CTranslate2.git \
    && git -C ct2 fetch --depth=1 origin 91d46ec8e21f49ba19f65e79872e0c87c51f4027 \
    && git -C ct2 checkout --detach FETCH_HEAD \
    && git -C ct2 submodule update --init --depth=1 third_party/ruy third_party/spdlog third_party/cpu_features \
    && git -C ct2/third_party/ruy submodule update --init --depth=1 third_party/cpuinfo
COPY backend/native/ct2-whisper/ helper/
RUN cmake -S helper -B out -G Ninja -DCT2_SOURCE=/build/ct2 \
       -DCMAKE_BUILD_TYPE=Release -DCMAKE_POLICY_VERSION_MINIMUM=3.5 \
       -DBUILD_CLI=OFF -DBUILD_TESTS=OFF -DBUILD_SHARED_LIBS=OFF \
       -DWITH_MKL=OFF -DWITH_RUY=ON -DRUY_MINIMAL_BUILD=ON -DOPENMP_RUNTIME=COMP \
    && cmake --build out --target nastool-ct2-whisper --parallel 2 \
    && strip out/nastool-ct2-whisper \
    && mkdir -p /out/licenses \
    && cp ct2/LICENSE /out/licenses/CTranslate2-LICENSE \
    && cp ct2/third_party/ruy/LICENSE /out/licenses/ruy-LICENSE \
    && cp ct2/third_party/ruy/third_party/cpuinfo/LICENSE /out/licenses/cpuinfo-LICENSE \
    && cp ct2/third_party/spdlog/LICENSE /out/licenses/spdlog-LICENSE \
    && cp ct2/third_party/cpu_features/LICENSE /out/licenses/cpu_features-LICENSE

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata libgomp libstdc++ ffmpeg whisper.cpp rclone aria2 chromium \
    && test -x /usr/bin/whisper-cli \
    && test -x /usr/bin/chromium \
    && test -u /usr/lib/chromium/chrome-sandbox \
    && addgroup -S -g 911 nt \
    && adduser -S -u 911 -G nt -h /app nt \
    && mkdir -p /config && chown nt:nt /config
WORKDIR /app
COPY --from=frontend-builder /build/frontend/dist /app/frontend/dist
COPY --from=ct2-builder /build/out/nastool-ct2-whisper /usr/local/bin/nastool-ct2-whisper
COPY --from=ct2-builder /out/licenses /usr/share/licenses/nas-tools-go
COPY --from=backend-builder /out/nas-tools-go /usr/local/bin/nas-tools-go
COPY --from=backend-builder /out/licenses /usr/share/licenses/nas-tools-go
COPY web/backend/user.sites.bin /app/user.sites.bin
COPY config/default-category.yaml /app/default-category.yaml
ENV NASTOOL_GO_ADDRESS="0.0.0.0:3000" \
    NASTOOL_DISABLE_LEGACY="true" \
    NASTOOL_CONFIG="/config/config.yaml" \
    NASTOOL_FRONTEND_DIST="/app/frontend/dist" \
    NASTOOL_SITE_CATALOG="/app/user.sites.bin" \
    NASTOOL_DEFAULT_CATEGORY="/app/default-category.yaml" \
    NASTOOL_BROWSER_BINARY="/usr/bin/chromium" \
    TZ="Asia/Shanghai"
USER 911:911
EXPOSE 3000
VOLUME ["/config"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:3000/api/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/nas-tools-go"]
