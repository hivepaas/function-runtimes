# syntax=docker/dockerfile:1
# The Node.js runtime, built from the repository's root:
#
#   docker build -f runtimes/node24/Dockerfile -t fn-node24 .

FROM node:24-trixie-slim
RUN groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY runtimes/js/runtime/ /hivepaas/runtime/
RUN chmod 755 /hivepaas/runtime/main.mjs \
    && ln -s /hivepaas/runtime/main.mjs /usr/local/bin/hivepaas-runtime
ENV NODE_ENV=production
USER hivepaas
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]
