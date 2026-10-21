#!/bin/sh
# Runs INSIDE the Alpine container. Builds aria2c fully static from
# /src/src against the OpenSSL bind-mounted at /usr/local/openssl.
# Output: /src/out/aria2c (stripped)
set -e

apk add -q --no-cache build-base autoconf automake libtool pkgconfig \
    gettext-dev autoconf-archive perl ca-certificates \
    zlib-dev zlib-static c-ares-dev sqlite-dev sqlite-static \
    libxml2-dev libxml2-static expat-dev xz-dev xz-static linux-headers

cd /src/src
mkdir -p /src/logs /src/out

autoreconf -i > /src/logs/autoreconf.log 2>&1 || {
    tail -30 /src/logs/autoreconf.log
    exit 1
}

export PKG_CONFIG_PATH=/usr/local/openssl/lib64/pkgconfig
./configure ARIA2_STATIC=yes \
    --with-ca-bundle=/etc/ssl/certs/ca-certificates.crt \
    --with-openssl --with-libcares \
    --with-sqlite3 --with-libxml2 --with-zlib \
    --without-libssh2 --without-gnutls --without-libgcrypt --without-libnettle \
    --disable-nls > /src/logs/configure.log 2>&1 || {
    tail -30 /src/logs/configure.log
    exit 1
}

make -j"$(nproc)" > /src/logs/make.log 2>&1 || {
    tail -40 /src/logs/make.log
    exit 1
}

strip -o /src/out/aria2c src/aria2c
file /src/out/aria2c 2>/dev/null || true
