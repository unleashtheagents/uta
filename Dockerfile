# ghcr.io/unleashtheagents/uta — the uta CLI with the tools its native agent
# loop shells out to (sh, git, curl). Pure Go, CGO_ENABLED=0, so the runtime
# image is Alpine plus the static binary.
#
#   docker run --rm -it -e NEBIUS_API_KEY -v "$PWD":/work ghcr.io/unleashtheagents/uta run -g "summarize this repo"
FROM golang:1.27-alpine AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/unleashtheagents/uta/internal/version.Version=${VERSION} -X github.com/unleashtheagents/uta/internal/version.Commit=${COMMIT}" \
    -o /out/uta ./cmd/uta

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git curl bash
COPY --from=build /out/uta /usr/local/bin/uta
WORKDIR /work
ENV UTA_HOME=/uta-home
VOLUME ["/uta-home"]
ENTRYPOINT ["uta"]
CMD ["--help"]
