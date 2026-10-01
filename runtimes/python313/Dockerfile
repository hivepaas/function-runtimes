# syntax=docker/dockerfile:1
# The Python runtime, built from the repository's root:
#
#   docker build -f runtimes/python313/Dockerfile -t fn-python313 .

FROM ghcr.io/astral-sh/uv:0.9 AS uv

FROM python:3.13-slim-trixie
COPY --from=uv /uv /usr/local/bin/
RUN groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY runtimes/python313/runtime/ /hivepaas/runtime/
COPY runtimes/python313/hivepaas-runtime /usr/local/bin/
RUN python -m compileall -q /hivepaas/runtime
ENV VIRTUAL_ENV=/app/.venv \
    PATH=/app/.venv/bin:$PATH \
    PYTHONPATH=/hivepaas/runtime \
    PYTHONSAFEPATH=1 \
    PYTHONUNBUFFERED=1 \
    UV_PYTHON_DOWNLOADS=never \
    UV_NO_CACHE=1 \
    UV_COMPILE_BYTECODE=1
USER hivepaas
WORKDIR /app
RUN uv venv --quiet /app/.venv
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]
