"""How a handler is loaded and called, and what its calls answer."""

import asyncio
import base64
import concurrent.futures
import importlib
import inspect
import json
import os
import re
import secrets
import sys
import threading
import time
import traceback
import urllib.parse
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field

from .wire import (  # noqa: F401 - the configuration is the runtime's too
    APP_DIR,
    RESULT_MARKER,
    Config,
    ConfigError,
    load_config,
    positive_whole_number,
    read_invoke_request,
)

# Longer than a proxy in front keeps an idle connection (Traefik: 90s).
IDLE_TIMEOUT_S = 300

_REQUEST_ID = re.compile(r"[A-Za-z0-9._-]{1,128}")
_TOKEN = re.compile(r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+")
_BAD_VALUE = re.compile(r"[\r\n\x00]")
_BODYLESS = (204, 304)


class CallError(Exception):
    """A call that failed; outcome says how."""

    def __init__(self, message: str, outcome: str = "error"):
        super().__init__(message)
        self.outcome = outcome


def load_handler(app_dir: str, cfg: Config) -> Callable:
    """The function cfg.handler names in the entrypoint's module. The module is
    imported by name, with app_dir first on the module path."""
    if app_dir in sys.path:
        sys.path.remove(app_dir)
    sys.path.insert(0, app_dir)
    module = importlib.import_module(os.path.normpath(cfg.entrypoint)[: -len(".py")].replace(os.sep, "."))
    handler = getattr(module, cfg.handler, None)
    if not callable(handler):
        raise CallError(f'{cfg.entrypoint} has no function "{cfg.handler}"')
    return handler


class Request:
    """The request a handler is given."""

    __slots__ = ("method", "path", "query", "query_all", "headers", "body")

    def __init__(self, method: str, path: str, query: dict, query_all: dict, headers: dict, body: bytes):
        self.method = method
        self.path = path
        self.query = query
        self.query_all = query_all
        self.headers = headers
        self.body = body

    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")

    def json(self):
        return json.loads(self.body)


class Context:
    """A call's id, when it times out (seconds since the epoch), and a logger
    whose lines carry the id."""

    __slots__ = ("request_id", "deadline", "_log")

    def __init__(self, request_id: str, deadline: float, log: Callable[[str], None]):
        self.request_id = request_id
        self.deadline = deadline
        self._log = log

    def log(self, *args) -> None:
        self._log(" ".join(str(arg) for arg in args))


def request_of(method: str, raw_path: str, header_pairs, body: bytes) -> Request:
    path, _, query_string = raw_path.partition("?")
    query_all: dict[str, list[str]] = {}
    for name, value in urllib.parse.parse_qsl(query_string, keep_blank_values=True):
        query_all.setdefault(name, []).append(value)
    headers: dict[str, str] = {}
    for name, value in header_pairs:
        key = name.lower()
        headers[key] = f"{headers[key]}, {value}" if key in headers else value
    query = {name: values[0] for name, values in query_all.items()}
    return Request(method.upper(), path, query, query_all, headers, body)


@dataclass
class Answer:
    status: int
    headers: dict = field(default_factory=dict)
    body: bytes = b""
    outcome: str = "ok"
    duration_ms: float = 0


def to_answer(value, max_body_size: int) -> Answer:
    """The answer to what a handler returned; a CallError when it is not a response."""
    if value is None:
        return Answer(204)
    if not isinstance(value, Mapping):
        raise CallError("the handler returned what is not a response: a dict with status, headers and body")
    status = value.get("status")
    if status is None:
        status = 200
    if type(status) is not int or not 200 <= status <= 599:
        raise CallError(f"the response's status {status!r} is not a whole number from 200 to 599")
    headers = _headers_of(value.get("headers"))
    body, content_type = _body_of(value.get("body"))
    if len(body) > max_body_size:
        raise CallError(
            f"the response's body of {len(body)} bytes is larger than HP_FN_MAX_BODY_SIZE, {max_body_size}",
            "response_too_large",
        )
    if status in _BODYLESS:
        return Answer(status, headers)
    if body and content_type and "content-type" not in headers:
        headers["content-type"] = content_type
    return Answer(status, headers, body)


def _headers_of(given) -> dict:
    if given is None:
        return {}
    if not isinstance(given, Mapping):
        raise CallError("the response's headers are not a dict")
    headers = {}
    for name, value in given.items():
        values = [str(one) for one in value] if isinstance(value, (list, tuple)) else [str(value)]
        if not isinstance(name, str) or not _TOKEN.fullmatch(name):
            raise CallError(f"the response's header {name!r} has a name that cannot be sent")
        if any(_BAD_VALUE.search(one) for one in values):
            raise CallError(f"the response's header {name!r} has a value that cannot be sent")
        headers[name.lower()] = values if isinstance(value, (list, tuple)) else values[0]
    return headers


def _body_of(body) -> tuple[bytes, str | None]:
    if body is None:
        return b"", None
    if isinstance(body, str):
        return body.encode("utf-8"), "text/plain; charset=utf-8"
    if isinstance(body, (bytes, bytearray, memoryview)):
        return bytes(body), "application/octet-stream"
    try:
        text = json.dumps(body, separators=(",", ":"), ensure_ascii=False, allow_nan=False)
    except (TypeError, ValueError) as error:
        raise CallError(f"the response's body cannot be JSON: {error}") from None
    return text.encode("utf-8"), "application/json"


def failure(status: int, message: str, request_id: str, outcome: str) -> Answer:
    """An answer the runtime makes itself."""
    return Answer(status, {"content-type": "application/json"},
                  _compact({"error": message, "requestId": request_id}).encode(), outcome)


def _own(status: int, value: dict) -> Answer:
    return Answer(status, {"content-type": "application/json"}, _compact(value).encode())


def request_id_of(sent: str | None) -> str:
    """The request's own id when the contract accepts it, else a new one."""
    return sent if sent and _REQUEST_ID.fullmatch(sent) else secrets.token_hex(16)


def _compact(value) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False)


def _describe(error: BaseException) -> str:
    if isinstance(error, CallError):
        return str(error)
    summary = "".join(traceback.format_exception_only(error)).strip()
    return f"{summary}\n" + "".join(traceback.format_exception(error)).rstrip()


def _is_async(fn) -> bool:
    return inspect.iscoroutinefunction(fn) or inspect.iscoroutinefunction(type(fn).__call__)


async def _awaited(awaitable):
    return await awaitable


def _printer(stream_name: str) -> Callable[[str], None]:
    lock = threading.Lock()

    def write(text: str) -> None:
        with lock:
            stream = getattr(sys, stream_name)
            stream.write(text + "\n")
            stream.flush()

    return write


class LocalPlaces:
    """The running calls of one process."""

    def __init__(self):
        self._lock = threading.Lock()
        self._count = 0

    def take(self, limit: int) -> bool:
        with self._lock:
            if self._count >= limit:
                return False
            self._count += 1
            return True

    def give(self) -> None:
        with self._lock:
            self._count -= 1


class Runtime:
    """Runs one handler under the configured limits. places counts the running
    calls: this process's, or, given, every worker's of an instance."""

    def __init__(self, handler, cfg: Config, out=None, err=None, places=None):
        self.handler = handler  # None when it could not be loaded
        self.cfg = cfg
        self.stopping = False
        self.closed = False
        self._places = places or LocalPlaces()
        self._write = out or _printer("stdout")
        self._error = err or _printer("stderr")
        self._is_async = handler is not None and _is_async(handler)
        self._lock = threading.Lock()
        self._pool = None  # made at the first call of a function
        self._loop = None  # made at the first call of a coroutine by call, which invoke uses

    @classmethod
    def load(cls, cfg: Config, out=None, err=None, places=None) -> "Runtime":
        """A runtime for the function in /app. A handler that cannot be loaded is logged."""
        try:
            handler = load_handler(APP_DIR, cfg)
        except (Exception, SystemExit) as error:
            (err or _printer("stderr"))(f"hivepaas: the handler could not be loaded: {_describe(error)}")
            handler = None
        return cls(handler, cfg, out, err, places)

    def close(self) -> None:
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
        if self._loop is not None:
            self._loop.call_soon_threadsafe(self._loop.stop)

    def _executor(self) -> concurrent.futures.ThreadPoolExecutor:
        with self._lock:
            if self._pool is None:
                self._pool = concurrent.futures.ThreadPoolExecutor(self.cfg.max_concurrency,
                                                                   thread_name_prefix="hivepaas-call")
            return self._pool

    def _own_loop(self) -> asyncio.AbstractEventLoop:
        """The loop call runs coroutines on, in a thread of its own."""
        with self._lock:
            if self._loop is None:
                self._loop = asyncio.new_event_loop()
                threading.Thread(target=self._loop.run_forever, daemon=True, name="hivepaas-loop").start()
            return self._loop

    def out(self, line: str) -> None:
        if not self.closed:
            self._write(line)

    def log_line(self, value: dict) -> None:
        self.out(_compact(value))

    def log_invocation(self, request_id: str, method: str, path: str, answer: Answer) -> None:
        self.log_line({"hp": "invocation", "requestId": request_id, "method": method, "path": path,
                       "status": answer.status, "durationMs": answer.duration_ms, "outcome": answer.outcome})

    def try_reserve(self) -> bool:
        """Takes a place among the running calls, when there is one."""
        return self._places.take(self.cfg.max_concurrency)

    def _release(self) -> None:
        self._places.give()

    def _context(self, request_id: str, timeout: float) -> Context:
        return Context(request_id, time.time() + timeout,
                       lambda msg: self.log_line({"hp": "log", "requestId": request_id, "msg": msg}))

    def call(self, request: Request, request_id: str) -> Answer:
        """Runs the handler on request within the timeout, blocking: invoke's
        way. The caller took a place for it; the place is given back when the
        handler returns, which may be after the call was answered."""
        start = time.perf_counter()
        timeout = self.cfg.timeout_ms / 1000
        ctx = self._context(request_id, timeout)
        if self._is_async:
            future = asyncio.run_coroutine_threadsafe(self._run_async(request, ctx), self._own_loop())
        else:
            future = self._executor().submit(self._run_sync, request, ctx)
        try:
            future.result(timeout=timeout)
        except (KeyboardInterrupt, SystemExit):
            raise
        except BaseException:  # what the handler did is read from the future below
            pass
        if not future.done():
            future.cancel()  # a coroutine is cancelled; a function runs on, in its place
            return self._timed(failure(504, "timeout", request_id, "timeout"), start)
        return self._settled(future, request_id, start)

    async def call_async(self, request: Request, request_id: str) -> Answer:
        """Runs the handler on request within the timeout: serve's way. A
        coroutine runs on the running loop, cancelled at the timeout - one that
        blocks holds the loop up; a function runs in a thread, and on past its
        timeout. The place the caller took is given back as call's is."""
        start = time.perf_counter()
        timeout = self.cfg.timeout_ms / 1000
        ctx = self._context(request_id, timeout)
        try:
            async with asyncio.timeout(timeout) as limit:
                if self._is_async:
                    value = await self._run_async(request, ctx)
                else:
                    value = await asyncio.get_running_loop().run_in_executor(
                        self._executor(), self._run_sync, request, ctx)
        except TimeoutError as error:
            if limit.expired():
                return self._timed(failure(504, "timeout", request_id, "timeout"), start)
            return self._failed(request_id, error, start)  # the handler's own
        except asyncio.CancelledError:
            raise
        except BaseException as error:
            return self._failed(request_id, error, start)
        try:
            answer = to_answer(value, self.cfg.max_body_size)
        except CallError as error:
            return self._failed(request_id, error, start)
        return self._timed(answer, start)

    def _settled(self, future, request_id: str, start: float) -> Answer:
        """The answer to a call whose handler is done, from its future."""
        if future.cancelled():
            return self._failed(request_id, CallError("the handler was cancelled"), start)
        error = future.exception()
        if error is not None:
            return self._failed(request_id, error, start)
        try:
            answer = to_answer(future.result(), self.cfg.max_body_size)
        except CallError as error:
            return self._failed(request_id, error, start)
        return self._timed(answer, start)

    def _run_sync(self, request: Request, ctx: Context):
        try:
            value = self.handler(request, ctx)
            if inspect.isawaitable(value):  # a function that returned a coroutine
                value = asyncio.run(_awaited(value))
            return value
        finally:
            self._release()

    async def _run_async(self, request: Request, ctx: Context):
        try:
            return await self.handler(request, ctx)
        finally:
            self._release()

    def _failed(self, request_id: str, error: BaseException, start: float) -> Answer:
        self._error(f"hivepaas: call {request_id} failed: {_describe(error)}")
        outcome = error.outcome if isinstance(error, CallError) else "error"
        return self._timed(failure(500, "internal error", request_id, outcome), start)

    @staticmethod
    def _timed(answer: Answer, start: float) -> Answer:
        answer.duration_ms = round((time.perf_counter() - start) * 1000, 3)
        return answer

    def own_path(self, path: str) -> Answer:
        if path == "/_hivepaas/health":
            return _own(503, {"status": "unavailable"}) if self.handler is None else _own(200, {"status": "ok"})
        return _own(404, {"error": "not found"})

    def invoke(self, data: bytes) -> int:
        """Calls the handler once with the request in data; returns the exit code."""
        if self.handler is None:
            return 3
        try:
            method, path, raw_path, header_pairs, body = read_invoke_request(data)
        except ValueError as error:
            self._error(f"hivepaas: {error}")
            return 2
        request_id = request_id_of(next((v for n, v in header_pairs if n.lower() == "x-request-id"), None))
        if len(body) > self.cfg.max_body_size:
            answer = failure(413, "request too large", request_id, "request_too_large")
        else:
            self.try_reserve()
            answer = self.call(request_of(method, raw_path, header_pairs, body), request_id)
        self.log_invocation(request_id, method.upper(), path, answer)
        headers = {name: value if isinstance(value, list) else [value] for name, value in answer.headers.items()}
        headers["x-request-id"] = [request_id]
        self.out(RESULT_MARKER + _compact({
            "status": answer.status, "headers": headers, "body": base64.b64encode(answer.body).decode(),
            "requestId": request_id, "durationMs": answer.duration_ms, "outcome": answer.outcome,
        }))
        self.closed = True  # nothing after the result, even from a handler still running
        return 0
