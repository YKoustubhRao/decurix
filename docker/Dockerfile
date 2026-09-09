FROM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o flowera ./cmd/flowera


FROM debian:bookworm-slim

WORKDIR /app

# The templates are embedded in the binary (web/embed.go), so only .env has
# to come along.
COPY --from=builder /app/flowera .
COPY .env ./

EXPOSE 8080

CMD ["./flowera"]