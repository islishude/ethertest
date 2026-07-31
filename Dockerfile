# syntax=docker/dockerfile:1
FROM golang:1.26.5 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go install -trimpath -ldflags="-s -w" ./cmd/ethertest

FROM gcr.io/distroless/base-debian13:latest
COPY --from=build /go/bin/ethertest /usr/local/bin/ethertest
VOLUME [ "/data", "/state" ]
EXPOSE 8545 5052
ENTRYPOINT ["/usr/local/bin/ethertest"]
