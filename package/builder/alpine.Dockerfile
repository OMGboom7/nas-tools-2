FROM golang:1.24-alpine AS db-builder
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nas-tools-db ./cmd/nas-tools-db

FROM python:3.10.11-alpine AS Builder

ENV NASTOOL_CONFIG=/nas-tools/config/config.yaml
ENV py_site_packages=/usr/local/lib/python3.10/site-packages

RUN apk add build-base git libxslt-dev libxml2-dev musl-dev gcc libffi-dev
RUN pip install --upgrade pip setuptools==67.7.2
RUN pip install wheel cython pyinstaller==5.7.0
WORKDIR /nas-tools
COPY . .
COPY --from=db-builder /out/nas-tools-db backend/bin/nas-tools-db
RUN pip install -r package/requirements.txt
RUN pip install pyparsing
RUN mkdir -p ${py_site_packages}/setuptools/_vendor/pyparsing/diagram/ \
    && cp ./package/rely/template.jinja2 ${py_site_packages}/setuptools/_vendor/pyparsing/diagram/
WORKDIR /nas-tools/package
RUN pyinstaller nas-tools.spec
RUN ./dist/nas-tools --init-db && ./dist/nas-tools --init-db && test -s ./dist/config/user.db && test -s ./dist/config/media.db
WORKDIR /rootfs
RUN cp /nas-tools/package/dist/nas-tools .

FROM scratch

COPY --from=Builder /rootfs/nas-tools /nas-tools
