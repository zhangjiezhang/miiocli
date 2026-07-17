FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY main.go voice_websocket.go aliyun_tts.go ota_service.go ./
COPY dashboard.html ota.html websocket.html app.css ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -o gomiio .


FROM pascall/gomiio:29
WORKDIR /app
EXPOSE 8080
RUN rm -rf /usr/local/bin/gomiio:
COPY --from=builder /app/gomiio /usr/local/bin/gomiio
VOLUME /app
CMD ["gomiio", "--filePath", "/app/app.yaml", "--daily", "8"]
