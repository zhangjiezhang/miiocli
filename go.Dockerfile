FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY main.go main.go
COPY go.mod go.mod
COPY go.sum go.sum
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o gomiio main.go


FROM pascall/miiocli:v0.6.0
WORKDIR /app
EXPOSE 8080
COPY --from=builder /app/gomiio /usr/local/bin/gomiio
VOLUME /app
CMD ["gomiio", "--filePath", "/app/app.yaml", "--daily", "8"]
