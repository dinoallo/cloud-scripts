# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG APP_DIR
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY ${APP_DIR}/go.mod ${APP_DIR}/go.sum ./
RUN go mod download
COPY ${APP_DIR}/ ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/app .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
