# The same Go as the desktop image's builder (docker/desktop.basic.Dockerfile), so one toolchain builds both (B-38).
FROM golang:1.27 AS builder

WORKDIR /app

COPY go.mod go.sum ./
# go has no download timeout of its own: ten minutes a try, three tries.
RUN for try in 1 2 3; do timeout 600 go mod download && exit 0; echo "go mod download: try $try failed or stalled"; sleep $((try * 20 - 10)); done; exit 1

COPY . .

# TheBot's sources, packed for go:embed (embed.go): without it the binary
# cannot build the desktop images itself (--build-images).
RUN go generate .

# Two cores and one package at a time (GOMAXPROCS, -p 1): an unbounded
# build inside Docker brought down the 8 GB WSL host this is developed on.
# -trimpath keeps the build machine's paths out of the binary, and the version is stamped as release.sh stamps it
# (DECURIX_VERSION, a build arg; "dev" when not given), so a running container says which build it is (B-38).
ENV GOMAXPROCS=2
ARG DECURIX_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -p 1 -trimpath -ldflags "-X main.version=${DECURIX_VERSION}" -o decurix ./cmd/decurix

# The phone build of the same binary, handed out by /phone/decurix when a
# phone sets up its link from its own browser (httpapi/phonelink.go).
RUN CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
    go build -p 1 -trimpath -ldflags "-s -w" -o decurix-android-arm64 ./cmd/decurix


FROM alpine:3.24

WORKDIR /app

# The control plane is one static binary (CGO_ENABLED=0); in here it runs only
# the docker command (the host-app hop) and bash. Alpine with docker-cli is a
# few MB from apk; Debian with docker.io (the whole engine) took about five
# minutes of a first start of the app.
# Bounded, as every download in a Dockerfile here is (internal/desktop/dockerfile_downloads_test.go), and never
# tied to one mirror: dl-cdn's community index opened and then sent nothing on the owner's network, and an apk
# here held a first start for 18 minutes. Three minutes a mirror (busybox timeout), each of Alpine's official
# mirrors in turn: the list desktop/electron/wsl/apk-safe.sh tries.
RUN for mirror in https://dl-cdn.alpinelinux.org/alpine https://mirrors.edge.kernel.org/alpine https://uk.alpinelinux.org/alpine; do \
        sed -i -E "s#^[a-z]+://[^ ]*(/(v[0-9]+\.[0-9]+|edge)/)#${mirror}\1#" /etc/apk/repositories; \
        timeout 180 apk add --no-cache docker-cli bash curl ca-certificates && exit 0; \
        echo "apk add: failed or did not finish within 180 s from ${mirror}; trying the next mirror"; sleep 10; \
    done; echo "apk add: failed from every Alpine mirror" >&2; exit 1

# The templates are embedded in the binary (web/embed.go). The settings (.env, with the keys) are never copied into
# an image layer, where anyone with the image could read them: compose hands them to the container when it starts
# (env_file in docker-compose.yaml).
COPY --from=builder /app/decurix .
COPY --from=builder /app/decurix-android-arm64 .

# The records database (decurix.db). docker-compose.yaml mounts a named volume
# here; the VOLUME line keeps a plain `docker run` from writing it into the
# container's own layer, where a rebuild would discard it.
ENV DECURIX_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

CMD ["./decurix"]