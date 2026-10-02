"""hivepaas-runtime for Python: serve, invoke, health and deps."""

import os
import subprocess
import sys
import urllib.error
import urllib.request

from . import processes
from .runtime import APP_DIR, ConfigError, Runtime, load_config

USAGE = "usage: hivepaas-runtime serve|invoke|health|deps"
HEALTH_TIMEOUT_S = 3


def main(argv: list[str]) -> int:
    command = argv[1] if len(argv) > 1 else "serve"
    if command == "deps":
        return deps()
    if command not in ("serve", "invoke", "health"):
        print(USAGE, file=sys.stderr)
        return 2
    try:
        cfg = load_config(os.environ)
    except ConfigError as error:
        print(f"hivepaas: {error}", file=sys.stderr)
        return 2
    if command == "health":
        return health(cfg.port)
    if command == "invoke":
        code = Runtime.load(cfg).invoke(sys.stdin.buffer.read())
    else:
        try:
            count = processes.process_count(os.environ, cfg)
        except ConfigError as error:
            print(f"hivepaas: {error}", file=sys.stderr)
            return 2
        code = processes.serve(cfg, count)
    sys.stdout.flush()
    sys.stderr.flush()
    os._exit(code)  # at once: a handler still running past its timeout is done with


def health(port: int) -> int:
    """Asks the server on this container: 0 when it answers 200."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # never through a proxy
    try:
        with opener.open(f"http://127.0.0.1:{port}/_hivepaas/health", timeout=HEALTH_TIMEOUT_S):
            return 0
    except urllib.error.HTTPError as error:
        print(f"hivepaas: health is {error.code}", file=sys.stderr)
    except OSError as error:
        print(f"hivepaas: {error}", file=sys.stderr)
    return 1


def deps() -> int:
    """Installs requirements.txt into /app/.venv, resolving requirements.lock first when there is none."""
    if not os.path.exists(f"{APP_DIR}/requirements.txt"):
        return 0
    if not os.path.exists(f"{APP_DIR}/requirements.lock"):
        compiled = subprocess.run(
            ["uv", "pip", "compile", "--quiet", "requirements.txt", "--output-file", "requirements.lock"],
            cwd=APP_DIR, check=False)
        if compiled.returncode != 0:
            return compiled.returncode
    return subprocess.run(["uv", "pip", "sync", "requirements.lock"], cwd=APP_DIR, check=False).returncode


if __name__ == "__main__":
    sys.exit(main(sys.argv))
