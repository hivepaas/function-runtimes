"""The server, as a client on a socket meets it: how a request is read, when a
connection is kept, what the server answers itself, and how it stops."""

import asyncio
import json
import socket
import unittest

from hivepaas_runtime.runtime import Runtime, load_config
from hivepaas_runtime.server import Server, listen


def handler(req, ctx):
    if req.path == "/slow":
        import time
        time.sleep(float(req.query["s"]))
        return {"body": "slow done"}
    if req.path == "/empty":
        return None
    return {"body": {"method": req.method, "path": req.path, "body": req.text(),
                     "custom": req.headers.get("x-custom"), "query": req.query_all}}


class _ServerCase(unittest.IsolatedAsyncioTestCase):
    """A server on a port of its own, and a client's means to talk to it."""

    async def asyncSetUp(self):
        await self.start()

    async def start(self, env=None, fn=handler):
        cfg = load_config({"HP_FN_TIMEOUT_MS": "1000", "HP_FN_MAX_BODY_SIZE": "200", **(env or {})})
        self.lines, self.errors = [], []
        self.runtime = Runtime(fn, cfg, out=self.lines.append, err=self.errors.append)
        self.addCleanup(self.runtime.close)
        sock = listen(0)
        self.port = sock.getsockname()[1]
        self.server = Server(self.runtime)
        await self.server.start(sock)
        self.addAsyncCleanup(self.server.stop)

    async def connect(self):
        reader, writer = await asyncio.open_connection("127.0.0.1", self.port)
        self.addCleanup(writer.close)
        return reader, writer

    async def response(self, reader):
        """A response: its status, its headers by lower-case name, its body."""
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 5)
        lines = head.decode("latin-1").split("\r\n")
        status = int(lines[0].split(" ")[1])
        headers = {}
        for line in lines[1:]:
            if line:
                name, _, value = line.partition(":")
                headers[name.strip().lower()] = value.strip()
        body = await reader.readexactly(int(headers.get("content-length", "0")))
        return status, headers, body

    async def closed(self, reader):
        return await asyncio.wait_for(reader.read(), 5) == b""


class ServerTest(_ServerCase):
    async def test_a_request_is_answered_and_its_connection_kept(self):
        reader, writer = await self.connect()
        for body in (b"one", b"two"):
            writer.write(b"POST /echo?a=1&a=2 HTTP/1.1\r\nHost: x\r\nX-Custom: v\r\nContent-Length: 3\r\n\r\n" + body)
            status, headers, answered = await self.response(reader)
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(answered), {"method": "POST", "path": "/echo", "body": body.decode(),
                                                    "custom": "v", "query": {"a": ["1", "2"]}})
            self.assertEqual(headers["content-type"], "application/json")
            self.assertIn("x-request-id", headers)
            self.assertIn("date", headers)
            self.assertNotIn("server", headers)
        self.assertEqual(sum('"hp":"invocation"' in line for line in self.lines), 2)

    async def test_a_head_that_arrives_in_pieces(self):
        reader, writer = await self.connect()
        for piece in (b"GET /pie", b"ces HTTP/1.1\r\nHo", b"st: x\r\n", b"\r\n"):
            writer.write(piece)
            await writer.drain()
            await asyncio.sleep(0.02)
        status, _, body = await self.response(reader)
        self.assertEqual((status, json.loads(body)["path"]), (200, "/pieces"))

    async def test_pipelined_requests_are_answered_in_order(self):
        reader, writer = await self.connect()
        writer.write(b"GET /first HTTP/1.1\r\nHost: x\r\n\r\nGET /second HTTP/1.1\r\nHost: x\r\n\r\n")
        paths = [json.loads((await self.response(reader))[2])["path"] for _ in range(2)]
        self.assertEqual(paths, ["/first", "/second"])

    async def test_http_1_0_keeps_a_connection_only_when_asked(self):
        reader, writer = await self.connect()
        writer.write(b"GET /once HTTP/1.0\r\n\r\n")
        status, headers, _ = await self.response(reader)
        self.assertEqual(status, 200)
        self.assertTrue(await self.closed(reader))

        reader, writer = await self.connect()
        for path in (b"/kept", b"/again"):
            writer.write(b"GET " + path + b" HTTP/1.0\r\nConnection: keep-alive\r\n\r\n")
            status, headers, body = await self.response(reader)
            self.assertEqual((status, headers.get("connection")), (200, "keep-alive"))
            self.assertEqual(json.loads(body)["path"], path.decode())

    async def test_http_1_1_closes_when_asked(self):
        reader, writer = await self.connect()
        writer.write(b"GET /bye HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
        status, headers, _ = await self.response(reader)
        self.assertEqual((status, headers.get("connection")), (200, "close"))
        self.assertTrue(await self.closed(reader))

    async def test_a_body_in_chunks(self):
        reader, writer = await self.connect()
        writer.write(b"POST /echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"
                     b"3\r\nabc\r\n4;ext=1\r\ndefg\r\n0\r\nTrailer: t\r\n\r\n")
        status, _, body = await self.response(reader)
        self.assertEqual((status, json.loads(body)["body"]), (200, "abcdefg"))

    async def test_a_body_over_the_limit_is_refused_and_the_connection_closed(self):
        for request in (b"POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 201\r\n\r\n",
                        b"POST /echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\nc9\r\n" + b"x" * 201):
            reader, writer = await self.connect()
            writer.write(request)
            status, headers, body = await self.response(reader)
            self.assertEqual((status, headers.get("connection"), json.loads(body)["error"]),
                             (413, "close", "request too large"))
            self.assertTrue(await self.closed(reader))
        self.assertTrue(any('"outcome":"request_too_large"' in line for line in self.lines))

    async def test_100_continue_is_sent_before_the_body_is_read(self):
        reader, writer = await self.connect()
        writer.write(b"POST /echo HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\n")
        interim = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 5)
        self.assertTrue(interim.startswith(b"HTTP/1.1 100"))
        writer.write(b"ok")
        status, _, body = await self.response(reader)
        self.assertEqual((status, json.loads(body)["body"]), (200, "ok"))

    async def test_a_body_that_does_not_arrive_is_no_call(self):
        reader, writer = await self.connect()
        writer.write(b"POST /echo HTTP/1.1\r\nHost: x\r\nX-Request-Id: stalled\r\nContent-Length: 10\r\n\r\nabc")
        self.assertTrue(await self.closed(reader))  # after the timeout, 1s
        self.assertFalse(any("stalled" in line for line in self.lines))

    async def test_what_cannot_be_read_is_refused_and_closed(self):
        for request, status in [(b"NOT HTTP\r\n\r\n", 400),
                                (b"GET / HTTP/1.1\r\nBad Header\r\n\r\n", 400),
                                (b"GET / HTTP/1.1\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n", 400),
                                (b"GET / HTTP/1.1\r\nX-Big: " + b"x" * 70000 + b"\r\n\r\n", 431)]:
            reader, writer = await self.connect()
            writer.write(request)
            answered, headers, _ = await self.response(reader)
            self.assertEqual((answered, headers.get("connection")), (status, "close"), request[:30])
            self.assertTrue(await self.closed(reader))
        self.assertFalse(any('"hp":"invocation"' in line for line in self.lines))

    async def test_the_runtimes_paths_read_their_body_and_are_not_logged(self):
        reader, writer = await self.connect()
        writer.write(b"POST /_hivepaas/health HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\nbody"
                     b"GET /after HTTP/1.1\r\nHost: x\r\n\r\n")
        status, _, body = await self.response(reader)
        self.assertEqual((status, json.loads(body)), (200, {"status": "ok"}))
        status, _, body = await self.response(reader)
        self.assertEqual((status, json.loads(body)["path"]), (200, "/after"))
        self.assertFalse(any("/_hivepaas" in line for line in self.lines))

    async def test_head_and_no_content_send_no_body(self):
        reader, writer = await self.connect()
        writer.write(b"HEAD /echo HTTP/1.1\r\nHost: x\r\n\r\n")
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 5)
        self.assertIn(b"content-length: ", head.lower())
        writer.write(b"GET /empty HTTP/1.1\r\nHost: x\r\n\r\n")
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 5)
        self.assertTrue(head.startswith(b"HTTP/1.1 204"), head)  # nothing of HEAD's body came before it
        self.assertNotIn(b"content-length", head.lower())

    async def test_calls_over_the_concurrency_are_turned_away(self):
        await self.server.stop()
        await self.start({"HP_FN_MAX_CONCURRENCY": "1", "HP_FN_TIMEOUT_MS": "3000"})
        slow_reader, slow_writer = await self.connect()
        slow_writer.write(b"GET /slow?s=0.5 HTTP/1.1\r\nHost: x\r\n\r\n")
        await asyncio.sleep(0.2)
        reader, writer = await self.connect()
        writer.write(b"GET /echo HTTP/1.1\r\nHost: x\r\n\r\n")
        status, headers, _ = await self.response(reader)
        self.assertEqual((status, headers.get("retry-after")), (429, "1"))
        self.assertEqual((await self.response(slow_reader))[0], 200)

    async def test_stopping_closes_idle_connections_and_lets_a_running_call_finish(self):
        await self.server.stop()
        await self.start({"HP_FN_TIMEOUT_MS": "3000"})
        idle_reader, _ = await self.connect()
        reader, writer = await self.connect()
        writer.write(b"GET /slow?s=0.5 HTTP/1.1\r\nHost: x\r\n\r\n")
        await asyncio.sleep(0.2)

        stopped = asyncio.ensure_future(self.server.stop())
        self.assertTrue(await self.closed(idle_reader))
        status, headers, body = await self.response(reader)
        self.assertEqual((status, headers.get("connection"), body), (200, "close", b"slow done"))
        await asyncio.wait_for(stopped, 5)
        with self.assertRaises(OSError):
            await asyncio.open_connection("127.0.0.1", self.port)


class CallHeaderTest(_ServerCase):
    async def test_a_call_is_told_its_outcome_and_its_handler_does_not_see_the_header(self):
        reader, writer = await self.connect()
        writer.write(b"GET /echo HTTP/1.1\r\nHost: x\r\nX-Hivepaas-Call: 1\r\nX-Custom: v\r\n\r\n")
        status, headers, body = await self.response(reader)
        self.assertEqual((status, headers.get("x-hivepaas-outcome")), (200, "ok"))
        float(headers["x-hivepaas-duration-ms"])
        self.assertEqual(json.loads(body)["custom"], "v")
        self.assertFalse(any("x-hivepaas-call" in line for line in self.seen_headers))

        writer.write(b"GET /echo HTTP/1.1\r\nHost: x\r\n\r\n")
        _, headers, _ = await self.response(reader)
        self.assertNotIn("x-hivepaas-outcome", headers)

    async def asyncSetUp(self):
        self.seen_headers = []

        def remembering(req, ctx):
            self.seen_headers.append(" ".join(req.headers))
            return handler(req, ctx)

        await self.start(fn=remembering)


class ListenTest(unittest.TestCase):
    def test_it_listens_on_ipv6_and_ipv4_where_there_is_ipv6(self):
        sock = listen(0)
        self.addCleanup(sock.close)
        port = sock.getsockname()[1]
        socket.create_connection(("127.0.0.1", port), timeout=2).close()
        if sock.family == socket.AF_INET6:
            socket.create_connection(("::1", port), timeout=2).close()


if __name__ == "__main__":
    unittest.main()
