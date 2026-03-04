# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.22-bookworm AS builder

# Install Tesseract and its development headers (required by gosseract/CGO).
RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    libtesseract-dev \
    libleptonica-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache dependency downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /out/discord-llm-bot \
    ./main.go

# Can't use scratch — gosseract links against libtesseract at runtime.
FROM debian:bookworm-slim

# Install Tesseract runtime + Japanese language data.
RUN apt-get update && apt-get install -y --no-install-recommends \
    tesseract-ocr \
    tesseract-ocr-jpn \
    tesseract-ocr-jpn-vert \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/discord-llm-bot /discord-llm-bot

ENTRYPOINT ["/discord-llm-bot"]