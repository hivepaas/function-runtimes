"""hivepaas-runtime for Python: serve, invoke, call, health and deps."""

import os
import sys

# The rest is imported by the commands that use it: call, every scheduled call
# of a function, starts as fast as a process can.
from .wire import APP_DIR, ConfigError, load_config

USAGE = "usage: hivepaas-runtime serve|invoke|call|health|deps"
HEALTH_TIMEOUT_S = 3


def main(argv: list[str]) -> int:
    command = argv[1] if len(argv) > 1 else "serve"
    if command == "deps":
        return deps()
    if command not in ("serve", "invoke", "call", "health"):
        print(USAGE, file=sys.stderr)
        return 2
    try:
        cfg = load_config(os.environ)
    except ConfigError as error:
        print(f"hivepaas: {error}", file=sys.stderr)
        return 2
    if command == "health":
        return health(cfg.port)
    if command == "call":  # the handler is not loaded here: serve has it
        from .call import call
        return call(sys.stdin.buffer.read(), cfg, _line(sys.stdout), _line(sys.stderr))
    if command == "invoke":
        from .runtime import Runtime
        code = Runtime.load(cfg).invoke(sys.stdin.buffer.read())
    else:
        from . import processes
        try:
            count = processes.process_count(os.environ, cfg)
        except ConfigError as error:
            print(f"hivepaas: {error}", file=sys.stderr)
            return 2
        code = processes.serve(cfg, count)
    sys.stdout.flush()
    sys.stderr.flush()
    os._exit(code)  # at once: a handler still running past its timeout is done with


def _line(stream):
    def write(text: str) -> None:
        stream.write(text + "\n")
        stream.flush()
    return write


def health(port: int) -> int:
    """Asks the server on this container: 0 when it answers 200."""
    import urllib.error
    import urllib.request
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
    import subprocess
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
