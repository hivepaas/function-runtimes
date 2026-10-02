//go:build conformance

package conformance

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fresh calls the fixture on a connection of its own, as a new client does:
// the connection is taken by whichever process accepts it first.
func (c *container) fresh(path string) response {
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(c.url + path)
	if err != nil {
		return response{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b, err: err}
}

// pids are the processes that answer /pid, asked over new connections until
// want of them answered.
func (c *container) pids(t *testing.T, want int) map[int]bool {
	t.Helper()
	seen := map[int]bool{}
	for range 300 {
		r := c.fresh("/pid")
		if r.err != nil || r.status != 200 {
			t.Fatalf("/pid: %d %q %v", r.status, r.body, r.err)
		}
		seen[int(r.json(t)["pid"].(float64))] = true
		if len(seen) >= want {
			break
		}
	}
	if len(seen) != want {
		t.Fatalf("%d processes answered, want %d: %v", len(seen), want, seen)
	}
	return seen
}

// checkProcesses checks a runtime that serves with several processes.
func checkProcesses(t *testing.T, imgs images) {
	t.Run("WEB_CONCURRENCY sets how many processes serve", func(t *testing.T) {
		c := imgs.serve(t, "WEB_CONCURRENCY=3")

		c.pids(t, 3)
		c.waitForLog(t, "stderr", func(line string) bool { return line == "hivepaas: serving with 3 processes" })
	})

	t.Run("the processes count their calls together", func(t *testing.T) {
		c := imgs.serve(t, "WEB_CONCURRENCY=2", "HP_FN_MAX_CONCURRENCY=2", "HP_FN_TIMEOUT_MS=5000")
		first, second := c.background("/slow?ms=2000"), c.background("/slow?ms=2000")
		time.Sleep(700 * time.Millisecond)

		for i := range 6 {
			if r := c.fresh("/text"); r.err != nil || r.status != http.StatusTooManyRequests {
				t.Errorf("call %d: %d %v", i, r.status, r.err)
			}
		}
		for _, slow := range []<-chan response{first, second} {
			if r := <-slow; r.err != nil || r.status != 200 {
				t.Errorf("a running call: %d %v", r.status, r.err)
			}
		}
	})

	t.Run("a process that dies is replaced", func(t *testing.T) {
		c := imgs.serve(t, "WEB_CONCURRENCY=2")
		before := c.pids(t, 2)
		var killed int
		for pid := range before {
			killed = pid
			break
		}
		docker(t, "exec", c.id, "sh", "-c", "kill -9 "+strconv.Itoa(killed)) // kill is the shell's own

		c.waitForLog(t, "stderr", func(line string) bool {
			return strings.HasPrefix(line, "hivepaas: process "+strconv.Itoa(killed)+" exited (signal 9)")
		})
		deadline := time.Now().Add(10 * time.Second)
		for {
			after := c.pids(t, 1)
			newcomer := false
			for pid := range after {
				if pid == killed {
					t.Fatalf("the killed process %d answered", killed)
				}
				newcomer = newcomer || !before[pid]
			}
			if newcomer {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no process took the dead one's place")
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	t.Run("stopping several processes lets a running call finish", func(t *testing.T) {
		stopped := imgs.serve(t, "WEB_CONCURRENCY=3", "HP_FN_TIMEOUT_MS=5000")
		slow := stopped.background("/slow?ms=1000")
		time.Sleep(300 * time.Millisecond)

		start := time.Now()
		docker(t, "stop", "--time", "10", stopped.id)

		if took := time.Since(start); took > 4*time.Second {
			t.Errorf("stopping took %v, for a call of 1s", took)
		}
		if r := <-slow; r.err != nil || r.status != 200 || string(r.body) != "slow done" {
			t.Errorf("the running call: %d %q %v", r.status, r.body, r.err)
		}
		if code := strings.TrimSpace(docker(t, "inspect", "--format", "{{.State.ExitCode}}", stopped.id)); code != "0" {
			t.Errorf("exit code %s", code)
		}
	})
}
