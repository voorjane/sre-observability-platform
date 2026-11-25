FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /alert-webhook ./cmd/alert-webhook && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /alert-webhook /alert-webhook
COPY --from=builder --chown=65532:65532 /data /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/alert-webhook"]
