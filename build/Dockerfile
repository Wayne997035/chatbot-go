# Builder
FROM --platform=linux/amd64 golang:1.24.1-alpine3.21 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o /app-binary ./cmd/api

# Runtime
FROM --platform=linux/amd64 alpine:3.21
RUN apk add --no-cache tzdata ca-certificates
ENV TZ=Asia/Taipei
WORKDIR /app
RUN mkdir -p ./config ./log
COPY --from=builder /app-binary .
COPY --from=builder /app/configs ./configs
EXPOSE 8080
CMD ["./app-binary"]
