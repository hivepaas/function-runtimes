package serve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// callHeader marks a request as call's: serve's answer to it says the call's
// outcome and duration, and the handler does not see it.
const callHeader = "X-Hivepaas-Call"

// callWaitMargin is how much longer than a call's timeout call waits for serve,
// which answers at it.
const callWaitMargin = 10 * time.Second

// notTheCalls are the headers of serve's answer that are the connection's or
// the runtime's.
var notTheCalls = map[string]bool{
	"content-length": true, "transfer-encoding": true, "connection": true, "keep-alive": true, "date": true,
	"x-hivepaas-outcome": true, "x-hivepaas-duration-ms": true,
}

// Call hands the request read from in - invoke's - to the serve running on this
// container, and writes its answer as Invoke writes a result. A call turned
// away (429) is sent again after Retry-After while the timeout allows. It
// returns the exit code: 0 with a result, 2 when the request cannot be read,
// 3 when serve does not answer.
func Call(cfg Config, in io.Reader, out, errOut io.Writer) int {
	ir, body, err := readInvokeRequest(in)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "hivepaas: %v\n", err)
		return exitUsage
	}
	method := strings.ToUpper(ir.Method)
	if method == "" {
		method = http.MethodGet
	}
	target := ir.Path
	if target == "" {
		target = "/"
	}
	if query := url.Values(ir.Query).Encode(); query != "" {
		target += "?" + query
	}
	if err := checkSendable(method, target, ir.Headers); err != nil {
		_, _ = fmt.Fprintf(errOut, "hivepaas: %v\n", err)
		return exitUsage
	}
	client := &http.Client{Timeout: cfg.Timeout + callWaitMargin}
	deadline := time.Now().Add(cfg.Timeout)
	for {
		a, err := sendToServe(client, cfg.Port, method, target, ir.Headers, body)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "hivepaas: serve does not answer: %v\n", err)
			return exitUnavailable
		}
		wait := retryAfter(a.header.Get("Retry-After"))
		if a.status == http.StatusTooManyRequests && time.Now().Add(wait).Before(deadline) {
			time.Sleep(wait)
			continue
		}
		line, _ := json.Marshal(callResult(a)) // strings, numbers and a map of strings always encode
		_, _ = fmt.Fprintln(out, resultMarker+string(line))
		return exitOK
	}
}

// served is serve's answer, read whole.
type served struct {
	status int
	header http.Header
	body   []byte
}

func sendToServe(client *http.Client, port int, method, target string, headers map[string][]string,
	body []byte,
) (served, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, target), bytes.NewReader(body))
	if err != nil {
		return served{}, fmt.Errorf("the request: %w", err)
	}
	for name, values := range headers {
		if !strings.EqualFold(name, callHeader) {
			req.Header[http.CanonicalHeaderKey(name)] = values
		}
	}
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
	}
	req.Header.Set(callHeader, "1")
	resp, err := client.Do(req)
	if err != nil {
		return served{}, fmt.Errorf("sending: %w", err)
	}
	defer resp.Body.Close()
	answered, err := io.ReadAll(resp.Body)
	if err != nil {
		return served{}, fmt.Errorf("reading the answer: %w", err)
	}
	return served{resp.StatusCode, resp.Header, answered}, nil
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds < 0 {
		return time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

// callResult is serve's answer as invoke's result line says one.
func callResult(a served) any {
	headers := map[string][]string{}
	for name, values := range a.header {
		if lower := strings.ToLower(name); !notTheCalls[lower] {
			headers[lower] = values
		}
	}
	duration, _ := strconv.ParseFloat(a.header.Get("X-Hivepaas-Duration-Ms"), 64)
	outcome := a.header.Get("X-Hivepaas-Outcome")
	if outcome == "" {
		outcome = outcomeOK
	}
	requestID := ""
	if ids := headers["x-request-id"]; len(ids) > 0 {
		requestID = ids[0]
	}
	return struct {
		Status     int                 `json:"status"`
		Headers    map[string][]string `json:"headers"`
		Body       string              `json:"body"`
		RequestID  string              `json:"requestId"`
		DurationMs float64             `json:"durationMs"`
		Outcome    string              `json:"outcome"`
	}{a.status, headers, base64.StdEncoding.EncodeToString(a.body), requestID, duration, outcome}
}

// errUnsendable is a part of a request that cannot go on an HTTP/1.1 request
// line or header.
var errUnsendable = errors.New("cannot be sent")

// checkSendable refuses what cannot go on an HTTP/1.1 request line or header.
func checkSendable(method, target string, headers map[string][]string) error {
	if !isToken(method) {
		return fmt.Errorf("the request's method %q %w", method, errUnsendable)
	}
	if !strings.HasPrefix(target, "/") || strings.ContainsFunc(target, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	}) {
		return fmt.Errorf("the request's path %q %w", target, errUnsendable)
	}
	for name, values := range headers {
		if !isToken(name) || slices.ContainsFunc(values, func(v string) bool { return strings.ContainsAny(v, "\r\n\x00") }) {
			return fmt.Errorf("the request's header %q %w", name, errUnsendable)
		}
	}
	return nil
}

func isToken(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return r > 0x7e || r <= ' ' || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r)
	})
}
