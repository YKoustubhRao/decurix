FROM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o decurix ./cmd/decurix


FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    docker.io \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# The templates are embedded in the binary (web/embed.go), so only .env has
# to come along.
COPY --from=builder /app/decurix .
COPY .env ./

# The records database (decurix.db). docker-compose.yaml mounts a named volume
# here; the VOLUME line keeps a plain `docker run` from writing it into the
# container's own layer, where a rebuild would discard it.
ENV DECURIX_DATA_DIR=/data
VOLUME /data

EXPOSE 8080

CMD ["./decurix"]