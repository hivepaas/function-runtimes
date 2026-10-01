//go:build conformance

package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func checkImages(t *testing.T, imgs images) {
	for _, image := range []string{imgs.runtime, imgs.build, imgs.fixture} {
		if image == "" {
			continue
		}
		var cfg struct {
			User         string
			WorkingDir   string
			ExposedPorts map[string]any
			Cmd          []string
			Healthcheck  struct{ Test []string }
		}
		out := docker(t, "image", "inspect", "--format", "{{json .Config}}", image)
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s: %v", image, err)
		}
		if cfg.User != "hivepaas" || cfg.WorkingDir != "/app" {
			t.Errorf("%s: user %q, working directory %q", image, cfg.User, cfg.WorkingDir)
		}
		if _, ok := cfg.ExposedPorts["8080/tcp"]; !ok {
			t.Errorf("%s: exposed ports %v", image, cfg.ExposedPorts)
		}
		if !reflect.DeepEqual(cfg.Cmd, []string{"hivepaas-runtime", "serve"}) {
			t.Errorf("%s: command %q", image, cfg.Cmd)
		}
		if !reflect.DeepEqual(cfg.Healthcheck.Test, []string{"CMD", "hivepaas-runtime", "health"}) {
			t.Errorf("%s: health check %q", image, cfg.Healthcheck.Test)
		}
	}
}

func checkServe(t *testing.T, rt runtimeDef, imgs images) {
	c := startServe(t, imgs.fixture, "HP_FN_TIMEOUT_MS=1000", "HP_FN_MAX_BODY_SIZE=4096", "GREETING=hi")

	t.Run("a body is sent with the type it has", func(t *testing.T) {
		for _, tc := range []struct{ path, contentType, body string }{
			{"/json?name=Ada", "application/json", `{"hello":"Ada"}`},
			{"/text", "text/plain; charset=utf-8", "hello"},
			{"/bytes", "application/octet-stream", "\x00\x01\x02\xff"},
		} {
			r := c.get(t, tc.path)
			if r.status != http.StatusOK || r.header.Get("Content-Type") != tc.contentType {
				t.Errorf("%s: %d %q", tc.path, r.status, r.header.Get("Content-Type"))
			}
			if !sameBody(r.body, []byte(tc.body)) {
				t.Errorf("%s: body %q, want %q", tc.path, r.body, tc.body)
			}
		}
	})

	t.Run("the handler sees the request as sent", func(t *testing.T) {
		r := c.call(t, http.MethodPost, "/echo?a=1&a=2", []byte("payload"),
			map[string]string{"X-Custom": "v", "X-Request-Id": "conf-echo"})

		want := map[string]any{
			"method": "POST", "path": "/echo", "query": "1", "queryAll": []any{"1", "2"},
			"header": "v", "body": "payload", "requestId": "conf-echo",
		}
		if got := r.json(t); r.status != http.StatusOK || !reflect.DeepEqual(got, want) {
			t.Errorf("%d %v, want %v", r.status, got, want)
		}
	})

	t.Run("a status of the handler's own, or no content", func(t *testing.T) {
		if r := c.get(t, "/status?code=201"); r.status != http.StatusCreated {
			t.Errorf("/status?code=201: %d", r.status)
		}
		if r := c.get(t, "/"); r.status != http.StatusNoContent || len(r.body) != 0 {
			t.Errorf("/: %d %q", r.status, r.body)
		}
		r := c.call(t, http.MethodHead, "/text", nil, nil)
		if r.status != http.StatusOK || len(r.body) != 0 || r.header.Get("Content-Length") != "5" {
			t.Errorf("HEAD /text: %d %q, length %q", r.status, r.body, r.header.Get("Content-Length"))
		}
	})

	t.Run("an error says nothing of itself", func(t *testing.T) {
		r := c.call(t, http.MethodGet, "/error", nil, map[string]string{"X-Request-Id": "conf-error"})

		if body := r.json(t); r.status != 500 || body["error"] != "internal error" || body["requestId"] != "conf-error" {
			t.Errorf("%d %v", r.status, body)
		}
		if bytes.Contains(r.body, []byte("fixture failure")) {
			t.Error("the error's text was sent")
		}
		c.waitForLog(t, "stderr", func(line string) bool {
			return strings.Contains(line, "hivepaas: call conf-error failed") && strings.Contains(line, "fixture failure")
		})
		if line := c.jsonLine(t, "invocation", "conf-error"); line["outcome"] != "error" || line["status"] != 500.0 {
			t.Errorf("invocation line %v", line)
		}
	})

	t.Run("a status that is not one is an error", func(t *testing.T) {
		if r := c.get(t, "/invalid"); r.status != 500 || r.json(t)["error"] != "internal error" {
			t.Errorf("%d %q", r.status, r.body)
		}
	})

	t.Run("a call is answered at its timeout", func(t *testing.T) {
		start := time.Now()
		r := c.call(t, http.MethodGet, "/slow?ms=3000", nil, map[string]string{"X-Request-Id": "conf-slow"})

		if r.status != http.StatusGatewayTimeout || r.json(t)["error"] != "timeout" {
			t.Errorf("%d %q", r.status, r.body)
		}
		if took := time.Since(start); took > 2500*time.Millisecond {
			t.Errorf("answered after %v", took)
		}
		if line := c.jsonLine(t, "invocation", "conf-slow"); line["outcome"] != "timeout" {
			t.Errorf("invocation line %v", line)
		}
	})

	t.Run("bodies over the limit", func(t *testing.T) {
		over := bytes.Repeat([]byte("x"), 4097)
		r := c.call(t, http.MethodPost, "/echo", over, map[string]string{"X-Request-Id": "conf-413"})
		if r.status != http.StatusRequestEntityTooLarge || r.json(t)["error"] != "request too large" {
			t.Errorf("a request body over the limit: %d %q", r.status, r.body)
		}
		if line := c.jsonLine(t, "invocation", "conf-413"); line["outcome"] != "request_too_large" {
			t.Errorf("invocation line %v", line)
		}

		r = c.call(t, http.MethodGet, "/big?bytes=4097", nil, map[string]string{"X-Request-Id": "conf-big"})
		if r.status != 500 || r.json(t)["error"] != "internal error" {
			t.Errorf("a response body over the limit: %d %q", r.status, r.body)
		}
		if line := c.jsonLine(t, "invocation", "conf-big"); line["outcome"] != "response_too_large" {
			t.Errorf("invocation line %v", line)
		}

		if r := c.call(t, http.MethodPost, "/text", bytes.Repeat([]byte("x"), 4096), nil); r.status != 200 {
			t.Errorf("a request body at the limit: %d", r.status)
		}
	})

	t.Run("a body sent in chunks", func(t *testing.T) {
		r := c.chunked(t, "/echo", []byte("payload"))
		if r.status != http.StatusOK || r.json(t)["body"] != "payload" {
			t.Errorf("a body in chunks: %d %q", r.status, r.body)
		}
		if r := c.chunked(t, "/text", bytes.Repeat([]byte("x"), 4096)); r.status != http.StatusOK {
			t.Errorf("a body in chunks at the limit: %d", r.status)
		}
		if r := c.chunked(t, "/text", bytes.Repeat([]byte("x"), 4097)); r.status != http.StatusRequestEntityTooLarge {
			t.Errorf("a body in chunks over the limit: %d %q", r.status, r.body)
		}
	})

	t.Run("a request whose body does not arrive is dropped", func(t *testing.T) {
		conn, err := net.Dial("tcp", strings.TrimPrefix(c.url, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		start := time.Now()
		_, err = io.WriteString(conn, "POST /echo HTTP/1.1\r\nHost: fixture\r\nX-Request-Id: conf-stalled\r\n"+
			"Content-Length: 10\r\n\r\nabc")
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		answer, _ := io.ReadAll(conn) // until the runtime closes the connection

		if len(answer) != 0 {
			t.Errorf("answered %q", answer)
		}
		if took := time.Since(start); took < 900*time.Millisecond || took > 4*time.Second {
			t.Errorf("the connection was closed after %v; the timeout is 1s", took)
		}
		time.Sleep(200 * time.Millisecond)
		if c.hasLog(t, "stdout", func(line string) bool { return strings.Contains(line, "conf-stalled") }) {
			t.Error("a call was logged")
		}
	})

	t.Run("the function's environment and user", func(t *testing.T) {
		if r := c.get(t, "/env"); string(r.body) != "hi" {
			t.Errorf("/env: %q", r.body)
		}
		if body := c.get(t, "/whoami").json(t); body["uid"] != 10001.0 || body["gid"] != 10001.0 {
			t.Errorf("/whoami: %v", body)
		}
		if r := c.get(t, "/lib"); string(r.body) != "from lib" {
			t.Errorf("/lib: %d %q", r.status, r.body)
		}
		remaining, _ := c.get(t, "/deadline").json(t)["remainingMs"].(float64)
		if remaining <= 0 || remaining > 1000 {
			t.Errorf("/deadline: %v ms left", remaining)
		}
	})

	t.Run("every call has an id and a log line", func(t *testing.T) {
		r := c.call(t, http.MethodGet, "/json?token=secret", nil, map[string]string{"X-Request-Id": "conf-id.1_A"})
		if got := r.header.Get("X-Request-Id"); got != "conf-id.1_A" {
			t.Errorf("x-request-id %q", got)
		}
		line := c.jsonLine(t, "invocation", "conf-id.1_A")
		if line["method"] != "GET" || line["path"] != "/json" || line["status"] != 200.0 || line["outcome"] != "ok" {
			t.Errorf("invocation line %v", line)
		}
		if _, ok := line["durationMs"].(float64); !ok {
			t.Errorf("invocation line without a duration: %v", line)
		}

		made := c.call(t, http.MethodGet, "/text", nil, map[string]string{"X-Request-Id": "not an id"})
		if id := made.header.Get("X-Request-Id"); !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(id) ||
			id == "not an id" {
			t.Errorf("made id %q", id)
		}
	})

	t.Run("a handler's log line carries its call's id", func(t *testing.T) {
		c.call(t, http.MethodGet, "/log", nil, map[string]string{"X-Request-Id": "conf-log"})
		if line := c.jsonLine(t, "log", "conf-log"); line["msg"] != "fixture log line" {
			t.Errorf("log line %v", line)
		}
	})

	// A warm call takes about a millisecond here; a server that waits for the
	// client's delayed ACK before the body takes some 40.
	t.Run("a warm call is answered without delay", func(t *testing.T) {
		took := make([]time.Duration, 21)
		for i := range took {
			start := time.Now()
			c.get(t, "/text")
			took[i] = time.Since(start)
		}
		slices.Sort(took)
		if median := took[len(took)/2]; median > 20*time.Millisecond {
			t.Errorf("the median call took %v", median)
		}
	})

	t.Run("the runtime's paths", func(t *testing.T) {
		if r := c.get(t, "/_hivepaas/health"); r.status != 200 || r.json(t)["status"] != "ok" {
			t.Errorf("health: %d %q", r.status, r.body)
		}
		if r := c.get(t, "/_hivepaas/anything"); r.status != 404 || r.json(t)["error"] != "not found" {
			t.Errorf("/_hivepaas/anything: %d %q", r.status, r.body)
		}
	})

	t.Run("docker sees it healthy", func(t *testing.T) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			status := strings.TrimSpace(docker(t, "inspect", "--format", "{{.State.Health.Status}}", c.id))
			if status == "healthy" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("health is %q", status)
			}
			time.Sleep(250 * time.Millisecond)
		}
	})

	t.Run("calls over the concurrency are turned away", func(t *testing.T) {
		one := startServe(t, imgs.fixture, "HP_FN_MAX_CONCURRENCY=1", "HP_FN_TIMEOUT_MS=5000")
		slow := one.background("/slow?ms=1500")
		time.Sleep(500 * time.Millisecond)

		r := one.get(t, "/text")

		if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "1" ||
			r.json(t)["error"] != "too many requests" {
			t.Errorf("%d, Retry-After %q, %q", r.status, r.header.Get("Retry-After"), r.body)
		}
		if r := <-slow; r.err != nil || r.status != 200 {
			t.Errorf("the running call: %d %v", r.status, r.err)
		}
	})

	t.Run("stopping lets the running call finish", func(t *testing.T) {
		stopped := startServe(t, imgs.fixture, "HP_FN_TIMEOUT_MS=5000")
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

	if !rt.compiled {
		t.Run("a handler that cannot be loaded", func(t *testing.T) {
			broken := startServe(t, imgs.fixture, "HP_FN_HANDLER=missing")

			if r := broken.get(t, "/_hivepaas/health"); r.status != 503 || r.json(t)["status"] != "unavailable" {
				t.Errorf("health: %d %q", r.status, r.body)
			}
			r := broken.call(t, http.MethodGet, "/text", nil, map[string]string{"X-Request-Id": "conf-broken"})
			if r.status != 500 || r.json(t)["error"] != "internal error" {
				t.Errorf("a call: %d %q", r.status, r.body)
			}
			if line := broken.jsonLine(t, "invocation", "conf-broken"); line["outcome"] != "not_loaded" {
				t.Errorf("invocation line %v", line)
			}
			broken.waitForLog(t, "stderr", func(line string) bool {
				return strings.Contains(line, "hivepaas: the handler could not be loaded")
			})
		})
	}
}

// sameBody compares bodies, as JSON when both are.
func sameBody(got, want []byte) bool {
	var g, w any
	if json.Unmarshal(got, &g) == nil && json.Unmarshal(want, &w) == nil {
		return reflect.DeepEqual(g, w)
	}
	return bytes.Equal(got, want)
}
