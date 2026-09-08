FROM golang:1.24 AS builder

WORKDIR /src

COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/ ./

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/k8s-gpu-device-plugin .

FROM ubuntu:24.04

COPY --from=builder /out/k8s-gpu-device-plugin /usr/local/bin/k8s-gpu-device-plugin

ENTRYPOINT ["/usr/local/bin/k8s-gpu-device-plugin"]