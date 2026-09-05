FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go test ./... && go build -trimpath -ldflags="-s -w" -o /storage-reader .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app && mkdir /data && chown app:app /data
USER app
WORKDIR /app
COPY --from=build /storage-reader /app/storage-reader
ENV DATA_DIR=/data LISTEN_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/storage-reader"]
