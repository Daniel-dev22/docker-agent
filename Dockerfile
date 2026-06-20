# syntax=docker/dockerfile:1.7
#
# docker-agent: manages the host Docker daemon over /var/run/docker.sock (read-
# only container fleet in Phase 0; lifecycle + compose ops in later phases) and
# reports a live fleet to controller. Pure-Go (docker SDK + compose-v2 as a
# library in Phase 2) — NO docker CLI / buildx / compose binary in the image.
#
# Built BY build-agent (dogfood): the private agent-kit-go module is fetched with
# the GitHub App token build-agent provides as the `ghtoken` secret. A credential
# helper reads it from the tmpfs mount at fetch time, so the token never lands in
# .gitconfig or any layer.
FROM golang:1.26-alpine AS builder
ARG GOAMD64=v3
ENV GOAMD64=${GOAMD64}
WORKDIR /app
RUN apk add --no-cache git
# Bypass the public proxy for our private GitHub org so go mod download hits git
# directly (the credential helper supplies the token over HTTPS).
ENV GOPRIVATE=github.com/Daniel-dev22/*
COPY go.mod go.sum* ./
# required=true so a build without the token fails loudly rather than silently
# attempting an anonymous (404) fetch of the private module.
RUN --mount=type=secret,id=ghtoken,required=true \
    git config --global credential.helper '!f() { echo username=x-access-token; echo "password=$(cat /run/secrets/ghtoken)"; }; f' && \
    go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" -o /out/docker-agent .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata
COPY --from=builder /out/docker-agent /usr/local/bin/docker-agent
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD curl -sf http://localhost:8080/health/ready || exit 1
ENTRYPOINT ["docker-agent"]
