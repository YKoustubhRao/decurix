FROM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TheBot's sources, packed for go:embed (embed.go): without it the binary
# cannot build the desktop images itself (--build-images).
RUN go generate .

# Two cores and one package at a time (GOMAXPROCS, -p 1): an unbounded
# build inside Docker brought down the 8 GB WSL host this is developed on.
ENV GOMAXPROCS=2
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -p 1 -o decurix ./cmd/decurix

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
RUN apk add --no-cache docker-cli bash curl ca-certificates

# The templates are embedded in the binary (web/embed.go), so only .env has
# to come along.
COPY --from=builder /app/decurix .
COPY --from=builder /app/decurix-android-arm64 .
COPY .env ./

# The records database (decurix.db). docker-compose.yaml mounts a named volume
# here; the VOLUME line keeps a plain `docker run` from writing it into the
# container's own layer, where a rebuild would discard it.
ENV DECURIX_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

CMD ["./decurix"]