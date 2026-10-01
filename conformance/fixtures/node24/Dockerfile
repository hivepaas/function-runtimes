# The fixture on the Node.js runtime, built as HivePaaS builds a function.
ARG RUNTIME
FROM ${RUNTIME}
COPY --chown=hivepaas:hivepaas . /app/
RUN hivepaas-runtime deps
