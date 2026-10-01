# The fixture on the Go runtime, built as HivePaaS builds a function: compiled
# in the build image, run in the runtime image. The `source` stage is the
# function before it is built, as a test run has it.
ARG BUILD_RUNTIME
ARG RUNTIME

FROM ${BUILD_RUNTIME} AS source
COPY --chown=hivepaas:hivepaas . /app/
RUN hivepaas-runtime deps

FROM source AS built
RUN hivepaas-runtime build

FROM ${RUNTIME}
COPY --from=built --chown=hivepaas:hivepaas /app /app
