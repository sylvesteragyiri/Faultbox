# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod ./
# go.sum is copied too once the module has dependencies.
COPY go.su[m] ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/faultbox ./cmd/faultbox

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/faultbox /usr/local/bin/faultbox

ENV FAULTBOX_PORT=8080 \
    FAULTBOX_DATABASE_PATH=/data/faultbox.db
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/faultbox", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/faultbox"]
