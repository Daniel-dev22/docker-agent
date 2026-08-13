# syntax=docker/dockerfile:1.7
#
# docker-agent manages the host Docker daemon over a mounted /var/run/docker.sock:
# container lifecycle, compose project ops, image-update checks and the
# health-gated stack update. Pure Go (the docker SDK plus compose as a library)
# — there is NO docker CLI, buildx or compose binary in the image.
#
# BUILD REQUIREMENT: github.com/Daniel-dev22/agent-kit-go is a private module, so
# `go mod download` needs a GitHub token supplied as a buildx secret:
#
#   docker build --secret id=ghtoken,src=<file-with-token> .
#
# A credential helper reads it from the tmpfs mount at fetch time, so the token
# never lands in .gitconfig or in any image layer. `required=true` makes a build
# without one fail loudly instead of silently attempting an anonymous (404) fetch.
# Once agent-kit-go is public, the GOPRIVATE line and the secret mount can go.
FROM golang:1.26-alpine AS builder
ARG GOAMD64=v3
ENV GOAMD64=${GOAMD64}
WORKDIR /app
RUN apk add --no-cache git
# Bypass the public module proxy for the private org so `go mod download` hits
# git directly (the credential helper supplies the token over HTTPS).
ENV GOPRIVATE=github.com/Daniel-dev22/*
COPY go.mod go.sum* ./
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
