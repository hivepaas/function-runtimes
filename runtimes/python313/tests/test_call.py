"""call: a request handed to the serve running beside it, its answer written as
invoke writes a result."""

import base64
import http.server
import json
import threading
import unittest

from hivepaas_runtime.call import call
from hivepaas_runtime.wire import RESULT_MARKER, load_config


def serve_answers(test, answers):
    """A server answering each request with the next of answers; it keeps what it was sent."""
    seen = []

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def send_response(self, code, message=None):  # without Server and Date, as serve sends none of the first
            self.send_response_only(code, message)

        def _answer(self):
            length = int(self.headers.get("content-length") or 0)
            seen.append({"method": self.command, "path": self.path, "headers": self.headers,
                         "body": self.rfile.read(length)})
            status, headers, body = answers[min(len(seen), len(answers)) - 1]
            self.send_response(status)
            for name, value in headers:
                self.send_header(name, value)
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        do_GET = do_POST = _answer

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    test.addCleanup(server.server_close)
    test.addCleanup(server.shutdown)
    return server.server_address[1], seen


def run_call(port, data, **env):
    out, err = [], []
    cfg = load_config({"HP_FN_PORT": str(port), **env})
    return call(data, cfg, out.append, err.append), out, err


class CallTest(unittest.TestCase):
    def test_a_request_is_sent_to_serve_and_its_answer_written_as_a_result(self):
        port, seen = serve_answers(self, [(201, [
            ("content-type", "application/json"), ("x-request-id", "r-1"), ("set-cookie", "a=1"),
            ("set-cookie", "b=2"), ("x-hivepaas-outcome", "ok"), ("x-hivepaas-duration-ms", "2.5"),
        ], b'{"ok":true}')])
        body = base64.b64encode(b"payload").decode()

        code, out, _ = run_call(port, json.dumps({
            "method": "post", "path": "/echo", "query": {"a": ["1", "2"]}, "headers": {"X-Custom": ["v"]},
            "body": body}).encode())

        self.assertEqual(code, 0)
        self.assertEqual(len(out), 1)
        self.assertTrue(out[0].startswith(RESULT_MARKER))
        self.assertEqual(json.loads(out[0][len(RESULT_MARKER):]), {
            "status": 201,
            "headers": {"content-type": ["application/json"], "x-request-id": ["r-1"], "set-cookie": ["a=1", "b=2"]},
            "body": base64.b64encode(b'{"ok":true}').decode(),
            "requestId": "r-1", "durationMs": 2.5, "outcome": "ok"})
        sent = seen[0]
        self.assertEqual((sent["method"], sent["path"], sent["body"]), ("POST", "/echo?a=1&a=2", b"payload"))
        self.assertEqual((sent["headers"]["x-hivepaas-call"], sent["headers"]["x-custom"]), ("1", "v"))

    def test_a_call_turned_away_is_sent_again_while_its_timeout_allows(self):
        busy = (429, [("retry-after", "0"), ("x-hivepaas-outcome", "throttled")], b"")
        port, seen = serve_answers(self, [busy, busy, (200, [("x-hivepaas-outcome", "ok")], b"done")])

        code, out, _ = run_call(port, b"{}")

        self.assertEqual((code, len(seen)), (0, 3))
        self.assertEqual(json.loads(out[0][len(RESULT_MARKER):])["status"], 200)

    def test_a_call_still_turned_away_at_its_timeout_is_a_throttled_result(self):
        port, seen = serve_answers(self, [(429, [("retry-after", "1"), ("x-hivepaas-outcome", "throttled")], b"")])

        code, out, _ = run_call(port, b"{}", HP_FN_TIMEOUT_MS="200")

        result = json.loads(out[0][len(RESULT_MARKER):])
        self.assertEqual((code, result["status"], result["outcome"], len(seen)), (0, 429, "throttled", 1))

    def test_a_request_that_cannot_be_read_exits_2_and_no_server_3(self):
        code, out, _ = run_call(1, b"not json")
        self.assertEqual((code, out), (2, []))

        port, _ = serve_answers(self, [])
        self.doCleanups()  # the server is closed: nothing listens on port
        code, out, err = run_call(port, b"{}")
        self.assertEqual((code, out), (3, []))
        self.assertIn("serve does not answer", "\n".join(err))

    def test_a_request_that_cannot_go_on_an_http_request_line_or_header_exits_2(self):
        for request in [{"path": "no-slash"}, {"path": "/a b"}, {"method": "G ET"},
                        {"headers": {"x-a": ["v\r\nx-b: w"]}}, {"headers": {"bad name": ["v"]}}]:
            code, out, _ = run_call(1, json.dumps(request).encode())
            self.assertEqual((code, out), (2, []), request)


if __name__ == "__main__":
    unittest.main()
