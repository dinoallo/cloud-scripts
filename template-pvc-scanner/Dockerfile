# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.22
ARG ALPINE_VERSION=3.20

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG APP_DIR=template-pvc-scanner
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY ${APP_DIR}/go.mod ${APP_DIR}/go.sum ./
RUN go mod download
COPY ${APP_DIR}/ ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/template-pvc-scanner .

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates tar && adduser -D -H -u 65532 scanner
COPY --from=build /out/template-pvc-scanner /template-pvc-scanner
USER 65532:65532
ENTRYPOINT ["/template-pvc-scanner"]
