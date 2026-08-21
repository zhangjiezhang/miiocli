FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY main.go kiwi_service.go voice_websocket.go voice_command.go codex_sessions.go aliyun_tts.go ota_service.go web_release_service.go ./
COPY internal ./internal
COPY dashboard.html ota.html webapp.html websocket.html codex.html app.css ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -o gomiio .

FROM pascall/gomiio:29
WORKDIR /app
EXPOSE 8080
RUN rm -f /usr/local/bin/gomiio
COPY --from=builder /app/gomiio /usr/local/bin/gomiio
VOLUME /app
CMD ["gomiio", "--filePath", "/app/app.yaml", "--daily", "8"]
