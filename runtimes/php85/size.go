package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

// cgroupRoot is where the container's limits are read.
const cgroupRoot = "/sys/fs/cgroup"

// memoryPerProcess is the part of a memory limit each PHP process is given at
// least: a 256 MiB function serves with up to 8.
const memoryPerProcess = 32 << 20

// idleTimeout is how often the processes the calls no longer need are
// stopped.
const idleTimeout = 30 * time.Second

var errNotPositive = errors.New("not a positive whole number")

// unlimited: a cgroup v1 limit this large is no limit.
const unlimited = 1 << 60

// size is how many PHP processes serve: min kept, up to max started as calls
// need them.
type size struct {
	min, max int
}

func (s size) String() string {
	if s.min == s.max {
		return processes(s.max)
	}
	return fmt.Sprintf("%d to %s, as many as the calls need", s.min, processes(s.max))
}

func processes(n int) string {
	if n == 1 {
		return "1 process"
	}
	return strconv.Itoa(n) + " processes"
}

// sizeFor is how many processes serve. WEB_CONCURRENCY, when it is set, is
// that many, all started. Otherwise one is kept and more are started as calls
// need them, up to HP_FN_MAX_CONCURRENCY and one per memoryPerProcess of the
// container's memory limit: a PHP process runs one call at a time.
func sizeFor(getenv func(string) string, cfg serve.Config, cgroup string) (size, error) {
	if given := getenv("WEB_CONCURRENCY"); given != "" {
		n, err := strconv.Atoi(given)
		if err != nil || n <= 0 {
			return size{}, fmt.Errorf("WEB_CONCURRENCY is %q: %w", given, errNotPositive)
		}
		n = min(n, cfg.MaxConcurrency)
		return size{min: n, max: n}, nil
	}
	most := cfg.MaxConcurrency
	if limit, ok := memoryLimit(cgroup); ok {
		most = max(1, min(most, int(limit/memoryPerProcess)))
	}
	return size{min: 1, max: most}, nil
}

// memoryLimit is the container's memory limit in bytes, when it has one.
func memoryLimit(cgroup string) (int64, bool) {
	value, err := os.ReadFile(filepath.Join(cgroup, "memory.max"))
	if err != nil {
		value, err = os.ReadFile(filepath.Join(cgroup, "memory", "memory.limit_in_bytes"))
	}
	if err != nil {
		return 0, false
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
	if err != nil || limit <= 0 || limit >= unlimited { // "max", or no limit
		return 0, false
	}
	return limit, true
}
