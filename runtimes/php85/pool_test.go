package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

// The test binary stands for PHP when HP_FAKE_PHP is set: it speaks the
// worker's side of the protocol, each path of a call showing one thing.
func TestMain(m *testing.M) {
	if os.Getenv("HP_FAKE_PHP") == "1" {
		fakePHP()
		return
	}
	os.Exit(m.Run())
}

func fakePHP() {
	calls := bufio.NewReader(os.NewFile(3, "calls"))
	replies := os.NewFile(4, "replies")
	reply := func(head map[string]any, body string) {
		head["bodyLength"] = len(body)
		line, _ := json.Marshal(head)
		_, _ = replies.Write(append(append(line, '\n'), body...))
	}
	switch os.Getenv("HP_FAKE_LOAD") {
	case "fail":
		reply(map[string]any{"loaded": false, "error": "/app/index.php has no function handler"}, "")
		os.Exit(3)
	case "die":
		os.Exit(255)
	}
	reply(map[string]any{"loaded": true}, "")
	for {
		line, err := calls.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var call callMessage
		_ = json.Unmarshal(line, &call)
		body := make([]byte, call.BodyLength)
		_, _ = io.ReadFull(calls, body)
		switch call.Path {
		case "/die":
			os.Exit(1)
		case "/slow":
			time.Sleep(time.Minute)
		case "/print":
			_, _ = fmt.Fprintln(os.Stdout, "a line the handler printed")
			_, _ = fmt.Fprint(os.Stdout, "one it did not finish")
		case "/fail":
			_, _ = fmt.Fprintf(os.Stdout, "%s%d\n", outputSync, call.Seq)
			reply(map[string]any{"seq": call.Seq, "error": "RuntimeException: boom"}, "")
			continue
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s%d\n", outputSync, call.Seq)
		reply(map[string]any{"seq": call.Seq, "status": 200,
			"headers": map[string][]string{"content-type": {"text/plain"}}},
			fmt.Sprintf("%d %s %s", os.Getpid(), call.Path, body))
	}
}

// lines is the runtime's output, as the pool passes it on.
type lines struct {
	mu        sync.Mutex
	out, errs []string
}

func (l *lines) Output(line string)      { l.add(&l.out, line) }
func (l *lines) ErrorOutput(line string) { l.add(&l.errs, line) }

func (l *lines) add(to *[]string, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*to = append(*to, line)
}

func (l *lines) has(errs bool, part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.out
	if errs {
		list = l.errs
	}
	for _, line := range list {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func testPool(t *testing.T, minProcs, maxProcs int, env ...string) (*pool, *lines) {
	t.Helper()
	p := newPool(poolConfig{min: minProcs, max: maxProcs, loadTimeout: 5 * time.Second,
		worker: workerConfig{command: []string{os.Args[0]}, dir: t.TempDir(),
			env: append(append(os.Environ(), "HP_FAKE_PHP=1"), env...)}})
	out := &lines{}
	p.out = out
	t.Cleanup(func() { p.stop(time.Second) })
	return p, out
}

func callPool(p *pool, path string, timeout time.Duration) (*hivepaas.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = hivepaas.WithInvocation(ctx, "req-1", nil)
	return p.handle(ctx, &hivepaas.Request{Method: http.MethodPost, Path: path, Body: []byte("payload"),
		Headers: http.Header{"X-Custom": {"v"}}})
}

func pidOf(t *testing.T, resp *hivepaas.Response) string {
	t.Helper()
	pid, _, _ := strings.Cut(string(resp.Body), " ")
	return pid
}

// A call reaches a process with its body, and what it printed is passed on
// before it is answered: a line it did not finish too.
func TestACallRunsInAPHPProcess(t *testing.T) {
	p, out := testPool(t, 1, 1)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}

	resp, err := callPool(p, "/print", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || !strings.HasSuffix(string(resp.Body), "/print payload") ||
		resp.Headers.Get("Content-Type") != "text/plain" {
		t.Errorf("%d %q %v", resp.Status, resp.Body, resp.Headers)
	}
	if !out.has(false, "a line the handler printed") || !out.has(false, "one it did not finish") {
		t.Errorf("not passed on before the answer: %v", out.out)
	}
	if out.has(false, outputSync) {
		t.Errorf("a sync line was passed on: %v", out.out)
	}
}

// What the handler raised fails the call with its words.
func TestAFailedCallSaysWhy(t *testing.T) {
	p, _ := testPool(t, 1, 1)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	if _, err := callPool(p, "/fail", 5*time.Second); err == nil || err.Error() != "RuntimeException: boom" {
		t.Errorf("error %v", err)
	}
	if resp, err := callPool(p, "/text", 5*time.Second); err != nil || resp.Status != 200 {
		t.Errorf("the next call: %v", err)
	}
}

// A process that ends in a call fails the call, says so, and is replaced.
func TestAProcessThatDiesInACallIsReplaced(t *testing.T) {
	p, out := testPool(t, 1, 1)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	first, err := callPool(p, "/text", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = callPool(p, "/die", 5*time.Second)

	if err == nil || !strings.Contains(err.Error(), "the PHP process exited (code 1)") {
		t.Errorf("error %v", err)
	}
	waitFor(t, func() bool { return out.has(true, "exited (code 1); starting another") })
	second, err := callPool(p, "/text", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if pidOf(t, second) == pidOf(t, first) {
		t.Error("the dead process answered")
	}
}

// A call that outlives its timeout stops its process, which is replaced at
// once.
func TestACallPastItsTimeoutStopsItsProcess(t *testing.T) {
	p, out := testPool(t, 1, 1)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()

	_, err := callPool(p, "/slow", 200*time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("error %v after %v", err, time.Since(start))
	}
	if !out.has(true, "stopped: its call outlived the timeout") {
		t.Errorf("standard error: %v", out.errs)
	}
	if resp, err := callPool(p, "/text", 5*time.Second); err != nil || resp.Status != 200 {
		t.Errorf("the next call: %v", err)
	}
}

// A handler that cannot be loaded: start says why, a call is not loaded, and
// no other process is started.
func TestAHandlerThatCannotBeLoaded(t *testing.T) {
	for load, says := range map[string]string{
		"fail": "/app/index.php has no function handler",
		"die":  "the PHP process exited (code 255)",
	} {
		t.Run(load, func(t *testing.T) {
			p, _ := testPool(t, 2, 2, "HP_FAKE_LOAD="+load)

			if err := p.start(); err == nil || !strings.Contains(err.Error(), says) {
				t.Fatalf("start: %v", err)
			}
			if p.isLoaded() {
				t.Error("said loaded")
			}
			if _, err := callPool(p, "/text", time.Second); !errors.Is(err, serve.ErrNotLoaded) {
				t.Errorf("a call: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.live)+p.starting != 0 {
				t.Errorf("%d processes, %d starting", len(p.live), p.starting)
			}
		})
	}
}

// Processes are started as calls need them, up to max, and those the calls
// no longer need are stopped, down to min.
func TestTheProcessesFollowTheCalls(t *testing.T) {
	p, _ := testPool(t, 1, 3)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 { // one more than max: it waits for a process
		wg.Go(func() { _, _ = callPool(p, "/slow", 500*time.Millisecond) })
	}
	time.Sleep(300 * time.Millisecond)
	p.mu.Lock()
	grown := len(p.live) + p.starting
	p.mu.Unlock()
	wg.Wait()
	if grown != 3 {
		t.Errorf("%d processes for 4 calls, max 3", grown)
	}

	waitFor(t, func() bool { // the timed out processes are replaced, up to min
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.idle) >= 1 && p.starting == 0
	})
	for range 3 {
		if _, err := callPool(p, "/text", 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	p.shrink() // the calls just made needed one at a time
	p.shrink()
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.live) == 1
	})
}

// Several idle processes take the calls in turn.
func TestIdleProcessesTakeCallsInTurn(t *testing.T) {
	p, _ := testPool(t, 3, 3)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.idle) == 3
	})
	seen := map[string]bool{}
	for range 6 {
		resp, err := callPool(p, "/text", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		seen[pidOf(t, resp)] = true
	}
	if len(seen) != 3 {
		t.Errorf("%d processes answered, want 3", len(seen))
	}
}

// Stopping ends every process.
func TestStoppingEndsTheProcesses(t *testing.T) {
	p, _ := testPool(t, 2, 2)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.idle) == 2
	})
	p.mu.Lock()
	workers := append([]*worker{}, p.idle...)
	p.mu.Unlock()

	// The race detector's binary, which stands for PHP here, takes its time to
	// exit.
	p.stop(10 * time.Second)

	for _, w := range workers {
		select {
		case <-w.exited:
			if w.state != "code 0" {
				t.Errorf("process %d: %s", w.pid, w.state)
			}
		case <-time.After(time.Second):
			t.Errorf("process %d did not exit", w.pid)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("never so")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
