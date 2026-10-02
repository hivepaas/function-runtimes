"""serve with several processes: how many, the running calls they count
together, and the first process, which starts them and watches over them."""

import math
import mmap
import os
import signal
import sys
import threading
import time
import traceback
from collections.abc import Callable, Mapping

from . import server
from .runtime import Config, Runtime, positive_whole_number

CGROUP = "/sys/fs/cgroup"
# A process is given at least this much of a memory limit.
MEMORY_PER_PROCESS = 128 * 1024 * 1024
# A cgroup v1 limit this large is no limit.
_UNLIMITED = 1 << 60
# A worker that exits is started again at most this often.
RESTART_EVERY_S = 1
# How long past the timeout a stopping worker is waited for before it is killed.
KILL_AFTER_S = 5
_POLL_S = 0.05


def process_count(env: Mapping[str, str], cfg: Config, cgroup: str = CGROUP,
                  cpu_count: Callable[[], int | None] = os.process_cpu_count) -> int:
    """How many processes serve: WEB_CONCURRENCY when it is set; otherwise the
    CPUs the container may use, within one per MEMORY_PER_PROCESS of its memory
    limit. Never more than HP_FN_MAX_CONCURRENCY, never fewer than one."""
    given = env.get("WEB_CONCURRENCY")
    if given:
        return min(positive_whole_number("WEB_CONCURRENCY", given), cfg.max_concurrency)
    count = min(_cpu_quota(cgroup) or cpu_count() or 1, cfg.max_concurrency)
    memory = _memory_limit(cgroup)
    if memory is not None:
        count = min(count, memory // MEMORY_PER_PROCESS)
    return max(1, count)


def _read(cgroup: str, path: str) -> str | None:
    try:
        with open(os.path.join(cgroup, path)) as f:
            return f.read().strip()
    except OSError:
        return None


def _cpu_quota(cgroup: str) -> int | None:
    """The CPUs of the cgroup's quota, rounded up; None without a quota."""
    v2 = _read(cgroup, "cpu.max")
    if v2 is not None:
        quota, _, period = v2.partition(" ")
    else:
        quota, period = _read(cgroup, "cpu/cpu.cfs_quota_us"), _read(cgroup, "cpu/cpu.cfs_period_us")
    try:
        quota_us, period_us = int(quota), int(period)
    except (TypeError, ValueError):
        return None  # "max", or nothing to read
    if quota_us <= 0 or period_us <= 0:
        return None
    return math.ceil(quota_us / period_us)


def _memory_limit(cgroup: str) -> int | None:
    """The cgroup's memory limit in bytes; None without one."""
    value = _read(cgroup, "memory.max")
    if value is None:
        value = _read(cgroup, "memory/memory.limit_in_bytes")
    try:
        limit = int(value)
    except (TypeError, ValueError):
        return None  # "max", or nothing to read
    return limit if 0 < limit < _UNLIMITED else None


class SharedPlaces:
    """The running calls of each worker, in memory every worker shares: a worker
    writes its own count only, and reads the others'. Two workers taking the
    last place at the same instant may both have it; there is no lock a dying
    worker could keep."""

    def __init__(self, workers: int):
        self._memory = mmap.mmap(-1, 8 * workers)  # anonymous and shared: forked workers see it
        self.counts = memoryview(self._memory).cast("q")

    def of(self, index: int) -> "WorkerPlaces":
        return WorkerPlaces(self, index)

    def reset(self, index: int) -> None:
        """Gives back a worker's places: it is dead, or not started yet."""
        self.counts[index] = 0

    def total(self) -> int:
        return sum(self.counts)


class WorkerPlaces:
    """One worker's view of SharedPlaces, what its Runtime takes places from."""

    def __init__(self, shared: SharedPlaces, index: int):
        self._shared = shared
        self._index = index
        self._lock = threading.Lock()  # the loop takes, the handlers' threads give

    def take(self, limit: int) -> bool:
        with self._lock:
            if self._shared.total() >= limit:
                return False
            self._shared.counts[self._index] += 1
            return True

    def give(self) -> None:
        with self._lock:
            self._shared.counts[self._index] -= 1


def serve(cfg: Config, count: int) -> int:
    """Answers on HP_FN_PORT with count processes until SIGTERM. One process
    serves itself; several are workers of this one."""
    sock = server.listen(cfg.port)
    print(f"hivepaas: serving with {count} process{'' if count == 1 else 'es'}", file=sys.stderr, flush=True)
    if count == 1:
        return server.serve(Runtime.load(cfg), sock)
    return _Supervisor(cfg, sock, count).run()


class _Supervisor:
    """The first process: it holds the socket, starts the workers, starts again
    one that exits, and stops them all on SIGTERM."""

    def __init__(self, cfg: Config, sock, count: int):
        self.cfg = cfg
        self.sock = sock
        self.count = count
        self.places = SharedPlaces(count)
        self.workers: dict[int, int] = {}  # pid -> index
        self.started = [0.0] * count
        self.stopping = False

    def run(self) -> int:
        for signum in (signal.SIGTERM, signal.SIGINT):
            signal.signal(signum, self._stop)
        for index in range(self.count):
            self._start(index)
        waiting: list[int] = []  # workers to start again
        while not self.stopping:
            for index in self._reap():
                waiting.append(index)
            now = time.monotonic()
            for index in [i for i in waiting if now - self.started[i] >= RESTART_EVERY_S]:
                waiting.remove(index)
                self._start(index)
            time.sleep(_POLL_S)
        return self._stop_workers()

    def _stop(self, *_):
        self.stopping = True

    def _start(self, index: int) -> None:
        self.places.reset(index)
        self.started[index] = time.monotonic()
        sys.stdout.flush()
        sys.stderr.flush()
        pid = os.fork()
        if pid == 0:
            os._exit(self._work(index))
        self.workers[pid] = index

    def _work(self, index: int) -> int:
        """A worker: the handler is loaded here, after the fork, never before it."""
        code = 1
        try:
            for signum in (signal.SIGTERM, signal.SIGINT):
                signal.signal(signum, signal.SIG_DFL)
            runtime = Runtime.load(self.cfg, places=self.places.of(index))
            code = server.serve(runtime, self.sock)
        except BaseException:
            traceback.print_exc()
        finally:
            sys.stdout.flush()
            sys.stderr.flush()
        return code

    def _reap(self) -> list[int]:
        """The workers that exited since the last look, by index; each said on stderr."""
        exited = []
        while self.workers:
            try:
                pid, status = os.waitpid(-1, os.WNOHANG)
            except ChildProcessError:
                break
            if pid == 0:
                break
            index = self.workers.pop(pid, None)
            if index is None:
                continue
            self.places.reset(index)
            if not self.stopping:
                print(f"hivepaas: process {pid} exited ({_how(status)}); starting another",
                      file=sys.stderr, flush=True)
            exited.append(index)
        return exited

    def _stop_workers(self) -> int:
        self.sock.close()
        for pid in list(self.workers):
            _kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + self.cfg.timeout_ms / 1000 + KILL_AFTER_S
        while self.workers and time.monotonic() < deadline:
            self._reap()
            time.sleep(_POLL_S)
        for pid in list(self.workers):
            _kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        return 0


def _kill(pid: int, signum: int) -> None:
    try:
        os.kill(pid, signum)
    except ProcessLookupError:
        pass


def _how(status: int) -> str:
    if os.WIFSIGNALED(status):
        return f"signal {os.WTERMSIG(status)}"
    return f"code {os.waitstatus_to_exitcode(status)}"
