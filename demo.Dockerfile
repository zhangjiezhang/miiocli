FROM --platform=$BUILDPLATFORM golang:alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
ADD main.go .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o demo main.go


FROM --platform=$TARGETPLATFORM pascall/miiocli:v0.6.0
WORKDIR /app
COPY --from=builder /app/demo .
CMD ["bash"]
