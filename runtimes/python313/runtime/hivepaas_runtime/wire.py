"""What crosses the runtime's edges, and nothing heavier: its configuration from
the environment, and invoke's request and result marker. call reads it without
loading the runtime, so it imports what a process starting fast can afford."""

import base64
import binascii
import json
import os
import urllib.parse
from collections import namedtuple
from collections.abc import Mapping

APP_DIR = "/app"
RESULT_MARKER = "#hivepaas-result "

_MAX_LIMIT = 2147483647


class ConfigError(Exception):
    """A configuration the runtime cannot start with."""


Config = namedtuple("Config", "entrypoint handler port timeout_ms max_concurrency max_body_size",
                    defaults=("main.py", "handler", 8080, 30000, 16, 6291456))


def load_config(env: Mapping[str, str]) -> Config:
    entrypoint = env.get("HP_FN_ENTRYPOINT") or "main.py"
    resolved = os.path.normpath(os.path.join(APP_DIR, entrypoint))
    if os.path.isabs(entrypoint) or not resolved.startswith(APP_DIR + os.sep) or not entrypoint.endswith(".py"):
        raise ConfigError(f'HP_FN_ENTRYPOINT is "{entrypoint}", which is not a Python file of the function')
    return Config(
        entrypoint=entrypoint,
        handler=env.get("HP_FN_HANDLER") or "handler",
        port=_positive(env, "HP_FN_PORT", 8080),
        timeout_ms=_positive(env, "HP_FN_TIMEOUT_MS", 30000),
        max_concurrency=_positive(env, "HP_FN_MAX_CONCURRENCY", 16),
        max_body_size=_positive(env, "HP_FN_MAX_BODY_SIZE", 6291456),
    )


def _positive(env: Mapping[str, str], name: str, fallback: int) -> int:
    value = env.get(name)
    if not value:
        return fallback
    return positive_whole_number(name, value)


def positive_whole_number(name: str, value: str) -> int:
    """value as a positive whole number; a ConfigError naming name when it is not one."""
    digits = value[1:] if value.startswith("+") else value
    if not (digits.isascii() and digits.isdigit()) or not 0 < int(digits) <= _MAX_LIMIT:
        raise ConfigError(f'{name} is "{value}", which is not a positive whole number')
    return int(digits)


def read_invoke_request(data: bytes):
    """invoke's request: its method, its path, the path with its query, its
    headers as pairs, and its body. A ValueError says why it cannot be read."""
    try:
        given = json.loads(data)
    except (ValueError, UnicodeDecodeError) as error:
        raise ValueError(f"the request is not JSON: {error}") from None
    if not isinstance(given, dict):
        raise ValueError("the request is not a JSON object")
    encoded = given.get("body") or ""
    try:
        if not isinstance(encoded, str):
            raise binascii.Error
        body = base64.b64decode(encoded, validate=True)
    except binascii.Error:
        raise ValueError("the request's body is not base64") from None
    path = str(given.get("path") or "/")
    query = urllib.parse.urlencode(_pairs(given.get("query"), "query"))
    method = str(given.get("method") or "GET")
    return method, path, f"{path}?{query}" if query else path, _pairs(given.get("headers"), "headers"), body


def _pairs(given, what: str) -> list[tuple[str, str]]:
    if given is None:
        return []
    if not isinstance(given, dict):
        raise ValueError(f"the request's {what} is not an object")
    return [(name, str(value)) for name, values in given.items()
            for value in (values if isinstance(values, list) else [values])]
