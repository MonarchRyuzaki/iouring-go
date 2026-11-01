FROM golang:alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY common/ ./common/
COPY uring-variant/ ./uring-variant/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /uring-server ./uring-variant

FROM alpine:3.19

WORKDIR /
COPY --from=builder /uring-server /uring-server

EXPOSE 8080
ENTRYPOINT ["/uring-server"]
CMD ["-port", "8080"]
