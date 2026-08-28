FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod main.go ./
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/benchwork .

FROM gcr.io/distroless/static-debian12:latest
COPY --from=builder /out/benchwork /benchwork
ENTRYPOINT ["/benchwork"]
