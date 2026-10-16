# The fixture on the PHP runtime, built as HivePaaS builds a function. Its
# library is a Composer path repository, standing for a package from Packagist,
# so that deps installs it without a network.
ARG RUNTIME
FROM ${RUNTIME}
COPY --chown=hivepaas:hivepaas . /app/
RUN hivepaas-runtime deps
