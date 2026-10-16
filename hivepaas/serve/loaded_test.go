package serve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

// A runtime whose handler is loaded as it starts answers its health check 503
// until the handler is.
func TestHealthWaitsForTheHandlerToBeLoaded(t *testing.T) {
	var loaded atomic.Bool
	rt, _, _ := newTestRuntime(noContent, testConfig)
	rt.SetLoaded(loaded.Load)

	rec := call(rt, http.MethodGet, "/_hivepaas/health", "", nil)
	if rec.Code != http.StatusServiceUnavailable || decode(t, rec.Body.Bytes())["status"] != "unavailable" {
		t.Errorf("not loaded: %d %s", rec.Code, rec.Body)
	}
	loaded.Store(true)
	if rec := call(rt, http.MethodGet, "/_hivepaas/health", "", nil); rec.Code != http.StatusOK {
		t.Errorf("loaded: %d %s", rec.Code, rec.Body)
	}
}

// A handler that could not be loaded answers every call 500, outcome
// not_loaded, with no failure of the call's own on standard error.
func TestACallToAHandlerNotLoaded(t *testing.T) {
	notLoaded := func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return nil, fmt.Errorf("%w: no function handler", ErrNotLoaded)
	}
	rt, out, errOut := newTestRuntime(notLoaded, testConfig)

	rec := call(rt, http.MethodGet, "/x", "", nil)

	if rec.Code != http.StatusInternalServerError || decode(t, rec.Body.Bytes())["error"] != "internal error" {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if outcome := out.outcome(t); outcome != "not_loaded" {
		t.Errorf("outcome %v", outcome)
	}
	if strings.Contains(errOut.String(), "failed") {
		t.Errorf("a call failure was written: %s", errOut)
	}
}

// A body found too large in another process is answered as one found here.
func TestAResponseFoundTooLargeElsewhere(t *testing.T) {
	tooLarge := func(context.Context, *hivepaas.Request) (*hivepaas.Response, error) {
		return nil, fmt.Errorf("%w: 9000 bytes", ErrResponseTooLarge)
	}
	rt, out, errOut := newTestRuntime(tooLarge, testConfig)

	if rec := call(rt, http.MethodGet, "/x", "", nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if outcome := out.outcome(t); outcome != "response_too_large" {
		t.Errorf("outcome %v", outcome)
	}
	if !strings.Contains(errOut.String(), "9000 bytes") {
		t.Errorf("standard error: %s", errOut)
	}
}

// What another process printed is written whole, and nothing after invoke's
// result.
func TestOutputPassedThroughStopsAtTheResult(t *testing.T) {
	rt, out, errOut := newTestRuntime(func(ctx context.Context, _ *hivepaas.Request) (*hivepaas.Response, error) {
		return nil, nil
	}, testConfig)
	rt.Output("printed by the function")
	rt.ErrorOutput("a warning")

	if code := rt.Invoke(strings.NewReader(`{}`)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	rt.Output("too late")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if lines[0] != "printed by the function" || !strings.HasPrefix(lines[len(lines)-1], resultMarker) {
		t.Errorf("standard output:\n%s", out)
	}
	if strings.Contains(out.String(), "too late") {
		t.Error("a line was written after the result")
	}
	if strings.TrimSpace(errOut.String()) != "a warning" {
		t.Errorf("standard error: %q", errOut)
	}
}
