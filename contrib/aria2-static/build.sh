#!/bin/sh
# Build a fully static aria2c (musl/Alpine, amd64) from the patched fork,
# so raria2's --aria2-session-size restart workaround is not needed.
#
# Usage: contrib/aria2-static/build.sh [output-dir]
#
# Output: <output-dir>/aria2c-static-linux-amd64 (+ .sha256)
# Note: keep the output dir separate from GoReleaser's dist/ - it is
# wiped by `goreleaser release --clean`. Default: aria2-dist/
#
# Env overrides:
#   ARIA2_REPO    git repository (default: patched fork)
#   ARIA2_REF     branch, tag or commit SHA (default: fix-stdin-blocking-1161)
#   OPENSSL_VER   OpenSSL release (default: 3.5.4)
#   ALPINE_IMAGE  build container (default: alpine:3.21)
#   WORK_DIR      build workspace (default: .aria2-build, cacheable)
#
# Requires: docker, git, sha256sum.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
OUT_DIR=${1:-aria2-dist}

ARIA2_REPO=${ARIA2_REPO:-https://github.com/smeinecke/aria2.git}
ARIA2_REF=${ARIA2_REF:-fix-stdin-blocking-1161}
OPENSSL_VER=${OPENSSL_VER:-3.5.4}
ALPINE_IMAGE=${ALPINE_IMAGE:-alpine:3.21}
WORK_DIR=${WORK_DIR:-.aria2-build}

mkdir -p "$OUT_DIR" "$WORK_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
WORK_DIR=$(cd "$WORK_DIR" && pwd)

SRC="$WORK_DIR/src"
OSSL_ROOT="$WORK_DIR/openssl-root"
mkdir -p "$WORK_DIR/logs" "$WORK_DIR/out"

# 1. aria2 sources at the requested ref (fetch by ref handles branch/tag/SHA).
if [ ! -d "$SRC/.git" ]; then
    git init -q "$SRC"
    git -C "$SRC" remote add origin "$ARIA2_REPO"
fi
git -C "$SRC" fetch -q --depth 1 origin "$ARIA2_REF"
# -f + clean -x: previous in-tree builds leave autoreconf output and
# (gitignored) object files behind, which can link stale code after a
# ref change
git -C "$SRC" checkout -q -f FETCH_HEAD
git -C "$SRC" clean -fdxq
echo "== aria2 source: $(git -C "$SRC" rev-parse --short HEAD) ($ARIA2_REF)"

# 2. OpenSSL, static, providers compiled in (no-module: a static binary
#    cannot dlopen provider modules). Cached in WORK_DIR between builds.
if [ ! -f "$OSSL_ROOT/usr/local/openssl/lib64/libcrypto.a" ]; then
    echo "== building OpenSSL $OPENSSL_VER (static)"
    docker run --rm \
        -e OPENSSL_VER="$OPENSSL_VER" \
        -v "$HERE:/scripts:ro" \
        -v "$WORK_DIR:/src" \
        "$ALPINE_IMAGE" sh /scripts/container-deps.sh
fi

# 3. aria2 static build. OpenSSL is bind-mounted at the prefix its .pc
#    files were generated for.
echo "== building aria2c (static)"
docker run --rm \
    -v "$HERE:/scripts:ro" \
    -v "$WORK_DIR:/src" \
    -v "$OSSL_ROOT/usr/local/openssl:/usr/local/openssl:ro" \
    "$ALPINE_IMAGE" sh /scripts/container-aria2.sh

# 4. Package.
cp "$WORK_DIR/out/aria2c" "$OUT_DIR/aria2c-static-linux-amd64"
(cd "$OUT_DIR" && sha256sum aria2c-static-linux-amd64 > aria2c-static-linux-amd64.sha256)

echo "== done: $OUT_DIR/aria2c-static-linux-amd64"
ls -lh "$OUT_DIR/aria2c-static-linux-amd64"
