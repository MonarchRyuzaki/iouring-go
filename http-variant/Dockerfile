FROM golang:alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY common/ ./common/
COPY http-variant/ ./http-variant/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /http-server ./http-variant

FROM alpine:3.19

WORKDIR /
COPY --from=builder /http-server /http-server

EXPOSE 8080
ENTRYPOINT ["/http-server"]
CMD ["-port", "8080", "-host", "0.0.0.0", "-metrics", "1"]
