# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26.11-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /agent ./cmd/agent

# ── Final stage ───────────────────────────────────────────────────────────────
FROM alpine:3.23

RUN apk add --no-cache ca-certificates libgcc

COPY --from=builder /agent /agent

ENTRYPOINT ["/agent"]
