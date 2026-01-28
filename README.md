# raria2

A wrapper for [aria2](https://aria2.github.io/) to mirror open directories.

This CLI tool tries to emulate the same behavior of `wget --recursive`, but with a couple of filters,
checks and caching and by using aria2c to perform the download of resources.

## Compile

```
go build .
```

## Usage

```
Usage: raria2 [--output OUTPUT] [--dry-run] [--max-connection-per-server CONNECTIONS] [--max-concurrent-downloads DOWNLOADS] [--max-depth DEPTH] [--accept EXT] [--reject EXT] [--accept-path PATTERN] [--reject-path PATTERN] [--visited-cache FILE] [--http-timeout DURATION] URL [-- ARIA2_OPTS...]

Positional arguments:
  URL                    The URL from where to fetch the resources from
  ARIA2_OPTS             Options forwarded to aria2c after the URL (use -- before them if
                         they look like flags)

Options:
  --output OUTPUT, -o OUTPUT
                         Output directory. If omitted, raria2 mirrors into
                         <host>/<path>/ derived from the URL (similar to wget)
  --dry-run, -d          Dry Run [default: false]
  --max-connection-per-server, -x
                         Parallel connections per download [default: 5]
  --max-concurrent-downloads, -j
                         Maximum concurrent downloads [default: 5]
  --max-depth DEPTH      Maximum HTML depth to crawl (-1 for unlimited) [default: -1]
  --accept EXT           Comma-separated list(s) of extensions to include (no dot, case-insensitive)
  --reject EXT           Comma-separated list(s) of extensions to exclude
  --accept-path PATTERN  Path glob (default) or regex:<expr> that must match to crawl/download
  --reject-path PATTERN  Path glob or regex to skip
  --visited-cache FILE   Persist visited URLs to this file so interrupted runs can resume
  --http-timeout DURATION
                         HTTP client timeout as Go duration string (e.g. 30s, 2m) [default: 30s]
  --help, -h             display this help and exit
```


## Example

```
# dry run mirroring into host/path structure automatically
raria2 -d 'https://proof.ovh.net/files/' -- --max-download-limit=1M

# explicitly setting output directory and concurrency knobs
raria2 -d -o output -x 10 -j 8 'https://mirror.nforce.com/pub/speedtests/' -- --max-download-limit=1M

# customize the HTTP timeout (here: 2 minutes)
raria2 --http-timeout=2m 'https://example.com/pub/'

# limit crawl depth to first directory level
raria2 --max-depth=1 'https://example.com/pub/'

# only download .iso files inside /iso/ paths and persist visited cache
raria2 --accept=iso --accept-path='glob:/iso/**' --visited-cache=visited.txt 'https://mirror.example.com/'
```
