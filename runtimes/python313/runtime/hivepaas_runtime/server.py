"""serve: the HTTP server."""

import http.server
import signal
import sys
import threading
import time

from .runtime import IDLE_TIMEOUT_S, Answer, Runtime, failure, request_id_of, request_of

_TOO_LARGE = object()
_BODYLESS = (204, 304)
# Headers the runtime sets itself.
_OWN_HEADERS = ("content-length", "transfer-encoding", "x-request-id")


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        if not isinstance(sys.exc_info()[1], (ConnectionError, TimeoutError)):  # a client that went away
            super().handle_error(request, client_address)


def serve(runtime: Runtime) -> int:
    """Answers until SIGTERM, then lets the running calls finish, for at most one timeout."""
    server = _Server(("", runtime.cfg.port), _handler_class(runtime))
    threading.Thread(target=server.serve_forever, daemon=True, name="hivepaas-server").start()
    stop = threading.Event()
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: stop.set())
    while not stop.wait(1):
        pass
    runtime.stopping = True
    server.shutdown()
    server.server_close()
    deadline = time.monotonic() + runtime.cfg.timeout_ms / 1000
    while runtime.answering > 0 and time.monotonic() < deadline:
        time.sleep(0.05)
    return 0


def _handler_class(runtime: Runtime):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        timeout = IDLE_TIMEOUT_S
        # Headers and body go out in two writes; with Nagle's algorithm the
        # second waits for the client's delayed ACK, some 40 ms.
        disable_nagle_algorithm = True

        def __getattr__(self, name):
            if name.startswith("do_"):  # every method reaches the function
                return self._answer
            raise AttributeError(name)

        def log_message(self, format, *args):  # the runtime logs its own lines
            pass

        def send_response(self, code, message=None):  # without a Server header
            self.send_response_only(code, message)
            self.send_header("Date", self.date_time_string())

        def _answer(self):
            runtime.begin_answer()
            try:
                self._answer_one()
            finally:
                runtime.end_answer()

        def _answer_one(self):
            request_id = request_id_of(self.headers.get("x-request-id"))
            path = self.path.partition("?")[0]
            if path.startswith("/_hivepaas/"):
                self._send(request_id, runtime.own_path(path))
                return
            method = self.command.upper()

            def answer(a: Answer, close: bool = False) -> None:
                runtime.log_invocation(request_id, method, path, a)
                self._send(request_id, a, close)

            try:
                body = self._read_body()
            except (OSError, ValueError):
                self.close_connection = True  # the body did not arrive: not a call
                return
            if body is _TOO_LARGE:
                # What is left of the body is not read: the connection closes.
                answer(failure(413, "request too large", request_id, "request_too_large"), close=True)
            elif runtime.handler is None:
                answer(failure(500, "internal error", request_id, "not_loaded"))
            elif not runtime.try_reserve():
                throttled = failure(429, "too many requests", request_id, "throttled")
                throttled.headers["retry-after"] = "1"
                answer(throttled)
            else:
                answer(runtime.call(request_of(self.command, self.path, self.headers.items(), body), request_id))

        def _read_body(self):
            limit = runtime.cfg.max_body_size
            self.connection.settimeout(runtime.cfg.timeout_ms / 1000)
            try:
                if "chunked" in self.headers.get("transfer-encoding", "").lower():
                    return self._read_chunked(limit)
                length = int(self.headers.get("content-length") or 0)
                if length < 0:
                    raise ValueError("a negative content length")
                if length > limit:
                    return _TOO_LARGE
                body = self.rfile.read(length)
                if len(body) < length:
                    raise ConnectionError("the client went away")
                return body
            finally:
                self.connection.settimeout(IDLE_TIMEOUT_S)

        def _read_chunked(self, limit: int):
            chunks, size = [], 0
            while True:
                line = self.rfile.readline(65537)
                if not line.endswith(b"\n"):
                    raise ConnectionError("a chunk's size did not arrive")
                chunk_size = int(line.split(b";", 1)[0].strip(), 16)
                if chunk_size == 0:
                    while self.rfile.readline(65537) not in (b"\r\n", b"\n", b""):
                        pass  # trailers
                    return b"".join(chunks)
                size += chunk_size
                if size > limit:
                    return _TOO_LARGE
                chunk = self.rfile.read(chunk_size)
                if len(chunk) < chunk_size or self.rfile.read(2) != b"\r\n":
                    raise ConnectionError("a chunk did not arrive")
                chunks.append(chunk)

        def _send(self, request_id: str, answer: Answer, close: bool = False) -> None:
            self.send_response(answer.status)
            for name, value in answer.headers.items():
                if name in _OWN_HEADERS:
                    continue
                for one in value if isinstance(value, list) else [value]:
                    self.send_header(name, one)
            self.send_header("x-request-id", request_id)
            if answer.status not in _BODYLESS:
                self.send_header("content-length", str(len(answer.body)))
            if close or runtime.stopping:
                self.send_header("connection", "close")
            self.end_headers()
            if self.command != "HEAD" and answer.status not in _BODYLESS and answer.body:
                self.wfile.write(answer.body)

    return Handler
