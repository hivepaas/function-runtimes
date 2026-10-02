"""What the conformance suite does not reach: how a module is loaded, how
whatever a handler returns becomes a response, how a request is read, and how
an async handler is called."""

import asyncio
import os
import sys
import tempfile
import threading
import time
import unittest

from hivepaas_runtime.runtime import (
    Config,
    ConfigError,
    Runtime,
    load_config,
    load_handler,
    request_of,
    to_answer,
)


class ConfigTest(unittest.TestCase):
    def test_the_defaults_are_the_contracts(self):
        self.assertEqual(load_config({}), Config(
            entrypoint="main.py", handler="handler", port=8080,
            timeout_ms=30000, max_concurrency=16, max_body_size=6291456))

    def test_the_environment_sets_the_limits(self):
        cfg = load_config({
            "HP_FN_ENTRYPOINT": "src/fn.py", "HP_FN_HANDLER": "hello", "HP_FN_PORT": "9000",
            "HP_FN_TIMEOUT_MS": "1500", "HP_FN_MAX_CONCURRENCY": "2", "HP_FN_MAX_BODY_SIZE": "1024"})
        self.assertEqual(cfg, Config(
            entrypoint="src/fn.py", handler="hello", port=9000,
            timeout_ms=1500, max_concurrency=2, max_body_size=1024))

    def test_a_limit_that_is_not_a_positive_whole_number_is_refused(self):
        for value in ["0", "-1", "abc", "1.5", "1e3"]:
            with self.assertRaisesRegex(ConfigError, "HP_FN_TIMEOUT_MS"):
                load_config({"HP_FN_TIMEOUT_MS": value})

    def test_an_entrypoint_that_is_not_a_python_file_of_the_function_is_refused(self):
        for value in ["../etc/passwd.py", "/etc/fn.py", "main.js"]:
            with self.assertRaisesRegex(ConfigError, "HP_FN_ENTRYPOINT"):
                load_config({"HP_FN_ENTRYPOINT": value})


class LoadTest(unittest.TestCase):
    def app(self, files):
        directory = tempfile.mkdtemp()
        for name, text in files.items():
            path = os.path.join(directory, name)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as f:
                f.write(text)
        self.addCleanup(lambda: sys.path.remove(directory) if directory in sys.path else None)
        return directory

    def test_a_handler_is_the_function_it_is_named(self):
        directory = self.app({"fn_a.py": "def handler(req, ctx):\n    return 'a'\n"
                                         "def hello(req, ctx):\n    return 'hello'\n"})
        self.assertEqual(load_handler(directory, load_config({"HP_FN_ENTRYPOINT": "fn_a.py"}))(None, None), "a")
        cfg = load_config({"HP_FN_ENTRYPOINT": "fn_a.py", "HP_FN_HANDLER": "hello"})
        self.assertEqual(load_handler(directory, cfg)(None, None), "hello")

    def test_the_functions_directory_is_first_on_the_module_path(self):
        directory = self.app({"src/fn_b.py": "import helper_b\ndef handler(req, ctx):\n    return helper_b.X\n",
                              "helper_b.py": "X = 'from helper'\n"})
        cfg = load_config({"HP_FN_ENTRYPOINT": "src/fn_b.py"})
        self.assertEqual(load_handler(directory, cfg)(None, None), "from helper")

    def test_a_handler_that_is_not_there_or_a_module_that_fails_cannot_be_loaded(self):
        directory = self.app({"fn_c.py": "x = 1\n", "fn_d.py": "raise RuntimeError('at import')\n"})
        with self.assertRaisesRegex(Exception, "handler"):
            load_handler(directory, load_config({"HP_FN_ENTRYPOINT": "fn_c.py"}))
        with self.assertRaisesRegex(RuntimeError, "at import"):
            load_handler(directory, load_config({"HP_FN_ENTRYPOINT": "fn_d.py"}))
        with self.assertRaises(ImportError):
            load_handler(directory, load_config({"HP_FN_ENTRYPOINT": "none.py"}))


class AnswerTest(unittest.TestCase):
    def test_a_body_is_sent_with_the_type_it_has(self):
        text = to_answer({"body": "hi"}, 100)
        self.assertEqual((text.status, text.headers["content-type"], text.body),
                         (200, "text/plain; charset=utf-8", b"hi"))
        self.assertEqual(to_answer({"body": b"\x00\xff"}, 100).headers["content-type"], "application/octet-stream")
        self.assertEqual(to_answer({"body": bytearray(b"x")}, 100).body, b"x")
        for value, json in [({"a": 1}, b'{"a":1}'), ([1, 2], b"[1,2]"), (0, b"0"), (False, b"false"),
                            ({"name": "Đà"}, '{"name":"Đà"}'.encode())]:
            answer = to_answer({"body": value}, 100)
            self.assertEqual((answer.headers["content-type"], answer.body), ("application/json", json))

    def test_nothing_is_no_content_and_no_body_is_an_empty_one(self):
        nothing = to_answer(None, 100)
        self.assertEqual((nothing.status, nothing.body), (204, b""))
        empty = to_answer({"status": 201}, 100)
        self.assertEqual((empty.status, empty.body, empty.headers), (201, b"", {}))

    def test_the_handlers_headers_are_sent_its_content_type_first(self):
        answer = to_answer({"headers": {"Content-Type": "text/csv", "Set-Cookie": ["a=1", "b=2"], "X-N": 1},
                            "body": "a,b"}, 100)
        self.assertEqual(answer.headers, {"content-type": "text/csv", "set-cookie": ["a=1", "b=2"], "x-n": "1"})

    def test_a_status_that_carries_no_body_sends_none(self):
        self.assertEqual(to_answer({"status": 204, "body": "x"}, 100).body, b"")
        self.assertEqual(to_answer({"status": 304, "body": "x"}, 100).body, b"")

    def test_what_is_not_a_response_is_an_error(self):
        for value in ["hello", 42, [], b"x", {"status": 42}, {"status": 600}, {"status": "200"}, {"status": True},
                      {"status": 200.0}, {"headers": {"bad name": "x"}}, {"headers": {"ok": "line\nbreak"}},
                      {"headers": "x"}, {"body": object()}, {"body": float("nan")}]:
            with self.subTest(value=value), self.assertRaises(Exception) as caught:
                to_answer(value, 100)
            self.assertEqual(getattr(caught.exception, "outcome", None), "error")

    def test_a_body_over_the_limit_is_its_own_error(self):
        with self.assertRaises(Exception) as caught:
            to_answer({"body": "x" * 101}, 100)
        self.assertEqual(caught.exception.outcome, "response_too_large")
        self.assertEqual(len(to_answer({"body": "x" * 100}, 100).body), 100)


class RequestTest(unittest.TestCase):
    def test_a_request_is_read_as_it_was_sent(self):
        req = request_of("post", "/echo/a%20b?a=1&a=2&b=x+y&c=",
                         [("X-Custom", "v"), ("Accept", "a"), ("accept", "b")], b'{"k":1}')
        self.assertEqual((req.method, req.path), ("POST", "/echo/a%20b"))
        self.assertEqual(req.query, {"a": "1", "b": "x y", "c": ""})
        self.assertEqual(req.query_all["a"], ["1", "2"])
        self.assertEqual(req.headers, {"x-custom": "v", "accept": "a, b"})
        self.assertEqual(req.text(), '{"k":1}')
        self.assertEqual(req.json(), {"k": 1})


class CallTest(unittest.TestCase):
    def runtime(self, handler, timeout_ms=1000):
        lines, errors = [], []
        cfg = load_config({"HP_FN_TIMEOUT_MS": str(timeout_ms)})
        runtime = Runtime(handler, cfg, out=lines.append, err=errors.append)
        self.addCleanup(runtime.close)
        return runtime, lines, errors

    def call(self, runtime, request_id="id-1"):
        self.assertTrue(runtime.try_reserve())
        return runtime.call(request_of("GET", "/", [], b""), request_id)

    def test_an_async_handler_is_awaited(self):
        async def handler(req, ctx):
            await asyncio.sleep(0.01)
            ctx.log("async", "log")
            return {"body": "async"}

        runtime, lines, _ = self.runtime(handler)
        answer = self.call(runtime)
        self.assertEqual((answer.status, answer.body, answer.outcome), (200, b"async", "ok"))
        self.assertIn('{"hp":"log","requestId":"id-1","msg":"async log"}', lines)

    def test_an_async_handler_is_cancelled_at_its_timeout(self):
        cancelled = threading.Event()

        async def handler(req, ctx):
            try:
                await asyncio.sleep(5)
            except asyncio.CancelledError:
                cancelled.set()
                raise

        runtime, _, _ = self.runtime(handler, timeout_ms=50)
        answer = self.call(runtime)
        self.assertEqual((answer.status, answer.outcome), (504, "timeout"))
        self.assertTrue(cancelled.wait(1))

    def test_an_async_callable_object_is_cancelled_at_its_timeout_too(self):
        cancelled = threading.Event()

        class Handler:
            async def __call__(self, req, ctx):
                try:
                    await asyncio.sleep(5)
                except asyncio.CancelledError:
                    cancelled.set()
                    raise

        runtime, _, _ = self.runtime(Handler(), timeout_ms=50)
        self.assertEqual(self.call(runtime).status, 504)
        self.assertTrue(cancelled.wait(1))

    def test_a_function_that_returns_a_coroutine_is_awaited(self):
        async def answer():
            return {"body": "awaited"}

        runtime, _, _ = self.runtime(lambda req, ctx: answer())
        self.assertEqual(self.call(runtime).body, b"awaited")

    def test_a_function_that_times_out_keeps_its_place_until_it_returns(self):
        release = threading.Event()

        def handler(req, ctx):
            release.wait(5)

        runtime, _, _ = self.runtime(handler, timeout_ms=50)
        runtime.cfg = load_config({"HP_FN_TIMEOUT_MS": "50", "HP_FN_MAX_CONCURRENCY": "1"})
        self.assertEqual(self.call(runtime).status, 504)
        self.assertFalse(runtime.try_reserve())
        release.set()
        deadline = time.monotonic() + 1
        while not runtime.try_reserve():
            self.assertLess(time.monotonic(), deadline, "the place was not given back")
            time.sleep(0.005)

    def test_an_error_is_logged_with_its_traceback_and_not_sent(self):
        def handler(req, ctx):
            raise ValueError("fixture failure")

        runtime, _, errors = self.runtime(handler)
        answer = self.call(runtime, "id-err")
        self.assertEqual((answer.status, answer.outcome), (500, "error"))
        self.assertNotIn(b"fixture failure", answer.body)
        first, *rest = "\n".join(errors).splitlines()
        self.assertEqual(first, "hivepaas: call id-err failed: ValueError: fixture failure")
        self.assertTrue(any("Traceback" in line for line in rest))

    def test_invoke_refuses_a_body_that_is_not_base64(self):
        runtime, _, errors = self.runtime(lambda req, ctx: None)
        self.assertEqual(runtime.invoke(b'{"body":"%%%"}'), 2)
        self.assertIn("base64", "\n".join(errors))


class CallAsyncTest(unittest.IsolatedAsyncioTestCase):
    """How serve calls a handler: on its event loop for a coroutine, in a thread
    for a function."""

    def runtime(self, handler, timeout_ms=1000, max_concurrency=16):
        lines, errors = [], []
        cfg = load_config({"HP_FN_TIMEOUT_MS": str(timeout_ms), "HP_FN_MAX_CONCURRENCY": str(max_concurrency)})
        runtime = Runtime(handler, cfg, out=lines.append, err=errors.append)
        self.addCleanup(runtime.close)
        return runtime, lines

    async def call(self, runtime, request_id="id-1"):
        self.assertTrue(runtime.try_reserve())
        return await runtime.call_async(request_of("GET", "/", [], b""), request_id)

    async def test_a_coroutine_runs_on_the_callers_loop(self):
        loop = asyncio.get_running_loop()

        async def handler(req, ctx):
            ctx.log("on", "loop")
            return {"body": "same loop" if asyncio.get_running_loop() is loop else "another loop"}

        runtime, lines = self.runtime(handler)
        answer = await self.call(runtime)
        self.assertEqual((answer.status, answer.body, answer.outcome), (200, b"same loop", "ok"))
        self.assertIn('{"hp":"log","requestId":"id-1","msg":"on loop"}', lines)

    async def test_a_function_runs_in_a_thread_and_keeps_its_place_past_its_timeout(self):
        release = threading.Event()
        loop_thread = threading.current_thread()
        threads = []

        def handler(req, ctx):
            threads.append(threading.current_thread())
            release.wait(5)

        runtime, _ = self.runtime(handler, timeout_ms=50, max_concurrency=1)
        answer = await self.call(runtime)
        self.assertEqual((answer.status, answer.outcome), (504, "timeout"))
        self.assertIsNot(threads[0], loop_thread)
        self.assertFalse(runtime.try_reserve())
        release.set()
        for _ in range(200):
            if runtime.try_reserve():
                break
            await asyncio.sleep(0.005)
        else:
            self.fail("the place was not given back")

    async def test_a_coroutine_is_cancelled_at_its_timeout_and_gives_its_place_back(self):
        cancelled = asyncio.Event()

        async def handler(req, ctx):
            try:
                await asyncio.sleep(5)
            except asyncio.CancelledError:
                cancelled.set()
                raise

        runtime, _ = self.runtime(handler, timeout_ms=50, max_concurrency=1)
        answer = await self.call(runtime)
        self.assertEqual((answer.status, answer.outcome), (504, "timeout"))
        await asyncio.wait_for(cancelled.wait(), 1)
        await asyncio.sleep(0)
        self.assertTrue(runtime.try_reserve())

    async def test_an_error_is_logged_and_not_sent(self):
        async def handler(req, ctx):
            raise ValueError("fixture failure")

        lines, errors = [], []
        runtime = Runtime(handler, load_config({}), out=lines.append, err=errors.append)
        self.addCleanup(runtime.close)
        self.assertTrue(runtime.try_reserve())
        answer = await runtime.call_async(request_of("GET", "/", [], b""), "id-err")
        self.assertEqual((answer.status, answer.outcome), (500, "error"))
        self.assertTrue(errors[0].startswith("hivepaas: call id-err failed: ValueError: fixture failure"))

    async def test_places_are_taken_from_what_the_runtime_is_given(self):
        class Places:
            def __init__(self):
                self.taken = []

            def take(self, limit):
                self.taken.append(limit)
                return False

            def give(self):
                pass

        places = Places()
        runtime = Runtime(lambda req, ctx: None, load_config({"HP_FN_MAX_CONCURRENCY": "7"}), places=places)
        self.addCleanup(runtime.close)
        self.assertFalse(runtime.try_reserve())
        self.assertEqual(places.taken, [7])


if __name__ == "__main__":
    unittest.main()
