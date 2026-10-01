# syntax=docker/dockerfile:1
# The Go runtime, built from the repository's root. Its `build` target compiles
# a function and is published as function-runtime-go127-build; its `run` target
# runs what was compiled and is published as function-runtime-go127:
#
#   docker build -f runtimes/go127/Dockerfile --target build -t fn-go127-build .
#   docker build -f runtimes/go127/Dockerfile --target run -t fn-go127 .

ARG VERSION=1.0.0-dev

FROM golang:1.27-trixie AS build
ARG VERSION
RUN groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY go.mod /hivepaas/go/
COPY hivepaas/ /hivepaas/go/hivepaas/
COPY runtimes/go127/hivepaas-runtime /usr/local/bin/
ENV HP_RUNTIME_VERSION=v${VERSION} \
    GOTOOLCHAIN=local \
    GOCACHE=/home/hivepaas/.cache/go-build
USER hivepaas
WORKDIR /app
# One function built here leaves the runtime and the standard library it uses
# compiled in the cache, so that a function's build compiles only the function.
COPY --chown=hivepaas:hivepaas runtimes/go127/warmup/ /app/
RUN hivepaas-runtime build \
    && rm -rf /app/* /app/.hivepaas
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]

FROM debian:trixie-slim AS run
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY runtimes/go127/hivepaas-runtime /usr/local/bin/
USER hivepaas
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]
