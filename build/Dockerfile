# Builder
FROM golang:1.26.2-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /app-binary ./cmd/api

# Runtime
FROM alpine:3.23
RUN apk add --no-cache tzdata ca-certificates wget
ENV TZ=Asia/Taipei
WORKDIR /app
RUN addgroup -S appgroup \
    && adduser -S appuser -G appgroup \
    && mkdir -p ./configs ./log \
    && chown -R appuser:appgroup /app
COPY --chown=appuser:appgroup --from=builder /app-binary .
COPY --chown=appuser:appgroup --from=builder /app/configs ./configs
EXPOSE 8080
USER appuser
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1
CMD ["./app-binary"]
