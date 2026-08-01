# syntax=docker/dockerfile:1
FROM golang:1.26.5 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
	go install -trimpath -ldflags="-s -w" ./cmd/ethertest
RUN mkdir -p /runtime/data /runtime/state

FROM gcr.io/distroless/base-debian13:nonroot
COPY --from=build /go/bin/ethertest /usr/local/bin/ethertest
COPY --from=build --chown=65532:65532 /runtime/data /data
COPY --from=build --chown=65532:65532 /runtime/state /state
VOLUME [ "/data", "/state" ]
EXPOSE 8545
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ethertest"]
