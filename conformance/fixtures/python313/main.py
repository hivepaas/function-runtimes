"""The function the conformance suite calls: each path shows one thing the
contract promises."""

import os
import time

import fixture_lib


def handler(req, ctx):
    q = req.query
    match req.path:
        case "/":
            return None
        case "/json":
            return {"body": {"hello": q.get("name")}}
        case "/text":
            return {"body": "hello"}
        case "/bytes":
            return {"body": bytes([0, 1, 2, 255])}
        case "/echo":
            return {
                "body": {
                    "method": req.method,
                    "path": req.path,
                    "query": q.get("a"),
                    "queryAll": req.query_all.get("a"),
                    "header": req.headers.get("x-custom"),
                    "body": req.text(),
                    "requestId": ctx.request_id,
                }
            }
        case "/status":
            return {"status": int(q["code"])}
        case "/error":
            raise RuntimeError("fixture failure")
        case "/slow":
            time.sleep(int(q["ms"]) / 1000)  # past the timeout on purpose
            return {"body": "slow done"}
        case "/big":
            return {"body": "x" * int(q["bytes"])}
        case "/invalid":
            return {"status": 42}
        case "/env":
            return {"body": os.environ.get("GREETING", "")}
        case "/deadline":
            return {"body": {"remainingMs": int((ctx.deadline - time.time()) * 1000)}}
        case "/log":
            ctx.log("fixture log line")
            return {"body": "logged"}
        case "/pid":
            return {"body": {"pid": os.getpid()}}
        case "/whoami":
            return {"body": {"uid": os.getuid(), "gid": os.getgid()}}
        case "/lib":
            return {"body": fixture_lib.name()}
    return {"status": 404, "body": {"error": "no route"}}
