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

FROM python:3.10.11-alpine
COPY package_list.txt requirements.txt /tmp/nas-tools-build/
RUN apk add --no-cache --virtual .build-deps \
        libffi-dev \
        gcc \
        musl-dev \
        libxml2-dev \
        libxslt-dev \
    && apk add --no-cache $(cat /tmp/nas-tools-build/package_list.txt) \
    && curl https://rclone.org/install.sh | bash \
    && if [ "$(uname -m)" = "x86_64" ]; then ARCH=amd64; elif [ "$(uname -m)" = "aarch64" ]; then ARCH=arm64; else exit 1; fi \
    && curl https://dl.min.io/client/mc/release/linux-${ARCH}/mc --create-dirs -o /usr/bin/mc \
    && chmod +x /usr/bin/mc \
    && pip install --upgrade pip setuptools wheel \
    && pip install cython \
    && pip install -r /tmp/nas-tools-build/requirements.txt \
    && apk del --purge .build-deps \
    && rm -rf /tmp/* /root/.cache /var/cache/apk/*

ENV S6_SERVICES_GRACETIME=30000 \
    S6_KILL_GRACETIME=60000 \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0 \
    S6_SYNC_DISKS=1 \
    HOME="/nt" \
    TERM="xterm" \
    PATH=${PATH}:/usr/lib/chromium \
    LANG="C.UTF-8" \
    TZ="Asia/Shanghai" \
    NASTOOL_CONFIG="/config/config.yaml" \
    NASTOOL_SITE_CATALOG="/nas-tools/web/backend/user.sites.bin" \
    NASTOOL_DEFAULT_CATEGORY="/nas-tools/config/default-category.yaml" \
    NASTOOL_AUTO_UPDATE=false \
    NASTOOL_CN_UPDATE=true \
    NASTOOL_VERSION=master \
    NASTOOL_UI_MODE=react \
    NASTOOL_LEGACY_PORT=3002 \
    NASTOOL_LEGACY_URL="http://127.0.0.1:3002" \
    NASTOOL_FRONTEND_DIST="/nas-tools/frontend/dist" \
    PS1="\u@\h:\w \$ " \
    PYPI_MIRROR="https://pypi.tuna.tsinghua.edu.cn/simple" \
    ALPINE_MIRROR="mirrors.ustc.edu.cn" \
    PUID=0 \
    PGID=0 \
    UMASK=000 \
    PYTHONWARNINGS="ignore:semaphore_tracker:UserWarning" \
    WORKDIR="/nas-tools"

WORKDIR ${WORKDIR}
RUN mkdir ${HOME} \
    && addgroup -S nt -g 911 \
    && adduser -S nt -G nt -h ${HOME} -s /bin/bash -u 911 \
    && python_ver=$(python3 -V | awk '{print $2}') \
    && python_path=$(which python3) \
    && [ -d "/usr/lib/python${python_ver%.*}/site-packages" ] || mkdir -p "/usr/lib/python${python_ver%.*}/site-packages" \
    && echo "${WORKDIR}/" > /usr/lib/python${python_ver%.*}/site-packages/nas-tools.pth \
    && echo 'fs.inotify.max_user_watches=5242880' >> /etc/sysctl.conf \
    && echo 'fs.inotify.max_user_instances=5242880' >> /etc/sysctl.conf \
    && echo "nt ALL=(ALL) NOPASSWD: ALL" >> /etc/sudoers

COPY --chown=nt:nt . ${WORKDIR}
COPY --from=frontend-builder --chown=nt:nt /build/frontend/dist ${WORKDIR}/frontend/dist
COPY --from=backend-builder /out/nas-tools-go /usr/local/bin/nas-tools-go
COPY --chmod=755 docker/rootfs /

EXPOSE 3000
VOLUME ["/config"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/usr/local/bin/nas-tools-healthcheck"]
ENTRYPOINT ["/init"]
