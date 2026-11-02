# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go ./
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tibi-mcp .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/Olsenius/tibi-mcp"
COPY --from=build /out/tibi-mcp /tibi-mcp
ENV PORT=8080 MCP_TRANSPORT=http
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/tibi-mcp", "-healthcheck"]
ENTRYPOINT ["/tibi-mcp"]
