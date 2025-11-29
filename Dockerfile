FROM golang:1.24 AS builder
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go env -w GOPROXY=${GOPROXY} && go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies

FROM debian:bookworm-slim AS runtime
RUN apt-get update \ 
    && apt-get install -y --no-install-recommends ca-certificates \ 
    && rm -rf /var/lib/apt/lists/* \ 
    && useradd -r -u 10001 easy
WORKDIR /app
COPY --from=builder /src/easy-proxies /usr/local/bin/easy-proxies
COPY config.example.yaml /etc/easy-proxies/config.yaml
# Pool mode: 2323, Management: 9091, Multi-port mode: 24000-24100
EXPOSE 2323 9091 24000-24100
USER easy
ENTRYPOINT ["/usr/local/bin/easy-proxies"]
CMD ["--config", "/etc/easy-proxies/config.yaml"]
