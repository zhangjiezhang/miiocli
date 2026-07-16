FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY main.go voice_websocket.go aliyun_tts.go ./
COPY dashboard.html dashboard.html
COPY go.mod go.mod
COPY go.sum go.sum
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o gomiio .


FROM pascall/gomiio:29
WORKDIR /app
EXPOSE 8080
RUN rm -rf /usr/local/bin/gomiio:
COPY --from=builder /app/gomiio /usr/local/bin/gomiio
VOLUME /app
CMD ["gomiio", "--filePath", "/app/app.yaml", "--daily", "8"]
