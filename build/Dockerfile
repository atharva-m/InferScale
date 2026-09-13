# syntax=docker/dockerfile:1.9
ARG GO_IMAGE=golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a

FROM ${GO_IMAGE} AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG COMMAND
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "${COMMAND}" && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/inferscale "./cmd/${COMMAND}"

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/inferscale /usr/local/bin/inferscale
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/inferscale"]
