FROM golang:alpine AS builder
ARG TARGETARCH
WORKDIR /app
ADD main.go .
RUN GOOS=linux GOARCH=$TARGETARCH go build -ldflags="-s -w" -o application main.go


FROM pascall/miiocli:v0.6.0
WORKDIR /app
COPY --from=builder /app/application .
CMD ["bash"]
