# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/faultbox ./cmd/faultbox

# The runtime image has no shell, so prepare the data directory here. A named
# volume mounted at /data copies this ownership on first use, which lets the
# nonroot user (65532) create the database.
RUN mkdir -p /out/data && chown 65532:65532 /out/data && chmod 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/faultbox /usr/local/bin/faultbox
COPY --from=build --chown=65532:65532 /out/data /data

# GOMEMLIMIT keeps the Go heap well inside the 128 MB container target (PRD §9).
# Raise it together with mem_limit if you give the container more memory.
ENV FAULTBOX_PORT=8080 \
    FAULTBOX_DATABASE_PATH=/data/faultbox.db \
    GOMEMLIMIT=96MiB
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot

# Probes /ready: the database must answer, not just the process.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/faultbox", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/faultbox"]
