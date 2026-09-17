FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
# VERSION 注入到 /admin/api/version 与 /v1/health，便于确认线上跑的是哪个构建：
#   VERSION=$(git rev-parse --short HEAD) docker compose up -d --build
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags="-s -w -X cline-go-proxy/internal/app.Version=${VERSION}" \
      -o cline-proxy .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/cline-proxy .

EXPOSE 3457

VOLUME ["/app/data"]

ENV PORT=3457

ENTRYPOINT ["/app/cline-proxy"]
CMD ["-port", "3457"]
