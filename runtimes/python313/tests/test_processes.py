"""How many processes serve, and the count of running calls they share."""

import os
import tempfile
import unittest

from hivepaas_runtime.processes import SharedPlaces, process_count
from hivepaas_runtime.runtime import ConfigError, load_config

MIB = 1024 * 1024


class ProcessCountTest(unittest.TestCase):
    def cgroup(self, files):
        """A cgroup directory holding files, by path."""
        root = tempfile.mkdtemp()
        for path, text in files.items():
            full = os.path.join(root, path)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "w") as f:
                f.write(text + "\n")
        return root

    def count(self, files=None, env=None, cpus=8, max_concurrency="16"):
        cfg = load_config({"HP_FN_MAX_CONCURRENCY": max_concurrency})
        return process_count(env or {}, cfg, cgroup=self.cgroup(files or {}), cpu_count=lambda: cpus)

    def test_without_limits_one_process_per_cpu(self):
        self.assertEqual(self.count({"cpu.max": "max 100000", "memory.max": "max"}), 8)
        self.assertEqual(self.count(), 8, "no cgroup files")

    def test_a_cpu_quota_rounded_up(self):
        self.assertEqual(self.count({"cpu.max": "150000 100000"}), 2)
        self.assertEqual(self.count({"cpu.max": "50000 100000"}), 1)
        self.assertEqual(self.count({"cpu/cpu.cfs_quota_us": "300000", "cpu/cpu.cfs_period_us": "100000"}), 3)
        self.assertEqual(self.count({"cpu/cpu.cfs_quota_us": "-1", "cpu/cpu.cfs_period_us": "100000"}), 8)

    def test_at_most_the_concurrency(self):
        self.assertEqual(self.count(cpus=64), 16)
        self.assertEqual(self.count(max_concurrency="3"), 3)

    def test_at_most_one_per_128_mib_of_a_memory_limit(self):
        self.assertEqual(self.count({"memory.max": str(256 * MIB)}), 2)
        self.assertEqual(self.count({"memory.max": str(100 * MIB)}), 1)
        self.assertEqual(self.count({"memory/memory.limit_in_bytes": str(512 * MIB)}), 4)
        self.assertEqual(self.count({"memory/memory.limit_in_bytes": "9223372036854771712"}), 8, "v1's unlimited")

    def test_web_concurrency_sets_it_within_the_concurrency(self):
        self.assertEqual(self.count(env={"WEB_CONCURRENCY": "3"}, cpus=1), 3)
        self.assertEqual(self.count(env={"WEB_CONCURRENCY": "1"}), 1)
        self.assertEqual(self.count(env={"WEB_CONCURRENCY": "40"}), 16)
        self.assertEqual(self.count(env={"WEB_CONCURRENCY": ""}), 8, "empty is not set")
        for value in ["0", "-2", "auto", "1.5"]:
            with self.assertRaisesRegex(ConfigError, "WEB_CONCURRENCY"):
                self.count(env={"WEB_CONCURRENCY": value})


class SharedPlacesTest(unittest.TestCase):
    def test_the_workers_count_their_calls_together(self):
        shared = SharedPlaces(2)
        first, second = shared.of(0), shared.of(1)
        self.assertTrue(first.take(2))
        self.assertTrue(second.take(2))
        self.assertFalse(first.take(2))
        self.assertFalse(second.take(2))
        second.give()
        self.assertTrue(first.take(2))
        self.assertEqual(shared.total(), 2)

    def test_a_dead_workers_places_are_given_back(self):
        shared = SharedPlaces(2)
        self.assertTrue(shared.of(1).take(1))
        self.assertFalse(shared.of(0).take(1))
        shared.reset(1)
        self.assertTrue(shared.of(0).take(1))

    def test_a_forked_worker_counts_in_the_memory_the_first_process_reads(self):
        shared = SharedPlaces(2)
        pid = os.fork()
        if pid == 0:
            os._exit(0 if shared.of(1).take(5) else 1)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 0)
        self.assertEqual(shared.total(), 1)


if __name__ == "__main__":
    unittest.main()
