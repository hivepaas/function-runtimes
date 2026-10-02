package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

// resultMarker starts the line that carries an invoke's result.
const resultMarker = "#hivepaas-result "

type invokeRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   map[string][]string `json:"query"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// Invoke calls the handler once with the request read from in, and writes the
// result as the last line of the output. It returns the process's exit code:
// 0 with a result, 2 when the request could not be read.
func (rt *Runtime) Invoke(in io.Reader) int {
	ir, body, err := readInvokeRequest(in)
	if err != nil {
		rt.errOut.write("hivepaas: " + err.Error())
		return exitUsage
	}
	req := &hivepaas.Request{
		Method:  strings.ToUpper(ir.Method),
		Path:    ir.Path,
		Query:   url.Values{},
		Headers: http.Header{},
		Body:    body,
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if req.Path == "" {
		req.Path = "/"
	}
	for name, values := range ir.Query {
		req.Query[name] = values
	}
	for name, values := range ir.Headers {
		key := http.CanonicalHeaderKey(name)
		req.Headers[key] = append(req.Headers[key], values...)
	}
	id := requestID(req.Headers.Get("X-Request-Id"))

	var a answer
	if int64(len(body)) > rt.cfg.MaxBodySize {
		a = failure(http.StatusRequestEntityTooLarge, "request too large", id, outcomeRequestTooLarge)
	} else {
		a = rt.call(context.Background(), id, req, func() {})
	}
	rt.logInvocation(id, req.Method, req.Path, a)
	rt.writeResult(id, a)
	return exitOK
}

// readInvokeRequest reads invoke's request, and its body out of base64.
func readInvokeRequest(in io.Reader) (*invokeRequest, []byte, error) {
	input, err := io.ReadAll(in)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the request: %w", err)
	}
	var ir invokeRequest
	if err := json.Unmarshal(input, &ir); err != nil {
		return nil, nil, fmt.Errorf("the request is not JSON: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(ir.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("the request's body is not base64: %w", err)
	}
	return &ir, body, nil
}

func (rt *Runtime) writeResult(id string, a answer) {
	headers := map[string][]string{}
	for name, values := range a.headers {
		headers[strings.ToLower(name)] = values
	}
	headers["x-request-id"] = []string{id}
	line, _ := json.Marshal(struct { // strings, numbers and a map of strings always encode
		Status     int                 `json:"status"`
		Headers    map[string][]string `json:"headers"`
		Body       string              `json:"body"`
		RequestID  string              `json:"requestId"`
		DurationMs float64             `json:"durationMs"`
		Outcome    string              `json:"outcome"`
	}{a.status, headers, base64.StdEncoding.EncodeToString(a.body), id, milliseconds(a.duration), a.outcome})
	rt.out.writeLast(resultMarker + string(line))
}
