# Patched static aria2 build

Builds a fully static `aria2c` (musl, linux/amd64) from
[smeinecke/aria2@`fix-stdin-blocking-1161`](https://github.com/smeinecke/aria2/tree/fix-stdin-blocking-1161),
which carries the fix for [aria2/aria2#1161](https://github.com/aria2/aria2/issues/1161):
URLs fed via `-i -` are dispatched as they arrive instead of at stdin EOF.

With this binary, raria2's `--aria2-session-size` restart workaround is
unnecessary - the built-in probe detects incremental stdin support
automatically.

## Usage

```sh
contrib/aria2-static/build.sh                   # produces aria2-dist/aria2c-static-linux-amd64
contrib/aria2-static/test-streaming.sh aria2-dist/aria2c-static-linux-amd64
```

`build.sh` runs everything inside an Alpine container (requires docker):

1. Clones `ARIA2_REF` of `ARIA2_REPO` into `$WORK_DIR/src`.
2. Builds static OpenSSL (`no-shared no-module` - provider modules cannot
   be dlopened in a static binary) into `$WORK_DIR/openssl-root`, unless
   already present. This directory is cached in CI.
3. Builds aria2 with `ARIA2_STATIC=yes` (OpenSSL, c-ares, sqlite3,
   libxml2, zlib; no libssh2/GnuTLS - `sftp://` is unsupported but unused
   by raria2), strips the result into `aria2-dist/`.

`test-streaming.sh` is the functional gate: it feeds one URL over a pipe
held open for 8s and fails unless the download lands within 4s. Run it
against any `aria2c` to check for the incremental-input fix.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ARIA2_REPO` | `smeinecke/aria2` fork | source repository |
| `ARIA2_REF` | `fix-stdin-blocking-1161` | branch/tag/SHA to build |
| `OPENSSL_VER` | `3.5.4` | OpenSSL release |
| `WORK_DIR` | `.aria2-build/` | workspace; `openssl-root` inside is cacheable |

## Release integration

`.github/workflows/release.yml` runs `build.sh` + `test-streaming.sh`
before GoReleaser on every `v*` tag, and the binary is attached to the
release via `release.extra_files` in `.goreleaser.yml`.
