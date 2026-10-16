// Package serve runs a Go function's handler as the contract says: an HTTP
// server, or one call read on standard input. The runtime's build compiles a
// main that calls Main; see CONTRACT.md in this repository.
package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

const (
	outcomeOK               = "ok"
	outcomeError            = "error"
	outcomeTimeout          = "timeout"
	outcomeThrottled        = "throttled"
	outcomeRequestTooLarge  = "request_too_large"
	outcomeResponseTooLarge = "response_too_large"
	outcomeNotLoaded        = "not_loaded"
)

const (
	// idleTimeout is longer than a proxy in front keeps an idle connection
	// (Traefik: 90s), so that the proxy, not the function, closes it.
	idleTimeout       = 5 * time.Minute
	readHeaderTimeout = 10 * time.Second
	stopPollInterval  = 10 * time.Millisecond
	requestIDBytes    = 16
)

var (
	errHandlerPanicked  = errors.New("the handler panicked")
	errStatusOutOfRange = errors.New("the response's status is not from 200 to 599")
)

// Errors a handler of a runtime that runs the function in other processes
// returns, for what those processes found.
var (
	// ErrNotLoaded: the handler could not be loaded. The call is answered 500,
	// outcome not_loaded, and logged as no failure of its own: the reason was
	// written when the load failed.
	ErrNotLoaded = errors.New("the handler could not be loaded")
	// ErrResponseTooLarge: the response's body is larger than
	// HP_FN_MAX_BODY_SIZE. The call is answered 500, outcome
	// response_too_large.
	ErrResponseTooLarge = errors.New("the response's body is larger than HP_FN_MAX_BODY_SIZE")
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Runtime runs one handler under the configured limits.
type Runtime struct {
	handler hivepaas.HandlerFunc
	cfg     Config
	out     *lineWriter
	errOut  *lineWriter
	slots   chan struct{}
	// loaded says whether the handler is loaded; nil for one compiled in.
	loaded func() bool
}

// New is a runtime for h. Its log goes to out, its errors to errOut.
func New(h hivepaas.HandlerFunc, cfg Config, out, errOut io.Writer) *Runtime {
	return &Runtime{
		handler: h,
		cfg:     cfg,
		out:     &lineWriter{w: out},
		errOut:  &lineWriter{w: errOut},
		slots:   make(chan struct{}, cfg.MaxConcurrency),
	}
}

// SetLoaded tells the runtime whether its handler is loaded, for one that is
// loaded as the runtime starts rather than compiled in: while loaded answers
// false, the health check answers 503. It is called before Serve.
func (rt *Runtime) SetLoaded(loaded func() bool) {
	rt.loaded = loaded
}

// Output writes line to the runtime's standard output, whole, as one line:
// what the function printed in another process, passed through. Nothing is
// written after invoke's result.
func (rt *Runtime) Output(line string) {
	rt.out.write(line)
}

// ErrorOutput writes line to the runtime's standard error, whole.
func (rt *Runtime) ErrorOutput(line string) {
	rt.errOut.write(line)
}

// Serve answers on l until ctx ends, then waits for the running calls, at
// most for one timeout, and returns.
func (rt *Runtime) Serve(ctx context.Context, l net.Listener) error {
	fresh := &freshConns{conns: map[net.Conn]struct{}{}}
	server := &http.Server{
		Handler:           rt,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ConnState:         fresh.track,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(l) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rt.cfg.Timeout)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(shutdownCtx) }()
	// A connection that has sent no request is not a call: it is closed, not
	// waited for, as Shutdown would for up to 5 seconds.
	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-stopped:
			if err != nil {
				return fmt.Errorf("stopping: %w", err)
			}
			if err := <-served; !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ticker.C:
			fresh.closeAll()
		}
	}
}

// freshConns are the server's connections that have not sent a request yet.
type freshConns struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (f *freshConns) track(c net.Conn, state http.ConnState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == http.StateNew {
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
}

func (f *freshConns) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		_ = c.Close()
		delete(f.conns, c)
	}
}

// ServeHTTP answers one request.
func (rt *Runtime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID(r.Header.Get("X-Request-Id"))
	isCall := r.Header.Get(callHeader) == "1"
	path := r.URL.EscapedPath()
	if strings.HasPrefix(path, "/_hivepaas/") {
		rt.serveOwnPath(w, path, id)
		return
	}
	method := strings.ToUpper(r.Method)
	logged := func(a answer) {
		rt.logInvocation(id, method, path, a)
		if isCall {
			w.Header().Set("X-Hivepaas-Outcome", a.outcome)
			w.Header().Set("X-Hivepaas-Duration-Ms", strconv.FormatFloat(milliseconds(a.duration), 'f', -1, 64))
		}
		writeAnswer(w, id, a)
	}

	if r.ContentLength > rt.cfg.MaxBodySize {
		logged(failure(http.StatusRequestEntityTooLarge, "request too large", id, outcomeRequestTooLarge))
		return
	}
	body, err := rt.readBody(w, r)
	if err != nil {
		dropConnection(w) // the client went away, or sends too slowly: not a call
		return
	}
	if int64(len(body)) > rt.cfg.MaxBodySize {
		logged(failure(http.StatusRequestEntityTooLarge, "request too large", id, outcomeRequestTooLarge))
		return
	}

	select {
	case rt.slots <- struct{}{}:
	default:
		a := failure(http.StatusTooManyRequests, "too many requests", id, outcomeThrottled)
		a.headers.Set("Retry-After", "1")
		logged(a)
		return
	}
	headers := r.Header.Clone()
	headers.Del(callHeader)
	headers.Set("Host", r.Host) // net/http takes it out of the headers
	req := &hivepaas.Request{Method: method, Path: path, Query: r.URL.Query(), Headers: headers, Body: body}
	// A call outlives its client: only its timeout ends it.
	logged(rt.call(context.WithoutCancel(r.Context()), id, req, func() { <-rt.slots }))
}

// dropConnection closes the request's connection without an answer. A panic
// with http.ErrAbortHandler alone would not do: the server would first read
// what is left of the body, with no deadline.
func dropConnection(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

// readBody reads at most one byte over the limit, within one timeout.
func (rt *Runtime) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(rt.cfg.Timeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	body, err := io.ReadAll(io.LimitReader(r.Body, rt.cfg.MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the request's body: %w", err)
	}
	return body, nil
}

func (rt *Runtime) serveOwnPath(w http.ResponseWriter, path, id string) {
	if path == "/_hivepaas/health" {
		if rt.loaded != nil && !rt.loaded() {
			writeAnswer(w, id, jsonAnswer(http.StatusServiceUnavailable, map[string]string{"status": "unavailable"}))
			return
		}
		writeAnswer(w, id, jsonAnswer(http.StatusOK, map[string]string{"status": "ok"}))
		return
	}
	writeAnswer(w, id, jsonAnswer(http.StatusNotFound, map[string]string{"error": "not found"}))
}

// answer is what a call answers, and how it went.
type answer struct {
	status   int
	headers  http.Header
	body     []byte
	outcome  string
	duration time.Duration
}

type handled struct {
	resp *hivepaas.Response
	err  error
}

// call runs the handler on req within the timeout. release runs when the
// handler returns, which may be after the call was answered.
func (rt *Runtime) call(parent context.Context, id string, req *hivepaas.Request, release func()) answer {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, rt.cfg.Timeout)
	defer cancel()
	ctx = hivepaas.WithInvocation(ctx, id, func(msg string) {
		rt.out.writeJSON(map[string]string{"hp": "log", "requestId": id, "msg": msg})
	})

	done := make(chan handled, 1)
	go func() {
		defer release()
		done <- rt.runHandler(ctx, req)
	}()
	var h handled
	select {
	case h = <-done:
	case <-ctx.Done():
		select {
		case h = <-done: // it returned as the timeout came
		default:
			a := failure(http.StatusGatewayTimeout, "timeout", id, outcomeTimeout)
			a.duration = time.Since(start)
			return a
		}
	}
	a := rt.respond(id, h)
	a.duration = time.Since(start)
	return a
}

func (rt *Runtime) runHandler(ctx context.Context, req *hivepaas.Request) (h handled) {
	defer func() {
		if p := recover(); p != nil {
			h = handled{err: fmt.Errorf("%w: %v\n%s", errHandlerPanicked, p, debug.Stack())}
		}
	}()
	resp, err := rt.handler(ctx, req)
	return handled{resp: resp, err: err}
}

// respond turns what the handler returned into the answer.
func (rt *Runtime) respond(id string, h handled) answer {
	switch {
	case errors.Is(h.err, ErrNotLoaded):
		return failure(http.StatusInternalServerError, "internal error", id, outcomeNotLoaded)
	case errors.Is(h.err, ErrResponseTooLarge):
		rt.callFailed(id, h.err)
		return failure(http.StatusInternalServerError, "internal error", id, outcomeResponseTooLarge)
	case h.err != nil:
		rt.callFailed(id, h.err)
		return failure(http.StatusInternalServerError, "internal error", id, outcomeError)
	}
	if h.resp == nil {
		return answer{status: http.StatusNoContent, headers: http.Header{}, outcome: outcomeOK}
	}
	status := h.resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		rt.callFailed(id, fmt.Errorf("%w: %d", errStatusOutOfRange, status))
		return failure(http.StatusInternalServerError, "internal error", id, outcomeError)
	}
	if size := int64(len(h.resp.Body)); size > rt.cfg.MaxBodySize {
		rt.callFailed(id, fmt.Errorf("%w: %d bytes, over %d", ErrResponseTooLarge, size, rt.cfg.MaxBodySize))
		return failure(http.StatusInternalServerError, "internal error", id, outcomeResponseTooLarge)
	}
	headers := http.Header{}
	for name, values := range h.resp.Headers {
		key := http.CanonicalHeaderKey(name)
		headers[key] = append(headers[key], values...)
	}
	body := h.resp.Body
	if !bodyAllowed(status) {
		body = nil
	}
	if len(body) > 0 && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/octet-stream")
	}
	return answer{status: status, headers: headers, body: body, outcome: outcomeOK}
}

func (rt *Runtime) callFailed(id string, err error) {
	rt.errOut.write(fmt.Sprintf("hivepaas: call %s failed: %v", id, err))
}

func (rt *Runtime) logInvocation(id, method, path string, a answer) {
	rt.out.writeJSON(struct {
		HP         string  `json:"hp"`
		RequestID  string  `json:"requestId"`
		Method     string  `json:"method"`
		Path       string  `json:"path"`
		Status     int     `json:"status"`
		DurationMs float64 `json:"durationMs"`
		Outcome    string  `json:"outcome"`
	}{"invocation", id, method, path, a.status, milliseconds(a.duration), a.outcome})
}

// failure is an answer the runtime makes itself.
func failure(status int, message, id, outcome string) answer {
	a := jsonAnswer(status, map[string]string{"error": message, "requestId": id})
	a.outcome = outcome
	return a
}

func jsonAnswer(status int, v map[string]string) answer {
	body, _ := json.Marshal(v) // a map of strings always encodes
	return answer{status: status, headers: http.Header{"Content-Type": {"application/json"}}, body: body}
}

func writeAnswer(w http.ResponseWriter, id string, a answer) {
	header := w.Header()
	for name, values := range a.headers {
		header[name] = values
	}
	header.Del("Transfer-Encoding")
	header.Set("X-Request-Id", id)
	if bodyAllowed(a.status) {
		header.Set("Content-Length", strconv.Itoa(len(a.body)))
	} else {
		header.Del("Content-Length")
	}
	w.WriteHeader(a.status)
	if len(a.body) > 0 {
		_, _ = w.Write(a.body)
	}
}

func bodyAllowed(status int) bool {
	return status != http.StatusNoContent && status != http.StatusNotModified
}

// requestID is the request's own id when the contract accepts it, else a new one.
func requestID(sent string) string {
	if requestIDPattern.MatchString(sent) {
		return sent
	}
	b := make([]byte, requestIDBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// milliseconds is d in milliseconds, to the microsecond.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / float64(time.Millisecond/time.Microsecond)
}

// lineWriter writes whole lines, one at a time, until it is closed.
type lineWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (lw *lineWriter) write(line string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if !lw.closed {
		_, _ = io.WriteString(lw.w, line+"\n")
	}
}

func (lw *lineWriter) writeJSON(v any) {
	line, err := json.Marshal(v)
	if err == nil {
		lw.write(string(line))
	}
}

// writeLast writes line, and nothing after it.
func (lw *lineWriter) writeLast(line string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if !lw.closed {
		_, _ = io.WriteString(lw.w, line+"\n")
		lw.closed = true
	}
}
