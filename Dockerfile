# Build stage. The whole Go module is compiled from source so the image does
# not depend on a prebuilt binary existing on the build host.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first: this layer only changes when go.mod/go.sum do.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
COPY embed.go ./

# CGO off keeps the result a true static binary, which is what lets the same
# build run in a scratch-like image and on an OpenWrt box.
ENV CGO_ENABLED=0
ARG VERSION=docker
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/needle-server ./cmd/needle-server && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/needle-client ./cmd/needle-client

# Runtime stage. No shell, no package manager, no libc: nothing to exploit if
# the server is ever reached by something it should not be.
FROM scratch AS runtime

# The named volume is owned by root, and this image runs as an unprivileged
# user, so the ownership has to be fixed once at startup. Docker copies the
# image's own /data (empty, but correctly owned) into a fresh volume, which is
# what makes that work.
COPY --from=build --chown=65534:65534 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/needle-server /needle-server
COPY --from=build /out/needle-client /needle-client
COPY --chown=65534:65534 --from=build /out/needle-server /data/.keep

VOLUME ["/data"]

EXPOSE 8080
USER 65534:65534

ENTRYPOINT ["/needle-server"]
CMD ["--db", "/data/needle.db", "--secure-cookies"]
