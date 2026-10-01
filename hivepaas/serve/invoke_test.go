package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

type invokeResult struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	RequestID  string              `json:"requestId"`
	DurationMs float64             `json:"durationMs"`
	Outcome    string              `json:"outcome"`
}

// result is the line after the marker, which must be the last line.
func result(t *testing.T, stdout string) invokeResult {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	payload, ok := strings.CutPrefix(lines[len(lines)-1], "#hivepaas-result ")
	if !ok {
		t.Fatalf("the last line is not the result: %q", stdout)
	}
	var r invokeResult
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		t.Fatalf("the result is not JSON: %v", err)
	}
	return r
}

func invoke(h hivepaas.HandlerFunc, cfg Config, input string) (code int, stdout, stderr string) {
	out, errOut := &syncBuffer{}, &syncBuffer{}
	code = New(h, cfg, out, errOut).Invoke(strings.NewReader(input))
	return code, out.String(), errOut.String()
}

func TestInvokeCallsTheHandlerOnceWithTheRequestGiven(t *testing.T) {
	var got *hivepaas.Request
	input := `{"method":"post","path":"/echo","query":{"a":["1","2"]},` +
		`"headers":{"x-custom":["v"],"x-request-id":["inv-1"]},` +
		`"body":"` + base64.StdEncoding.EncodeToString([]byte("payload")) + `"}`

	code, stdout, _ := invoke(func(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
		got = req
		hivepaas.Log(ctx, "fixture log line")
		return hivepaas.JSON(200, map[string]string{"hello": "Ada"})
	}, testConfig, input)

	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got.Method != "POST" || got.Path != "/echo" || got.Text() != "payload" ||
		len(got.Query["a"]) != 2 || got.Headers.Get("X-Custom") != "v" {
		t.Fatalf("request = %+v", got)
	}
	r := result(t, stdout)
	body, _ := base64.StdEncoding.DecodeString(r.Body)
	if r.Status != 200 || string(body) != `{"hello":"Ada"}` || r.Outcome != "ok" || r.RequestID != "inv-1" {
		t.Fatalf("result = %+v, body %q", r, body)
	}
	if ct := r.Headers["content-type"]; len(ct) != 1 || ct[0] != "application/json" {
		t.Fatalf("headers = %v", r.Headers)
	}
	if !strings.Contains(stdout, `"msg":"fixture log line"`) || !strings.Contains(stdout, `"hp":"invocation"`) {
		t.Fatalf("the call's log is not in the output: %q", stdout)
	}
}

func TestAnEmptyRequestIsAGetOfTheRoot(t *testing.T) {
	var got *hivepaas.Request
	code, stdout, _ := invoke(func(_ context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
		got = req
		return nil, nil
	}, testConfig, `{}`)

	if code != 0 || got.Method != "GET" || got.Path != "/" || len(got.Body) != 0 {
		t.Fatalf("exit %d, request %+v", code, got)
	}
	if r := result(t, stdout); r.Status != 204 || r.Body != "" {
		t.Fatalf("result = %+v", r)
	}
}

// A failed call is still a result: the exit code says only whether there is one.
func TestAFailedCallIsAResultNotAFailedInvoke(t *testing.T) {
	code, stdout, stderr := invoke(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return nil, errors.New("fixture failure")
	}, testConfig, `{}`)

	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if r := result(t, stdout); r.Status != 500 || r.Outcome != "error" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(stderr, "fixture failure") {
		t.Fatalf("the error is not in the log: %q", stderr)
	}
}

// The result is the last line even when the handler, past its timeout, still
// logs.
func TestInvokeTimesOutLikeServe(t *testing.T) {
	cfg := testConfig
	cfg.Timeout = 30 * time.Millisecond
	logged := make(chan struct{})
	out, errOut := &syncBuffer{}, &syncBuffer{}
	rt := New(func(ctx context.Context, _ *hivepaas.Request) (*hivepaas.Response, error) {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		hivepaas.Log(ctx, "too late")
		close(logged)
		return nil, nil
	}, cfg, out, errOut)

	code := rt.Invoke(strings.NewReader(`{}`))
	<-logged

	if r := result(t, out.String()); code != 0 || r.Status != 504 || r.Outcome != "timeout" {
		t.Fatalf("exit %d, result %+v", code, r)
	}
}

// A request that cannot be read is the only failure of invoke itself.
func TestARequestThatCannotBeReadExits2(t *testing.T) {
	for name, input := range map[string]string{
		"not JSON":          `not json`,
		"a body not base64": `{"body":"%%%"}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := invoke(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
				t.Error("the handler was called")
				return nil, nil
			}, testConfig, input)

			if code != 2 || strings.Contains(stdout, "#hivepaas-result") || stderr == "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestInvokeRefusesABodyOverTheLimit(t *testing.T) {
	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", int(testConfig.MaxBodySize)+1)))

	code, stdout, _ := invoke(func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		t.Error("the handler was called")
		return nil, nil
	}, testConfig, `{"body":"`+big+`"}`)

	if r := result(t, stdout); code != 0 || r.Status != 413 || r.Outcome != "request_too_large" {
		t.Fatalf("exit %d, result %+v", code, r)
	}
}

// health asks the server on this container: 0 when it answers 200.
func TestHealthAsksTheServerOnThePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, _ := newTestRuntime(noContent, testConfig)
	server := &http.Server{Handler: rt, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	cfg := testConfig
	cfg.Port, _ = strconv.Atoi(port)
	errOut := &syncBuffer{}

	if code := Health(cfg, errOut); code != 0 {
		t.Fatalf("health of a running server = %d: %s", code, errOut)
	}
	_ = server.Close()
	if code := Health(cfg, errOut); code != 1 {
		t.Fatalf("health of a stopped server = %d", code)
	}
}

func TestAnUnknownCommandIsAUsageError(t *testing.T) {
	stderr := &syncBuffer{}

	code := run(noContent, []string{"bogus"}, env(nil), strings.NewReader(""), &syncBuffer{}, stderr)

	if code != 2 || !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestALimitThatIsNotAPositiveNumberStopsTheRuntime(t *testing.T) {
	for _, command := range []string{"serve", "invoke"} {
		stderr := &syncBuffer{}

		code := run(noContent, []string{command}, env(map[string]string{"HP_FN_TIMEOUT_MS": "0"}),
			strings.NewReader("{}"), &syncBuffer{}, stderr)

		if code != 2 || !strings.Contains(stderr.String(), "HP_FN_TIMEOUT_MS") {
			t.Errorf("%s: exit %d, stderr %q", command, code, stderr)
		}
	}
}

func TestInvokeIsACommand(t *testing.T) {
	stdout := &syncBuffer{}

	code := run(noContent, []string{"invoke"}, env(nil), strings.NewReader("{}"), stdout, &syncBuffer{})

	if r := result(t, stdout.String()); code != 0 || r.Status != 204 {
		t.Fatalf("exit %d, result %+v", code, r)
	}
}
