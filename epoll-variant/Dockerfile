FROM golang:alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY common/ ./common/
COPY epoll-variant/ ./epoll-variant/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /epoll-server ./epoll-variant

FROM alpine:3.19

WORKDIR /
COPY --from=builder /epoll-server /epoll-server

EXPOSE 8080
ENTRYPOINT ["/epoll-server"]
CMD ["-port", "8080", "-host", "0.0.0.0", "-metrics", "1"]
