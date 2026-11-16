# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency definitions
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary without CGO
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/order-api ./cmd/order-api

# Production minimal runtime stage
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -g 10001 -S appgroup \
    && adduser -u 10001 -S appuser -G appgroup

WORKDIR /app
COPY --from=builder /app/order-api /app/order-api

USER 10001:10001

EXPOSE 8000

ENTRYPOINT ["/app/order-api"]
