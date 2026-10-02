//go:build conformance

package conformance

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runCall runs hivepaas-runtime call in the serving container c, with input.
func (c *container) runCall(t *testing.T, input string) result {
	t.Helper()
	return dockerRun(t, input, "exec", "-i", c.id, "hivepaas-runtime", "call")
}

// checkCall checks call: a request handed to the serve running in the
// container, answered as invoke answers it.
func checkCall(t *testing.T, imgs images) {
	c := imgs.serve(t, "HP_FN_TIMEOUT_MS=1000")

	t.Run("a request is answered as invoke answers it", func(t *testing.T) {
		body := base64.StdEncoding.EncodeToString([]byte("payload"))
		r := c.runCall(t, `{"method":"POST","path":"/echo","query":{"a":["1","2"]},`+
			`"headers":{"x-custom":["v"],"x-request-id":["conf-call"]},"body":"`+body+`"}`)

		res := parseResult(t, r)
		want := map[string]any{"method": "POST", "path": "/echo", "query": "1", "queryAll": []any{"1", "2"},
			"header": "v", "body": "payload", "requestId": "conf-call"}
		var got map[string]any
		if err := json.Unmarshal(res.body(t), &got); err != nil || !jsonEqual(got, want) {
			t.Errorf("body %s, want %v", res.body(t), want)
		}
		if res.Status != 200 || res.RequestID != "conf-call" || res.Outcome != "ok" || res.DurationMs == nil {
			t.Errorf("result %+v", res)
		}
		if res.Headers["content-type"] == nil || !slices.Equal(res.Headers["x-request-id"], []string{"conf-call"}) {
			t.Errorf("headers %v", res.Headers)
		}
		for name := range res.Headers {
			if name == "content-length" || name == "date" || name == "connection" || strings.HasPrefix(name, "x-hivepaas-") {
				t.Errorf("the result has the header %s", name)
			}
		}
		if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 1 {
			t.Errorf("call wrote more than its result:\n%s", r.stdout)
		}
		if line := c.jsonLine(t, "invocation", "conf-call"); line["path"] != "/echo" || line["outcome"] != "ok" {
			t.Errorf("serve's invocation line %v", line)
		}
	})

	t.Run("a failed call and a call past its timeout are results", func(t *testing.T) {
		for input, want := range map[string]struct {
			status  int
			outcome string
		}{
			`{"path":"/error"}`:                        {500, "error"},
			`{"path":"/slow","query":{"ms":["3000"]}}`: {504, "timeout"},
		} {
			if res := parseResult(t, c.runCall(t, input)); res.Status != want.status || res.Outcome != want.outcome {
				t.Errorf("%s: result %+v", input, res)
			}
		}
	})

	t.Run("a request that cannot be read, and no server", func(t *testing.T) {
		if r := c.runCall(t, "not json"); r.code != 2 || strings.Contains(r.stdout, resultMarker) {
			t.Errorf("not JSON: exit %d\n%s", r.code, r.stdout)
		}
		r := dockerRun(t, `{}`, "run", "--rm", "-i", "--network", "none", imgs.fixture, "hivepaas-runtime", "call")
		if r.code != 3 || strings.Contains(r.stdout, resultMarker) {
			t.Errorf("no server: exit %d\n%s\n%s", r.code, r.stdout, lastLines(r.stderr))
		}
	})

	t.Run("a call over the concurrency waits for a place", func(t *testing.T) {
		one := imgs.serve(t, "HP_FN_MAX_CONCURRENCY=1", "HP_FN_TIMEOUT_MS=5000")
		slow := one.background("/slow?ms=1500")
		time.Sleep(300 * time.Millisecond)

		start := time.Now()
		res := parseResult(t, one.runCall(t, `{"path":"/text"}`))

		if res.Status != 200 || string(res.body(t)) != "hello" {
			t.Errorf("result %+v", res)
		}
		if took := time.Since(start); took < 700*time.Millisecond {
			t.Errorf("answered after %v, while the slow call ran", took)
		}
		if r := <-slow; r.err != nil || r.status != 200 {
			t.Errorf("the running call: %d %v", r.status, r.err)
		}
	})

	t.Run("serve says the outcome to a call, and keeps the header from the handler", func(t *testing.T) {
		r := c.call(t, http.MethodGet, "/headers", nil, map[string]string{"X-Hivepaas-Call": "1", "X-Custom": "v"})

		if r.header.Get("X-Hivepaas-Outcome") != "ok" {
			t.Errorf("X-Hivepaas-Outcome %q", r.header.Get("X-Hivepaas-Outcome"))
		}
		if _, err := strconv.ParseFloat(r.header.Get("X-Hivepaas-Duration-Ms"), 64); err != nil {
			t.Errorf("X-Hivepaas-Duration-Ms %q", r.header.Get("X-Hivepaas-Duration-Ms"))
		}
		names, _ := r.json(t)["names"].([]any)
		if !slices.Contains(names, any("x-custom")) || slices.Contains(names, any("x-hivepaas-call")) {
			t.Errorf("the handler saw %v", names)
		}
		if plain := c.get(t, "/text"); plain.header.Get("X-Hivepaas-Outcome") != "" {
			t.Errorf("a request that is no call got X-Hivepaas-Outcome")
		}
	})
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
