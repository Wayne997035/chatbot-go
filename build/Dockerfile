# Builder
FROM --platform=linux/amd64 golang:1.26.1-alpine3.21 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /app-binary ./cmd/api

# Runtime
FROM --platform=linux/amd64 alpine:3.21
RUN apk add --no-cache tzdata ca-certificates wget
ENV TZ=Asia/Taipei
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p ./configs ./log && chown -R appuser:appgroup /app
COPY --from=builder /app-binary .
COPY --from=builder /app/configs ./configs
RUN chown -R appuser:appgroup /app
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1
CMD ["./app-binary"]
