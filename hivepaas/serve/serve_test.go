package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

// syncBuffer is a log both the runtime and a handler's goroutine write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines are the JSON log lines of a given kind ("invocation", "log").
func (b *syncBuffer) lines(kind string) []map[string]any {
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["hp"] == kind {
			found = append(found, v)
		}
	}
	return found
}

// outcome is the outcome of the only call in the log.
func (b *syncBuffer) outcome(t *testing.T) any {
	t.Helper()
	lines := b.lines("invocation")
	if len(lines) != 1 {
		t.Fatalf("invocation lines = %v", lines)
	}
	return lines[0]["outcome"]
}

var testConfig = Config{Port: 8080, Timeout: time.Second, MaxConcurrency: 4, MaxBodySize: 64}

func newTestRuntime(h hivepaas.HandlerFunc, cfg Config) (*Runtime, *syncBuffer, *syncBuffer) {
	out, errOut := &syncBuffer{}, &syncBuffer{}
	return New(h, cfg, out, errOut), out, errOut
}

func noContent(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
	return nil, nil
}

func call(rt *Runtime, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	return v
}

func TestTheHandlerSeesTheRequestAsSent(t *testing.T) {
	var got *hivepaas.Request
	rt, _, _ := newTestRuntime(func(_ context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
		got = req
		return nil, nil
	}, testConfig)

	call(rt, "post", "/echo/a%20b?a=1&a=2&b=x+y", "payload", map[string]string{"X-Custom": "v"})

	if got == nil {
		t.Fatal("the handler was not called")
	}
	if got.Method != "POST" || got.Path != "/echo/a%20b" || got.Text() != "payload" {
		t.Fatalf("request = %s %s %q", got.Method, got.Path, got.Body)
	}
	if len(got.Query["a"]) != 2 || got.Query.Get("a") != "1" || got.Query.Get("b") != "x y" {
		t.Fatalf("query = %v", got.Query)
	}
	if got.Headers.Get("x-custom") != "v" || got.Headers.Get("host") != "example.com" {
		t.Fatalf("headers = %v", got.Headers)
	}
}

func TestAResponseIsSentAsMadeWithTheCallsID(t *testing.T) {
	rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		resp, err := hivepaas.JSON(201, map[string]string{"hello": "Ada"})
		resp.Headers.Set("X-Fixture", "yes")
		return resp, err
	}, testConfig)

	rec := call(rt, "GET", "/json?token=secret", "", map[string]string{"X-Request-Id": "test-123"})

	if rec.Code != 201 || rec.Body.String() != `{"hello":"Ada"}` {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Fixture") != "yes" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if rec.Header().Get("X-Request-Id") != "test-123" {
		t.Fatalf("x-request-id = %q", rec.Header().Get("X-Request-Id"))
	}
	lines := out.lines("invocation")
	if len(lines) != 1 {
		t.Fatalf("invocation lines = %v", lines)
	}
	line := lines[0]
	if line["requestId"] != "test-123" || line["method"] != "GET" || line["path"] != "/json" ||
		line["status"] != float64(201) || line["outcome"] != "ok" {
		t.Fatalf("invocation line = %v", line)
	}
	if _, ok := line["durationMs"].(float64); !ok {
		t.Fatalf("invocation line has no duration: %v", line)
	}
}

func TestARequestIDIsMadeWhenTheRequestHasNoUsableOne(t *testing.T) {
	rt, _, _ := newTestRuntime(noContent, testConfig)
	made := regexp.MustCompile(`^[0-9a-f]{32}$`)

	for _, sent := range []string{"", "has spaces", strings.Repeat("a", 129)} {
		rec := call(rt, "GET", "/", "", map[string]string{"X-Request-Id": sent})
		if id := rec.Header().Get("X-Request-Id"); !made.MatchString(id) {
			t.Errorf("sent %q, got id %q", sent, id)
		}
	}
}

func TestNoResponseIsNoContent(t *testing.T) {
	rt, _, _ := newTestRuntime(noContent, testConfig)

	rec := call(rt, "GET", "/", "", nil)

	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestABodyWithoutAContentTypeIsOctetStream(t *testing.T) {
	rt, _, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return &hivepaas.Response{Body: []byte{0, 1, 2, 255}}, nil
	}, testConfig)

	rec := call(rt, "GET", "/", "", nil)

	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), []byte{0, 1, 2, 255}) {
		t.Fatalf("response = %d %v", rec.Code, rec.Body.Bytes())
	}
	if rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

// A handler may name its headers in any case.
func TestAHandlersHeadersAreSentWhateverTheirCase(t *testing.T) {
	rt, _, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return &hivepaas.Response{Headers: http.Header{"content-type": {"text/csv"}}, Body: []byte("a,b")}, nil
	}, testConfig)

	rec := call(rt, "GET", "/", "", nil)

	if got := rec.Header().Values("Content-Type"); len(got) != 1 || got[0] != "text/csv" {
		t.Fatalf("content type = %v", got)
	}
}

func TestAStatusThatCarriesNoBodySendsNone(t *testing.T) {
	for _, status := range []int{204, 304} {
		rt, _, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
			return hivepaas.Text(status, "ignored"), nil
		}, testConfig)

		rec := call(rt, "GET", "/", "", nil)

		if rec.Code != status || rec.Body.Len() != 0 {
			t.Errorf("response = %d %q", rec.Code, rec.Body.String())
		}
	}
}

// What went wrong is in the log, never in the response.
func TestAHandlerErrorIsAnInternalErrorThatSaysNothing(t *testing.T) {
	cases := map[string]hivepaas.HandlerFunc{
		"an error": func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
			return nil, errors.New("fixture failure")
		},
		"a panic": func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
			panic("fixture failure")
		},
	}
	for _, status := range []int{42, 101, 600} {
		cases[fmt.Sprintf("a status of %d", status)] =
			func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
				return &hivepaas.Response{Status: status, Body: []byte("fixture failure")}, nil
			}
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			rt, out, errOut := newTestRuntime(h, testConfig)

			rec := call(rt, "GET", "/", "", map[string]string{"X-Request-Id": "req-err"})

			if rec.Code != 500 {
				t.Fatalf("status = %d", rec.Code)
			}
			body := decode(t, rec.Body.Bytes())
			if body["error"] != "internal error" || body["requestId"] != "req-err" {
				t.Fatalf("body = %v", body)
			}
			if strings.Contains(rec.Body.String(), "fixture failure") {
				t.Fatal("the error's text was sent")
			}
			if !strings.Contains(errOut.String(), "req-err") {
				t.Fatalf("the error was not logged with the call's id: %q", errOut.String())
			}
			if outcome := out.outcome(t); outcome != "error" {
				t.Fatalf("outcome = %v", outcome)
			}
		})
	}
}

// A call that outlives its timeout is answered at once, and keeps its place
// among the running calls until the handler returns.
func TestACallThatTimesOutIsAnsweredAndStillCounted(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	cfg := testConfig
	cfg.Timeout, cfg.MaxConcurrency = 50*time.Millisecond, 1
	rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		if calls.Add(1) == 1 {
			<-release
		}
		return nil, nil
	}, cfg)

	start := time.Now()
	rec := call(rt, "GET", "/slow", "", nil)

	if rec.Code != 504 || decode(t, rec.Body.Bytes())["error"] != "timeout" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("answered after %v", elapsed)
	}
	if outcome := out.outcome(t); outcome != "timeout" {
		t.Fatalf("outcome = %v", outcome)
	}
	if rec := call(rt, "GET", "/", "", nil); rec.Code != 429 {
		t.Fatalf("a call while the timed-out one still runs = %d", rec.Code)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for call(rt, "GET", "/", "", nil).Code == 429 {
		if time.Now().After(deadline) {
			t.Fatal("the place was not given back when the handler returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheHandlersContextEndsAtTheTimeout(t *testing.T) {
	cfg := testConfig
	cfg.Timeout = 30 * time.Millisecond
	remaining := make(chan time.Duration, 1)
	rt, _, _ := newTestRuntime(func(ctx context.Context, _ *hivepaas.Request) (*hivepaas.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			remaining <- 0
			return nil, nil
		}
		remaining <- time.Until(deadline)
		<-ctx.Done()
		return nil, ctx.Err()
	}, cfg)

	call(rt, "GET", "/", "", nil)

	if r := <-remaining; r <= 0 || r > cfg.Timeout {
		t.Fatalf("remaining = %v", r)
	}
}

func TestCallsBeyondTheConcurrencyLimitAreTurnedAway(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	cfg := testConfig
	cfg.MaxConcurrency = 1
	rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}, cfg)

	done := make(chan int)
	go func() { done <- call(rt, "GET", "/", "", nil).Code }()
	<-started
	rec := call(rt, "GET", "/", "", nil)
	close(release)

	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("second call = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if body := decode(t, rec.Body.Bytes()); body["error"] != "too many requests" {
		t.Fatalf("body = %v", body)
	}
	if code := <-done; code != 204 {
		t.Fatalf("first call = %d", code)
	}
	outcomes := map[any]int{}
	for _, line := range out.lines("invocation") {
		outcomes[line["outcome"]]++
	}
	if outcomes["throttled"] != 1 || outcomes["ok"] != 1 {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestARequestBodyOverTheLimitIsRefused(t *testing.T) {
	for name, length := range map[string]int64{"with its length": 65, "without a length": -1} {
		t.Run(name, func(t *testing.T) {
			called := false
			rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
				called = true
				return nil, nil
			}, testConfig)
			req := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 65)))
			req.ContentLength = length
			rec := httptest.NewRecorder()

			rt.ServeHTTP(rec, req)

			if rec.Code != 413 || called {
				t.Fatalf("response = %d, handler called %v", rec.Code, called)
			}
			if body := decode(t, rec.Body.Bytes()); body["error"] != "request too large" {
				t.Fatalf("body = %v", body)
			}
			if outcome := out.outcome(t); outcome != "request_too_large" {
				t.Fatalf("outcome = %v", outcome)
			}
		})
	}
}

func TestTheWholeBodyReachesTheHandler(t *testing.T) {
	var got []byte
	rt, _, _ := newTestRuntime(func(_ context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
		got = req.Body
		return nil, nil
	}, testConfig)
	body := strings.Repeat("y", int(testConfig.MaxBodySize))

	call(rt, "POST", "/", body, nil)

	if string(got) != body {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A request whose body does not arrive is not a call: the connection is
// dropped, and nothing is logged.
func TestARequestWhoseBodyDoesNotArriveIsDropped(t *testing.T) {
	called := false
	rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		called = true
		return nil, nil
	}, testConfig)
	req := httptest.NewRequest("POST", "/", failingReader{})

	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
		}
		if called || len(out.lines("invocation")) != 0 {
			t.Fatalf("handler called %v, log %q", called, out.String())
		}
	}()
	rt.ServeHTTP(httptest.NewRecorder(), req)
}

// Over a real connection: the runtime closes it at the timeout, rather than
// wait for the rest of the body.
func TestAConnectionWhoseBodyStallsIsClosedAtTheTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig
	cfg.Timeout = 100 * time.Millisecond
	rt, out, _ := newTestRuntime(noContent, cfg)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = rt.Serve(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	start := time.Now()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\nabc")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	answer, _ := io.ReadAll(conn)

	if took := time.Since(start); took > time.Second {
		t.Fatalf("the connection was closed after %v", took)
	}
	if len(answer) != 0 || len(out.lines("invocation")) != 0 {
		t.Fatalf("answered %q, logged %q", answer, out.String())
	}
}

func TestAResponseBodyOverTheLimitIsAnInternalError(t *testing.T) {
	rt, out, errOut := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return hivepaas.Text(200, strings.Repeat("x", int(testConfig.MaxBodySize)+1)), nil
	}, testConfig)

	rec := call(rt, "GET", "/", "", nil)

	if rec.Code != 500 || decode(t, rec.Body.Bytes())["error"] != "internal error" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if outcome := out.outcome(t); outcome != "response_too_large" {
		t.Fatalf("outcome = %v", outcome)
	}
	if !strings.Contains(errOut.String(), "HP_FN_MAX_BODY_SIZE") {
		t.Fatalf("the reason is not in the log: %q", errOut.String())
	}
}

func TestTheRuntimesPathsNeverReachTheHandler(t *testing.T) {
	called := false
	rt, out, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		called = true
		return nil, nil
	}, testConfig)

	health := call(rt, "GET", "/_hivepaas/health", "", nil)
	other := call(rt, "GET", "/_hivepaas/anything", "", nil)

	if health.Code != 200 || decode(t, health.Body.Bytes())["status"] != "ok" {
		t.Fatalf("health = %d %q", health.Code, health.Body.String())
	}
	if other.Code != 404 || decode(t, other.Body.Bytes())["error"] != "not found" {
		t.Fatalf("/_hivepaas/anything = %d %q", other.Code, other.Body.String())
	}
	if called || len(out.lines("invocation")) != 0 {
		t.Fatalf("handler called %v, log %q", called, out.String())
	}
}

func TestAHandlersLogLineCarriesTheCallsID(t *testing.T) {
	rt, out, _ := newTestRuntime(func(ctx context.Context, _ *hivepaas.Request) (*hivepaas.Response, error) {
		hivepaas.Log(ctx, "fixture %s", "log line")
		return nil, nil
	}, testConfig)

	call(rt, "GET", "/", "", map[string]string{"X-Request-Id": "req-log"})

	lines := out.lines("log")
	if len(lines) != 1 || lines[0]["requestId"] != "req-log" || lines[0]["msg"] != "fixture log line" {
		t.Fatalf("log lines = %v", lines)
	}
}

// Stopping waits for the calls that are running, then returns.
func TestStoppingLetsRunningCallsFinish(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	rt, _, _ := newTestRuntime(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		return hivepaas.Text(200, "finished"), nil
	}, testConfig)
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- rt.Serve(ctx, listener) }()
	url := "http://" + listener.Addr().String() + "/"

	replied := make(chan string, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			replied <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		replied <- string(body)
	}()
	<-started
	stop()

	if body := <-replied; body != "finished" {
		t.Fatalf("the running call got %q", body)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve returned %v", err)
	}
	if _, err := net.Dial("tcp", listener.Addr().String()); err == nil {
		t.Fatal("still listening after it stopped")
	}
}
