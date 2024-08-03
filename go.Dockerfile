FROM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY main.go main.go
COPY go.mod go.mod
COPY go.sum go.sum
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o gomiiocli main.go


FROM pascall/miiocli:v0.6.0
WORKDIR /app
COPY --from=builder /app/gomiiocli /usr/local/bin/gomiiocli
