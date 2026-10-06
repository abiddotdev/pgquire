# The pgquire server as a small image: a static binary (no cgo) on distroless.
# Builds from server/; the release workflow publishes it to ghcr.io/abiddotdev/pgquire for amd64 and arm64.
#
# Build:  docker build -t pgquire .
# Run:    docker run -p 127.0.0.1:8432:8432 -v pgquire-config:/config -e PGQUIRE_TOKEN=secret pgquire
#
# Publish on loopback only (-p 127.0.0.1:...): saved Postgres connections are reachable
# through this server, so don't expose it to the network. Without PGQUIRE_TOKEN the token is
# new each start; `docker logs` shows the link.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH VERSION
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w ${VERSION:+-X main.Version=$VERSION}" -o /out/pgquire . && mkdir /out/config

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pgquire /pgquire
# owned by the nonroot user, so a fresh named volume mounted here is writable
COPY --from=build --chown=65532:65532 /out/config /config
VOLUME /config
EXPOSE 8432
# Defaults as variables, so `docker run -e PGQUIRE_LISTEN=0.0.0.0:9000 ...` can change them.
ENV PGQUIRE_LISTEN=0.0.0.0:8432 PGQUIRE_NO_OPEN=1 PGQUIRE_CONFIG=/config/connections.json
ENTRYPOINT ["/pgquire"]
