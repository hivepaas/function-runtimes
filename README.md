# function-runtimes

The runtimes HivePaaS runs functions with: one per language, each an image that
turns a handler into a server.

| Runtime | Language | Image |
|---|---|---|
| `node24` | Node.js 24 | `ghcr.io/hivepaas/function-runtime-node24` |
| `python313` | Python 3.13 | `ghcr.io/hivepaas/function-runtime-python313` |
| `go127` | Go 1.27 | `ghcr.io/hivepaas/function-runtime-go127`, compiled in `function-runtime-go127-build` |

What a handler can count on, and what the images promise, is the contract:
[CONTRACT.md](CONTRACT.md). An image's major version is the contract's version.

## Layout

- `hivepaas/`: the Go package a Go handler is written against, and
  `hivepaas/serve`, the Go runtime itself.
- `runtimes/<runtime>/`: each runtime's Dockerfile and code.
- `conformance/`: the suite every image passes, in `serve` and in `invoke`, with
  a fixture function per runtime in `conformance/fixtures/<runtime>/`.

## Trying a runtime

```bash
docker build -t fn-node24 -f runtimes/node24/Dockerfile .
mkdir -p /tmp/fn && cat > /tmp/fn/index.js <<'JS'
module.exports = async (req) => ({ body: { hello: req.query.name ?? "world" } })
JS
docker run --rm -p 8080:8080 -v /tmp/fn:/app fn-node24
curl 'localhost:8080/?name=Ada'
```

## Tests

```bash
go test ./...                                    # the Go packages
go test -tags conformance ./conformance/         # every runtime image, with docker
RUNTIMES=node24 go test -tags conformance ./conformance/
```

The Node.js and Python runtimes have unit tests of their own, run in their base
images:

```bash
docker run --rm -v "$PWD:/src:ro" -w /src node:24-trixie-slim node --test 'runtimes/node24/test/*.test.mjs'
docker run --rm -v "$PWD:/src:ro" -w /src -e PYTHONPATH=/src/runtimes/python313/runtime \
  python:3.13-slim-trixie python -m unittest discover -s runtimes/python313/tests
```

## Releasing

A tag `vX.Y.Z` on `main` builds every image for amd64 and arm64, pushes
`function-runtime-<runtime>:X.Y.Z` (and `:X` for a release that is not a
prerelease) to GHCR, and drafts a GitHub Release listing each image by digest.
Those lines go into HivePaaS's `release.json`, under `functionRuntimes`. The
tag's major is the contract's: a change that breaks the contract is a new major,
with its own section in [CONTRACT.md](CONTRACT.md).

## License

Apache License 2.0.
