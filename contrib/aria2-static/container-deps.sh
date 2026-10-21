#!/bin/sh
# Runs INSIDE the Alpine container. Builds static OpenSSL into
# /src/openssl-root (DESTDIR of prefix /usr/local/openssl).
set -e

apk add -q --no-cache build-base perl linux-headers wget ca-certificates

cd /src
wget -q "https://github.com/openssl/openssl/releases/download/openssl-${OPENSSL_VER}/openssl-${OPENSSL_VER}.tar.gz"
tar xzf "openssl-${OPENSSL_VER}.tar.gz"
mv "openssl-${OPENSSL_VER}" openssl-src
cd openssl-src

./Configure linux-x86_64 no-shared no-module no-tests --prefix=/usr/local/openssl \
    > /src/logs/openssl-conf.log 2>&1
make -j"$(nproc)" > /src/logs/openssl-make.log 2>&1 || {
    tail -30 /src/logs/openssl-make.log
    exit 1
}
make install_sw DESTDIR=/src/openssl-root > /src/logs/openssl-install.log 2>&1

ls /src/openssl-root/usr/local/openssl/lib64/libcrypto.a
