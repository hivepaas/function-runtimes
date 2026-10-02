"""serve: the HTTP server, on asyncio, one per process. A connection reads
what arrives into a buffer and takes requests from it; a call runs as a task of
its own, one at a time per connection."""

import asyncio
import email.utils
import http
import re
import signal
import socket
import time

from .call import CALL_HEADER
from .runtime import IDLE_TIMEOUT_S, Answer, Runtime, failure, request_id_of, request_of

# The head of a request - its line and its headers - is read whole within this
# long of its first byte, and is at most HEAD_MAX bytes.
HEAD_TIMEOUT_S = 10
HEAD_MAX = 65536
BACKLOG = 1024
# How often the connections' deadlines are looked at.
_SWEEP_S = 0.25
# A chunk's size line, or a trailer, longer than this cannot be read.
_LINE_MAX = 8192

_BODYLESS = (204, 304)
# Headers the runtime sets itself.
_OWN_HEADERS = ("content-length", "transfer-encoding", "x-request-id", "connection")
_TOKEN = re.compile(r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+")
_REASONS = {status.value: status.phrase for status in http.HTTPStatus}
_TRAILERS = -1  # what a chunked body's reader expects after its last chunk


class BadRequest(Exception):
    """A request that cannot be read: answered with status, and the connection closed."""

    def __init__(self, status: int = 400):
        super().__init__(status)
        self.status = status


def listen(port: int) -> socket.socket:
    """A socket listening on port, IPv6 and IPv4; IPv4 alone where the host has no IPv6."""
    try:
        sock = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
        try:
            sock.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(("::", port))
        except OSError:
            sock.close()
            raise
    except OSError:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(("0.0.0.0", port))
    sock.listen(BACKLOG)
    sock.setblocking(False)
    return sock


def serve(runtime: Runtime, sock: socket.socket) -> int:
    """Answers on sock until SIGTERM, then lets the running calls finish, for at most one timeout."""

    async def main() -> int:
        server = Server(runtime)
        await server.start(sock)
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for signum in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(signum, stop.set)
        await stop.wait()
        await server.stop()
        return 0

    return asyncio.run(main())


class Server:
    """The connections of one process, and the calls they carry."""

    def __init__(self, runtime: Runtime):
        self.runtime = runtime
        self.connections: set[_Connection] = set()
        self.busy = 0  # requests being answered
        self._server = None
        self._sweeper = None
        self._date = (0, "")

    async def start(self, sock: socket.socket) -> None:
        loop = asyncio.get_running_loop()
        self._server = await loop.create_server(lambda: _Connection(self), sock=sock)
        self._sweeper = loop.call_later(_SWEEP_S, self._sweep)

    async def stop(self) -> None:
        """Takes no new connection, closes the idle ones, and waits for the
        running calls, at most one timeout; what they answer closes their
        connection."""
        if self._server is None:
            return
        self.runtime.stopping = True
        self._server.close()
        self._server = None
        for connection in list(self.connections):
            if not connection.busy:
                connection.close()
        deadline = time.monotonic() + self.runtime.cfg.timeout_ms / 1000
        while self.busy > 0 and time.monotonic() < deadline:
            await asyncio.sleep(0.05)
        if self._sweeper is not None:
            self._sweeper.cancel()

    def _sweep(self) -> None:
        """Closes the connections past their deadline: idle, a head or a body that does not arrive."""
        loop = asyncio.get_running_loop()
        now = loop.time()
        for connection in list(self.connections):
            if connection.deadline is not None and connection.deadline <= now:
                connection.close()
        self._sweeper = loop.call_later(_SWEEP_S, self._sweep)

    def date(self) -> str:
        """The Date header's value, made once a second."""
        second = int(time.time())
        if self._date[0] != second:
            self._date = (second, email.utils.formatdate(second, usegmt=True))
        return self._date[1]


class _Head:
    """A request's line and headers, as read."""

    __slots__ = ("method", "target", "version", "headers", "_by_name")

    def __init__(self, method: str, target: str, version: str, headers: list[tuple[str, str]]):
        self.method = method
        self.target = target
        self.version = version
        self.headers = headers
        self._by_name: dict[str, list[str]] = {}
        for name, value in headers:
            self._by_name.setdefault(name.lower(), []).append(value)

    def values(self, name: str) -> list[str]:
        return self._by_name.get(name, [])

    def first(self, name: str) -> str | None:
        found = self.values(name)
        return found[0] if found else None

    def tokens(self, name: str) -> set[str]:
        return {token.strip().lower() for value in self.values(name) for token in value.split(",")}


class _Connection(asyncio.Protocol):
    """One client's connection: requests taken from what it sends, answered in order."""

    def __init__(self, server: Server):
        self.server = server
        self.runtime = server.runtime
        self.loop = asyncio.get_running_loop()
        self.transport = None
        self.buffer = bytearray()
        self.busy = False  # a request is being answered
        self.ended = False  # the client sends no more
        self.deadline = None  # when, waiting, the connection is closed
        self.head = None  # the request whose body is being read
        self.chunked = False
        self.length = 0
        self.chunks: list[bytes] = []
        self.chunk_left = None  # None: a size line is next

    def connection_made(self, transport) -> None:
        self.transport = transport
        self.server.connections.add(self)
        self._wait(IDLE_TIMEOUT_S)

    def connection_lost(self, exc) -> None:
        self.server.connections.discard(self)
        self.deadline = None

    def data_received(self, data: bytes) -> None:
        self.buffer += data
        if not self.busy:
            self._advance()

    def eof_received(self) -> bool:
        self.ended = True
        if not self.busy:
            self.close()
        return True  # a call being answered still writes its answer

    def close(self) -> None:
        self.deadline = None
        if self.transport is not None:
            self.transport.close()

    def _wait(self, seconds: float) -> None:
        self.deadline = self.loop.time() + seconds

    def _advance(self) -> None:
        """Takes the requests the buffer holds whole, one at a time."""
        try:
            while not self.busy and not self.transport.is_closing():
                if self.head is None and not self._take_head():
                    return
                body = self._take_body()
                if body is None:
                    return
                head, self.head = self.head, None
                self._dispatch(head, body)
        except BadRequest as bad:
            self._refuse(bad.status)

    def _take_head(self) -> bool:
        while self.buffer[:2] == b"\r\n":  # empty lines before a request are allowed
            del self.buffer[:2]
        end = self.buffer.find(b"\r\n\r\n", 0, HEAD_MAX + 4)
        if end < 0:
            if len(self.buffer) > HEAD_MAX:
                raise BadRequest(431)
            if not self.buffer:
                self._wait(IDLE_TIMEOUT_S)
            elif self.deadline is None or self.deadline - self.loop.time() > HEAD_TIMEOUT_S:
                self._wait(HEAD_TIMEOUT_S)
            return False
        head = _parse_head(bytes(self.buffer[:end]))
        del self.buffer[:end + 4]
        self.chunked, self.length = _framing(head)
        self.chunks, self.chunk_left = [], None
        self.head = head
        if self.chunked or self.length:
            self._wait(self.runtime.cfg.timeout_ms / 1000)
            fits = self.chunked or self.length <= self.runtime.cfg.max_body_size
            if fits and head.version == "HTTP/1.1" and "100-continue" in head.tokens("expect"):
                self.transport.write(b"HTTP/1.1 100 Continue\r\n\r\n")
        return True

    def _take_body(self):
        """The body, once whole; None until then; TOO_LARGE past the limit."""
        limit = self.runtime.cfg.max_body_size
        if not self.chunked:
            if self.length > limit:
                return _TOO_LARGE
            if len(self.buffer) < self.length:
                return None
            body = bytes(self.buffer[:self.length])
            del self.buffer[:self.length]
            return body
        while True:
            if self.chunk_left is None or self.chunk_left == _TRAILERS:
                end = self.buffer.find(b"\r\n", 0, _LINE_MAX)
                if end < 0:
                    if len(self.buffer) > _LINE_MAX:
                        raise BadRequest()
                    return None
                line = bytes(self.buffer[:end])
                del self.buffer[:end + 2]
                if self.chunk_left == _TRAILERS:
                    if not line:
                        return b"".join(self.chunks)
                    continue
                try:
                    size = int(line.split(b";", 1)[0].strip(), 16)
                except ValueError:
                    raise BadRequest() from None
                if size == 0:
                    self.chunk_left = _TRAILERS
                    continue
                self.length += size
                if self.length > limit:
                    return _TOO_LARGE
                self.chunk_left = size
            else:
                if len(self.buffer) < self.chunk_left + 2:
                    return None
                if self.buffer[self.chunk_left:self.chunk_left + 2] != b"\r\n":
                    raise BadRequest()
                self.chunks.append(bytes(self.buffer[:self.chunk_left]))
                del self.buffer[:self.chunk_left + 2]
                self.chunk_left = None

    def _dispatch(self, head: _Head, body) -> None:
        """Answers a request read whole: at once what the runtime answers itself,
        in a task what the handler does."""
        self.deadline = None
        runtime = self.runtime
        if head.version == "HTTP/1.1":
            keep = "close" not in head.tokens("connection")
        else:
            keep = "keep-alive" in head.tokens("connection")
        keep = keep and body is not _TOO_LARGE  # what is left of a body refused is not read
        request_id = request_id_of(head.first("x-request-id"))
        path = head.target.partition("?")[0]
        is_call = head.first(CALL_HEADER) == "1"
        if path.startswith("/_hivepaas/"):
            self._send(head, request_id, runtime.own_path(path), keep, is_call)
            return
        if body is _TOO_LARGE:
            answer = failure(413, "request too large", request_id, "request_too_large")
        elif runtime.handler is None:
            answer = failure(500, "internal error", request_id, "not_loaded")
        elif not runtime.try_reserve():
            answer = failure(429, "too many requests", request_id, "throttled")
            answer.headers["retry-after"] = "1"
        else:
            self.busy = True
            self.server.busy += 1
            self.loop.create_task(self._call(head, body, request_id, path, keep, is_call))
            return
        runtime.log_invocation(request_id, head.method.upper(), path, answer)
        self._send(head, request_id, answer, keep, is_call)

    async def _call(self, head: _Head, body: bytes, request_id: str, path: str, keep: bool,
                    is_call: bool) -> None:
        try:
            headers = [(name, value) for name, value in head.headers if name.lower() != CALL_HEADER]
            answer = await self.runtime.call_async(request_of(head.method, head.target, headers, body), request_id)
            self.runtime.log_invocation(request_id, head.method.upper(), path, answer)
            self.busy = False
            self._send(head, request_id, answer, keep, is_call)
        finally:
            self.busy = False
            self.server.busy -= 1
        if not self.transport.is_closing():
            self._advance()

    def _send(self, head: _Head, request_id: str, answer: Answer, keep: bool, is_call: bool = False) -> None:
        """Writes the answer, and to a call what the runtime made of it; closes
        the connection after it unless it is kept."""
        keep = keep and not self.ended and not self.runtime.stopping
        lines = [f"HTTP/1.1 {answer.status} {_REASONS.get(answer.status, '')}", f"date: {self.server.date()}"]
        for name, value in answer.headers.items():
            if name in _OWN_HEADERS:
                continue
            for one in value if isinstance(value, list) else [value]:
                lines.append(f"{name}: {one}")
        lines.append(f"x-request-id: {request_id}")
        if is_call and answer.outcome:
            lines.append(f"x-hivepaas-outcome: {answer.outcome}")
            lines.append(f"x-hivepaas-duration-ms: {answer.duration_ms}")
        bodyless = answer.status in _BODYLESS
        if not bodyless:
            lines.append(f"content-length: {len(answer.body)}")
        if not keep:
            lines.append("connection: close")
        elif head.version == "HTTP/1.0":
            lines.append("connection: keep-alive")
        data = ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1")
        if head.method != "HEAD" and not bodyless:
            data += answer.body
        if self.transport.is_closing():
            return
        self.transport.write(data)
        if keep:
            self._wait(IDLE_TIMEOUT_S)
        else:
            self.close()

    def _refuse(self, status: int) -> None:
        if not self.transport.is_closing():
            self.transport.write((f"HTTP/1.1 {status} {_REASONS.get(status, '')}\r\ndate: {self.server.date()}\r\n"
                                  f"content-length: 0\r\nconnection: close\r\n\r\n").encode("latin-1"))
        self.close()


_TOO_LARGE = object()


def _parse_head(raw: bytes) -> _Head:
    lines = raw.split(b"\r\n")
    parts = lines[0].decode("latin-1").split(" ")
    if len(parts) != 3 or not _TOKEN.fullmatch(parts[0]) or not parts[1] or parts[2] not in ("HTTP/1.1", "HTTP/1.0"):
        raise BadRequest()
    headers = []
    for line in lines[1:]:
        name, colon, value = line.decode("latin-1").partition(":")
        if not colon or not _TOKEN.fullmatch(name):
            raise BadRequest()
        headers.append((name, value.strip(" \t")))
    return _Head(parts[0], parts[1], parts[2], headers)


def _framing(head: _Head) -> tuple[bool, int]:
    """Whether the body comes in chunks, else its length. A request that says
    both, or either in a way that cannot be read, is refused."""
    lengths = set(head.values("content-length"))
    encodings = head.values("transfer-encoding")
    if encodings:
        if lengths or not encodings[-1].lower().rstrip().endswith("chunked"):
            raise BadRequest()
        return True, 0
    if not lengths:
        return False, 0
    length = next(iter(lengths))
    if len(lengths) != 1 or not length.isdigit():
        raise BadRequest()
    return False, int(length)
