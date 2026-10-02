# The function runtime contract

What a function's handler can count on from its runtime, and what HivePaaS can
count on from the runtime's image. Every runtime of this repository keeps it, and
the conformance suite (`conformance/`) checks that each one does.

The contract has a version. A runtime image's major version is the contract's:
every `1.x.y` image keeps v1. A later contract is a new major, beside this one.

## v1

### The image

| | |
|---|---|
| Name | `ghcr.io/hivepaas/function-runtime-<runtime>:<version>` |
| Runtimes | `node24`, `python313`, `go127` |
| Go's build image | `ghcr.io/hivepaas/function-runtime-go127-build:<version>`. A Go function is compiled in this image, which holds the toolchain, and runs in `function-runtime-go127`, which does not. |
| Base | Debian 13 (trixie), slim; Go's build image is the full `golang` image |
| The function's code | `/app`, the working directory |
| The function's libraries | Node.js: `/app/node_modules`. Python: the virtual environment `/app/.venv`. Go: the module cache, in the build image. |
| The runtime's own code | `/hivepaas`, and the command `hivepaas-runtime` on the `PATH` |
| User | `hivepaas`, uid and gid 10001. The function never runs as root. |
| Port | 8080 (`HP_FN_PORT`) |
| Command | `hivepaas-runtime serve` |
| Health check | `hivepaas-runtime health`, as the image's `HEALTHCHECK` |

`hivepaas-runtime` has these commands:

| Command | What it does |
|---|---|
| `serve` | Answers HTTP until stopped. On `SIGTERM` it takes no new connection, waits for the running calls, at most one timeout, and exits. |
| `invoke` | Calls the handler once, with a request read on standard input, and exits. |
| `health` | Exits 0 when the server on this container answers its health check. |
| `deps` | Installs the function's libraries from its manifest in `/app`. Node.js: `package.json`, with `npm ci`, or `npm install` that makes `package-lock.json` when there is none. Python: `requirements.txt`, resolved into `requirements.lock` when there is none, then installed into `/app/.venv` with `uv`. Go, in the build image: the modules `go.mod` requires. Without a manifest it does nothing. |
| `build` | Go only, in the build image: compiles the function in `/app` with the runtime into `/app/.hivepaas/function`. It first tidies `go.mod` and `go.sum`: what the code imports is added, at the versions it resolves, and the runtime module is required at the image's version. In the build image, `serve` and `invoke` build first when the function is not built. |

A command exits 2 when it, its configuration or its input cannot be used, and
3 when the function cannot be loaded or built, with the reason on standard
error.

### Configuration

From the environment. The function's own variables are in the same environment.

| Variable | Meaning | Default |
|---|---|---|
| `HP_FN_ENTRYPOINT` | Node.js, Python: the file of the handler, relative to `/app` - for Node.js a `.js`, `.mjs` or `.cjs` file, or TypeScript's `.ts`, `.mts` or `.cts`. Go: the package's directory, relative to the module. | `index.js`, `main.py`, `.` |
| `HP_FN_HANDLER` | The handler's name. | `default` (Node.js), `handler` (Python), `Handle` (Go) |
| `HP_FN_TIMEOUT_MS` | How long one call may take. | `30000` |
| `HP_FN_MAX_CONCURRENCY` | Calls one instance takes at once. | `16` |
| `HP_FN_MAX_BODY_SIZE` | Bytes of a request body, and of a response body. | `6291456` (6 MiB) |
| `HP_FN_PORT` | The port `serve` listens on. | `8080` |
| `WEB_CONCURRENCY` | Python: how many processes `serve` runs, at most `HP_FN_MAX_CONCURRENCY`. | as many as the container's CPUs, below |

A value that is not a positive whole number stops the runtime before it serves.

For Go, the entrypoint and the handler are read by `build`: the handler is
compiled in.

### The handler

Node.js. The module may be ES or CommonJS; the handler is its default export, or
the export `HP_FN_HANDLER` names:

```js
export default async function (req, ctx) {
  return { status: 200, body: { hello: req.query.name } }
}
```

TypeScript, on Node.js. A `.ts`, `.mts` or `.cts` file runs as Node.js 24 runs
it: its types are removed as it loads, and are not checked. Only syntax that
can be removed is allowed - no `enum`, no `namespace` holding values, no
parameter properties, no `import x = require(...)`, no decorators: a file with
one cannot be loaded. An import names the file with its extension
(`./lib/util.ts`), and a type from another file is imported with
`import type`. `.mts` is an ES module, `.cts` CommonJS, and `.ts` follows
`package.json`'s `"type"`. `tsconfig.json` is not read, and a `.ts` file under
`node_modules` is not run.

Python. The handler may be a function or a coroutine. `/app` is first on the
module path, so the function's own modules import by name:

```python
def handler(req, ctx):
    return {"status": 200, "body": {"hello": req.query.get("name")}}
```

Python serves with several processes, each loading the handler itself: as
many as the container's CPUs - its CPU quota, rounded up, else the CPUs it may
run on - at most `HP_FN_MAX_CONCURRENCY`, and at most one per 128 MiB of its
memory limit; `WEB_CONCURRENCY` sets the number. What a module keeps in memory
is its process's; `HP_FN_MAX_CONCURRENCY` counts the calls of every process
together. A coroutine runs on its process's event loop: one that blocks holds
up the other calls of its process, and its timeout is answered only when it
gives the loop back. Blocking code belongs in a plain function, which runs in a
thread. `serve` says on standard error, as it starts, `hivepaas: serving with
4 processes`.

Go. A package of the function's module; the types are the package
`github.com/hivepaas/function-runtimes/hivepaas`:

```go
func Handle(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
	return hivepaas.JSON(200, map[string]string{"hello": req.Query.Get("name")})
}
```

### The request

| | Node.js | Python | Go |
|---|---|---|---|
| Method, in upper case | `req.method` | `req.method` | `req.Method` |
| Path, without the query, as sent (still percent-encoded) | `req.path` | `req.path` | `req.Path` |
| Query: the first value of each name | `req.query` | `req.query` | `req.Query.Get(name)` |
| Query: every value of each name | `req.queryAll` | `req.query_all` | `req.Query[name]` |
| Headers, by name in lower case | `req.headers` | `req.headers` | `req.Headers.Get(name)` |
| Body, as bytes | `req.body` (Buffer) | `req.body` (bytes) | `req.Body` |
| Body, as text | `req.text()` | `req.text()` | `req.Text()` |
| Body, as JSON | `req.json()` | `req.json()` | `req.JSON(&v)` |

A header sent several times is, in Node.js and Python, one value, its values
joined by `, `; in Go, `req.Headers.Get(name)` is its first value and
`req.Headers.Values(name)` every one. `host` is among the headers.

A call starts once its request has been received whole. A request whose body
does not arrive - the client went away, or took longer than `HP_FN_TIMEOUT_MS`
to send it - is not a call: the runtime closes the connection, the handler does
not see it and nothing is logged.

### The context

| | Node.js | Python | Go |
|---|---|---|---|
| The call's id | `ctx.requestId` | `ctx.request_id` | `hivepaas.RequestID(ctx)` |
| When the call times out | `ctx.deadline` (milliseconds since the epoch) | `ctx.deadline` (seconds since the epoch) | `ctx.Deadline()` |
| Aborted at the timeout | `ctx.signal` (an `AbortSignal`) | - | `ctx.Done()` |
| A log line carrying the id | `ctx.log(...)` | `ctx.log(...)` | `hivepaas.Log(ctx, ...)` |

The call's id is the request's `X-Request-Id` when it has one of 1 to 128
letters, digits, `.`, `_` or `-`; otherwise the runtime makes one. Every response
carries it in `X-Request-Id`.

### The response

Node.js and Python: an object (a dict) with any of `status`, `headers`, `body`;
or nothing. `headers` maps a name to a value, or to a list of values for a
header sent several times.

| Returned | Status | Body |
|---|---|---|
| nothing (`undefined`, `None`, a nil response) | 204 | none |
| no `status` | 200 | |
| a `status` that is not a whole number from 200 to 599 | 500 | the error of a failed call |
| `body` text | | as it is, `text/plain; charset=utf-8` |
| `body` bytes (Buffer, `bytes`) | | as they are, `application/octet-stream` |
| `body` anything else (an object, a list, a number) | | as JSON, `application/json` |

A `Content-Type` among the headers replaces the one the body would have. Go
builds its response with `hivepaas.JSON`, `hivepaas.Text`, or a `Response` of
its own, whose body without a `Content-Type` is `application/octet-stream`.

A response to a `HEAD` request, and one whose status is 204 or 304, is sent
without its body. The runtime sets `Content-Length` and `X-Request-Id` itself.

### What the runtime answers itself

| When | Status | Body |
|---|---|---|
| The handler raised an error, or returned what is not a response | 500 | `{"error": "internal error", "requestId": "…"}` |
| The call outlived `HP_FN_TIMEOUT_MS` | 504 | `{"error": "timeout", "requestId": "…"}` |
| `HP_FN_MAX_CONCURRENCY` calls are already running | 429, `Retry-After: 1` | `{"error": "too many requests", "requestId": "…"}` |
| The request's body is larger than `HP_FN_MAX_BODY_SIZE` | 413 | `{"error": "request too large", "requestId": "…"}` |
| The response's body is larger than `HP_FN_MAX_BODY_SIZE` | 500 | `{"error": "internal error", "requestId": "…"}` |
| The handler could not be loaded | 500 | `{"error": "internal error", "requestId": "…"}` |
| A path under `/_hivepaas/` that is not the runtime's | 404 | `{"error": "not found"}` |

The error itself is never sent: it is in the log. A call that times out is
answered at once, but it keeps its place among the running calls until the
handler returns, so the concurrency limit counts what really runs.

`serve` keeps an idle connection for 5 minutes, longer than a proxy in front of
it keeps one, so that the proxy is the one that closes it. A connection is kept
between calls: for HTTP/1.1 unless the request says `Connection: close`; for
HTTP/1.0 when the request says `Connection: keep-alive`, which the answer says
back. `serve` listens on IPv6 and IPv4 where the host has both, on IPv4
otherwise.

### Paths of the runtime

Paths under `/_hivepaas/` never reach the handler.

| Path | Answer |
|---|---|
| `/_hivepaas/health` | 200 `{"status": "ok"}` once the handler is loaded; 503 `{"status": "unavailable"}` when it could not be |

A handler that cannot be loaded does not stop the server: it answers its health
check with 503, so that an orchestrator keeps the previous version running.

### Logs

The runtime writes, on standard output, one JSON line per call:

```json
{"hp":"invocation","requestId":"…","method":"GET","path":"/","status":200,"durationMs":12.5,"outcome":"ok"}
```

`durationMs` is how long the handler ran, in milliseconds to the microsecond; 0
when the runtime answered without it. `path` is the path without its query, so
that what the query carries stays out of the log. `outcome` is one of `ok`,
`error`, `timeout`, `throttled`, `request_too_large`, `response_too_large`,
`not_loaded`. Paths of the runtime are not logged. A line the handler writes
with the context's logger is:

```json
{"hp":"log","requestId":"…","msg":"…"}
```

What the handler prints itself is passed through as it is. Python's processes
write to the same standard output: a line of more than 4 KiB may interleave
with one another process writes at the same moment. The runtime's own
errors - a handler that could not be loaded, an error a handler raised - go to
standard error, as text: a first line `hivepaas: call <id> failed: <error>`, or
`hivepaas: the handler could not be loaded: <error>`, then the stack where
there is one.

### Invoke

`hivepaas-runtime invoke` reads one request on standard input, as JSON:

```json
{"method": "POST", "path": "/hello", "query": {"name": ["Ada"]},
 "headers": {"content-type": ["application/json"]}, "body": "<base64>"}
```

Every field may be left out: the method is then `GET`, the path `/`, the body
empty. It calls the handler once, with the same limits and logs as `serve`, and
writes the result as the last line of standard output, after the marker
`#hivepaas-result `:

```
#hivepaas-result {"status":200,"headers":{"content-type":["application/json"]},"body":"<base64>","requestId":"…","durationMs":3.2,"outcome":"ok"}
```

`headers` are by name in lower case, with `x-request-id`, without
`content-length`. The call's id is the request's `x-request-id` header under the
same rule as `serve`. Every other line is the call's log; nothing is written
after the result, even by a handler still running past its timeout.

It exits 0 when it wrote a result, whatever the status; 2 when the request on
its input could not be read; 3 when the handler could not be loaded, or, in Go's
build image, the function could not be built. Exits 2 and 3 write no result.
