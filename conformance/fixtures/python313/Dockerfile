# The fixture on the Python runtime, built as HivePaaS builds a function. Its
# library is packed into a wheel first, standing for one from an index, so that
# deps installs it without a network.
ARG RUNTIME
FROM ${RUNTIME}
COPY --chown=hivepaas:hivepaas . /app/
RUN python lib-src/make_wheel.py && hivepaas-runtime deps
