# syntax=docker/dockerfile:1
# The Bun runtime, built from the repository's root. It runs the JavaScript
# runtime's code, which Node.js runs too, on Bun:
#
#   docker build -f runtimes/bun1/Dockerfile -t fn-bun1 .

FROM oven/bun:1.4.2-slim
RUN groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY runtimes/js/runtime/ /hivepaas/runtime/
COPY runtimes/bun1/hivepaas-runtime /usr/local/bin/
ENV NODE_ENV=production
USER hivepaas
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]
