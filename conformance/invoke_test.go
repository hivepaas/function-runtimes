//go:build conformance

package conformance

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const resultMarker = "#hivepaas-result "

// invokeResult is the line after the marker.
type invokeResult struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	RequestID  string              `json:"requestId"`
	DurationMs *float64            `json:"durationMs"`
	Outcome    string              `json:"outcome"`
}

func (r invokeResult) body(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(r.Body)
	if err != nil {
		t.Fatalf("the result's body is not base64: %v", err)
	}
	return b
}

// invoke runs the image's invoke with input on its standard input, without a
// network, as a test run does.
func invoke(t *testing.T, image, input string, env ...string) result {
	t.Helper()
	args := []string{"run", "--rm", "-i", "--network", "none"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	return dockerRun(t, input, append(args, image, "hivepaas-runtime", "invoke")...)
}

// parseResult reads the result from the last line of the output, which must
// be the result's.
func parseResult(t *testing.T, r result) invokeResult {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	payload, ok := strings.CutPrefix(lines[len(lines)-1], resultMarker)
	if !ok {
		t.Fatalf("the last line is not the result:\n%s\n%s", r.stdout, r.stderr)
	}
	var res invokeResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("the result is not JSON: %v", err)
	}
	if res.DurationMs == nil {
		t.Errorf("the result has no duration: %s", payload)
	}
	return res
}

func checkInvoke(t *testing.T, rt runtimeDef, imgs images) {
	t.Run("the request reaches the handler, the result is the last line", func(t *testing.T) {
		input := `{"method":"post","path":"/echo","query":{"a":["1","2"]},` +
			`"headers":{"x-custom":["v"],"x-request-id":["inv-echo"]},` +
			`"body":"` + base64.StdEncoding.EncodeToString([]byte("payload")) + `"}`

		r := invoke(t, imgs.fixture, input)

		res := parseResult(t, r)
		if res.Status != 200 || res.Outcome != "ok" || res.RequestID != "inv-echo" {
			t.Errorf("result %+v", res)
		}
		var got map[string]any
		if err := json.Unmarshal(res.body(t), &got); err != nil {
			t.Fatalf("the body is not JSON: %v", err)
		}
		want := map[string]any{
			"method": "POST", "path": "/echo", "query": "1", "queryAll": []any{"1", "2"},
			"header": "v", "body": "payload", "requestId": "inv-echo",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("body %v, want %v", got, want)
		}
		if !reflect.DeepEqual(res.Headers["content-type"], []string{"application/json"}) ||
			!reflect.DeepEqual(res.Headers["x-request-id"], []string{"inv-echo"}) {
			t.Errorf("headers %v", res.Headers)
		}
		if !strings.Contains(r.stdout, `"hp":"invocation"`) {
			t.Errorf("no invocation line before the result:\n%s", r.stdout)
		}
	})

	t.Run("an empty request is a GET of the root", func(t *testing.T) {
		res := parseResult(t, invoke(t, imgs.fixture, `{}`))
		if res.Status != 204 || res.Body != "" {
			t.Errorf("result %+v", res)
		}
	})

	t.Run("a failed call is a result", func(t *testing.T) {
		r := invoke(t, imgs.fixture, `{"path":"/error"}`)

		if res := parseResult(t, r); res.Status != 500 || res.Outcome != "error" {
			t.Errorf("result %+v", res)
		}
		if !strings.Contains(r.stderr, "fixture failure") {
			t.Errorf("the error is not on standard error:\n%s", r.stderr)
		}
	})

	t.Run("a call is answered at its timeout", func(t *testing.T) {
		r := invoke(t, imgs.fixture, `{"path":"/slow","query":{"ms":["8000"]}}`, "HP_FN_TIMEOUT_MS=1000")

		if res := parseResult(t, r); res.Status != 504 || res.Outcome != "timeout" {
			t.Errorf("result %+v", res)
		}
		if r.took > 6*time.Second {
			t.Errorf("invoke took %v", r.took)
		}
	})

	t.Run("a handler's log line comes before the result", func(t *testing.T) {
		r := invoke(t, imgs.fixture, `{"path":"/log","headers":{"x-request-id":["inv-log"]}}`)

		parseResult(t, r)
		if !strings.Contains(r.stdout, `"msg":"fixture log line"`) || !strings.Contains(r.stdout, `"requestId":"inv-log"`) {
			t.Errorf("no log line:\n%s", r.stdout)
		}
	})

	t.Run("a body over the limit", func(t *testing.T) {
		body := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 17)))
		r := invoke(t, imgs.fixture, `{"path":"/echo","body":"`+body+`"}`, "HP_FN_MAX_BODY_SIZE=16")

		if res := parseResult(t, r); res.Status != 413 || res.Outcome != "request_too_large" {
			t.Errorf("result %+v", res)
		}
	})

	t.Run("a request that cannot be read", func(t *testing.T) {
		r := invoke(t, imgs.fixture, `not json`)

		if r.code != 2 || strings.Contains(r.stdout, resultMarker) {
			t.Errorf("exit %d\n%s", r.code, r.stdout)
		}
	})

	// The image keeps the lock file its build made; deps installs from it
	// again, as a test run's libraries image does, without a network.
	t.Run("deps again, from the lock file the build made", func(t *testing.T) {
		image := imgs.fixture
		if rt.compiled {
			image = imgs.source
		}

		r := dockerRun(t, `{"path":"/lib"}`, "run", "--rm", "-i", "--network", "none", image,
			"sh", "-c", "hivepaas-runtime deps >&2 && hivepaas-runtime invoke")

		if res := parseResult(t, r); res.Status != 200 || string(res.body(t)) != "from lib" {
			t.Errorf("result %+v\n%s", res, r.stderr)
		}
	})

	t.Run("a handler that cannot be loaded", func(t *testing.T) {
		image, handler, says := imgs.fixture, "HP_FN_HANDLER=missing", "the handler could not be loaded"
		if rt.compiled {
			image, handler, says = imgs.source, "HP_FN_HANDLER=Missing", "the function does not compile"
		}

		r := invoke(t, image, `{}`, handler)

		if r.code != 3 || strings.Contains(r.stdout, resultMarker) || !strings.Contains(r.stderr, says) {
			t.Errorf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
	})

	if rt.compiled {
		t.Run("a function not built yet is built first", func(t *testing.T) {
			r := invoke(t, imgs.source, `{"path":"/text"}`)

			if res := parseResult(t, r); res.Status != 200 || string(res.body(t)) != "hello" {
				t.Errorf("result %+v", res)
			}
			for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
				if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, resultMarker) {
					t.Errorf("a line on standard output that is neither log nor result: %q", line)
				}
			}
		})
	}
}
