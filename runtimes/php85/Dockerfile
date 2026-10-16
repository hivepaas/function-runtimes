# syntax=docker/dockerfile:1
# The PHP runtime, built from the repository's root. Its server is the Go
# runtime's own, which hands each call to PHP processes that keep the handler
# loaded:
#
#   docker build -f runtimes/php85/Dockerfile -t fn-php85 .

FROM golang:1.27-trixie AS server
WORKDIR /src
COPY go.mod ./
COPY hivepaas/ hivepaas/
COPY runtimes/php85/*.go runtimes/php85/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hivepaas-runtime ./runtimes/php85

FROM composer:2 AS composer

FROM mlocati/php-extension-installer:2 AS extensions

# PHP and its extensions are built in the official image, which keeps the
# compilers that build them; the runtime takes PHP alone, and the packages of
# the libraries it loads.
FROM php:8.5-cli-trixie AS php
COPY --from=extensions /usr/bin/install-php-extensions /usr/local/bin/
# The extensions a function may use; one it needs beyond them is not
# installed by Composer.
RUN install-php-extensions bcmath exif gd gmp intl mysqli opcache pdo_mysql pdo_pgsql pgsql redis sockets zip
RUN find /usr/local/bin/php /usr/local/lib/php/extensions -type f \( -name php -o -name '*.so' \) \
        -exec ldd {} + 2>/dev/null \
    | awk '$3 ~ /^\// { print $3 }' | sort -u | xargs realpath | xargs dpkg -S 2>/dev/null \
    | cut -d: -f1 | sort -u > /php-packages

FROM debian:trixie-slim
COPY --from=php /php-packages /tmp/php-packages
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates $(cat /tmp/php-packages) \
    && rm -rf /var/lib/apt/lists/* /tmp/php-packages
COPY --from=php /usr/local/bin/php /usr/local/bin/php
COPY --from=php /usr/local/lib/php/ /usr/local/lib/php/
COPY --from=php /usr/local/etc/php/ /usr/local/etc/php/
# Every extension loads, and says nothing as PHP starts.
RUN test -z "$(php -r '' 2>&1)" \
    && php -m | grep -qx intl && php -m | grep -qx redis && php -m | grep -qx gd && php -m | grep -qx pdo_pgsql
COPY --from=composer /usr/bin/composer /usr/local/bin/composer
RUN groupadd --gid 10001 hivepaas \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin hivepaas \
    && mkdir /app \
    && chown hivepaas:hivepaas /app
COPY runtimes/php85/php.ini /usr/local/etc/php/php.ini
COPY runtimes/php85/runtime/ /hivepaas/php/
COPY --from=server /hivepaas-runtime /usr/local/bin/hivepaas-runtime
ENV COMPOSER_NO_INTERACTION=1
USER hivepaas
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
    CMD ["hivepaas-runtime", "health"]
CMD ["hivepaas-runtime", "serve"]
