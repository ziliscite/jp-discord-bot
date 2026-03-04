FROM golang:1.25.6-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    libtesseract-dev \
    libleptonica-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /out/discord-llm-bot \
    ./cmd/main.go

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    tesseract-ocr-jpn \
    tesseract-ocr-jpn-vert \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/discord-llm-bot /discord-llm-bot
ENTRYPOINT ["/discord-llm-bot"]