"""call: a request handed to the serve running on this container, its answer
written as invoke writes a result. It starts as fast as a process can: it loads
nothing of the runtime, and speaks HTTP/1.1 on a socket rather than import
http.client, which brings the email package along."""

import base64
import json
import socket
import time
from collections.abc import Callable

from .wire import RESULT_MARKER, Config, read_invoke_request

# A request with this header is call's: serve's answer to it says the call's
# outcome and duration, and the handler does not see it.
CALL_HEADER = "x-hivepaas-call"
# How much longer than a call's timeout call waits for serve, which answers at it.
WAIT_MARGIN_S = 10
# The headers of serve's answer that are the connection's or the runtime's.
_NOT_THE_CALLS = {"content-length", "transfer-encoding", "connection", "keep-alive", "date",
                  "x-hivepaas-outcome", "x-hivepaas-duration-ms"}
# Headers call sets itself.
_OWN = {CALL_HEADER, "content-length", "connection", "transfer-encoding"}
_TOKEN_CHARS = frozenset("!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")


class _Unreadable(ValueError):
    """A request call cannot send."""


def call(data: bytes, cfg: Config, out: Callable[[str], None], err: Callable[[str], None]) -> int:
    """Sends the request in data to serve, and writes its answer as a result. A
    call turned away (429) is sent again after Retry-After while the timeout
    allows. Returns the exit code: 0 with a result, 2 when the request cannot be
    read, 3 when serve does not answer."""
    try:
        method, _, target, header_pairs, body = read_invoke_request(data)
        request = _request_bytes(method.upper(), target, header_pairs, body)
    except ValueError as error:
        err(f"hivepaas: {error}")
        return 2
    deadline = time.monotonic() + cfg.timeout_ms / 1000
    while True:
        try:
            status, headers, answered = _exchange(cfg.port, request, cfg.timeout_ms / 1000 + WAIT_MARGIN_S)
        except (OSError, ValueError) as error:
            err(f"hivepaas: serve does not answer: {error}")
            return 3
        wait = _seconds(headers.get("retry-after", ["1"])[0])
        if status == 429 and time.monotonic() + wait < deadline:
            time.sleep(wait)
            continue
        out(RESULT_MARKER + json.dumps(_result(status, headers, answered), separators=(",", ":"),
                                       ensure_ascii=False))
        return 0


def _request_bytes(method: str, target: str, header_pairs, body: bytes) -> bytes:
    if not method or not set(method) <= _TOKEN_CHARS:
        raise _Unreadable(f"the request's method {method!r} cannot be sent")
    if not target.startswith("/") or any(c.isspace() or ord(c) < 32 for c in target):
        raise _Unreadable(f"the request's path {target!r} cannot be sent")
    lines = [f"{method} {target} HTTP/1.1"]
    names = set()
    for name, value in header_pairs:
        if not name or not set(name) <= _TOKEN_CHARS or any(c in value for c in "\r\n\0"):
            raise _Unreadable(f"the request's header {name!r} cannot be sent")
        if name.lower() not in _OWN:
            lines.append(f"{name}: {value}")
            names.add(name.lower())
    if "host" not in names:
        lines.append("host: 127.0.0.1")
    lines += [f"{CALL_HEADER}: 1", f"content-length: {len(body)}", "connection: close"]
    try:
        return ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1") + body
    except UnicodeEncodeError:
        raise _Unreadable("the request's headers are not Latin-1") from None


def _exchange(port: int, request: bytes, timeout: float):
    """Sends request to serve and reads its answer whole: its status, its
    headers by lower-case name, its body."""
    with socket.create_connection(("127.0.0.1", port), timeout=timeout) as sock:
        sock.sendall(request)
        received = bytearray()
        while (end := received.find(b"\r\n\r\n")) < 0:
            chunk = sock.recv(65536)
            if not chunk:
                raise ValueError("the connection ended before an answer")
            received += chunk
        lines = bytes(received[:end]).decode("latin-1").split("\r\n")
        status = int(lines[0].split(" ", 2)[1])
        headers: dict[str, list[str]] = {}
        for line in lines[1:]:
            name, _, value = line.partition(":")
            headers.setdefault(name.strip().lower(), []).append(value.strip())
        body = bytearray(received[end + 4:])
        length = headers.get("content-length")
        while length is None or len(body) < int(length[0]):
            chunk = sock.recv(65536)
            if not chunk:
                if length is None:
                    break  # the body is what came before the connection closed
                raise ValueError("the connection ended before the answer's body")
            body += chunk
        return status, headers, bytes(body)


def _seconds(value: str) -> float:
    try:
        return max(0.0, float(value))
    except ValueError:
        return 1.0


def _result(status: int, headers: dict[str, list[str]], body: bytes) -> dict:
    kept = {name: values for name, values in headers.items() if name not in _NOT_THE_CALLS}
    try:
        duration = float(headers.get("x-hivepaas-duration-ms", ["0"])[0])
    except ValueError:
        duration = 0.0
    return {
        "status": status,
        "headers": kept,
        "body": base64.b64encode(body).decode(),
        "requestId": kept.get("x-request-id", [""])[0],
        "durationMs": duration,
        "outcome": headers.get("x-hivepaas-outcome", ["ok"])[0],
    }
