# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/payment-worker ./cmd/payment-worker

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /app/payment-worker /app/payment-worker

USER nonroot:nonroot

EXPOSE 8001

ENTRYPOINT ["/app/payment-worker"]
