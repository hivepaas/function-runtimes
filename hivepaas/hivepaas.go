// Package hivepaas is what a Go function is written against: the request it is
// given, the response it returns, and its call's id and logger.
//
// A function is a package whose handler has the type HandlerFunc:
//
//	func Handle(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
//		return hivepaas.JSON(200, map[string]string{"hello": req.Query.Get("name")})
//	}
//
// The runtime compiles it into a server; see CONTRACT.md in this repository.
package hivepaas

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
)

// Request is one call.
type Request struct {
	// Method is the HTTP method, in upper case.
	Method string
	// Path is the request's path, without its query.
	Path string
	// Query is the query string: Query.Get("name") is the first value.
	Query url.Values
	// Headers are the request's headers. Get looks them up in any case.
	Headers http.Header
	// Body is the request's body, whole.
	Body []byte
}

// Text is the body as text.
func (r *Request) Text() string {
	return string(r.Body)
}

// JSON decodes the body into v.
func (r *Request) JSON(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("the request body is not JSON: %w", err)
	}
	return nil
}

// Response is what a call answers. A zero Status is 200; a nil response is
// 204, no content.
type Response struct {
	Status  int
	Headers http.Header
	// Body is sent as it is. Without a Content-Type header it is sent as
	// application/octet-stream.
	Body []byte
}

// JSON is a response whose body is v as JSON.
func JSON(status int, v any) (*Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the response cannot be JSON: %w", err)
	}
	return &Response{
		Status:  status,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    body,
	}, nil
}

// Text is a response whose body is text.
func Text(status int, text string) *Response {
	return &Response{
		Status:  status,
		Headers: http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:    []byte(text),
	}
}

// HandlerFunc is a function's handler. The context's deadline is the call's
// timeout. An error answers 500 and is logged; its text is not sent.
type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

type invocationKey struct{}

type invocation struct {
	requestID string
	log       func(msg string)
}

// WithInvocation gives a call's context its id and its logger. The runtime
// calls it; a handler has no need to.
func WithInvocation(ctx context.Context, requestID string, logFunc func(msg string)) context.Context {
	return context.WithValue(ctx, invocationKey{}, &invocation{requestID: requestID, log: logFunc})
}

// RequestID is the id of the call ctx belongs to, empty outside a call.
func RequestID(ctx context.Context) string {
	if inv, ok := ctx.Value(invocationKey{}).(*invocation); ok {
		return inv.requestID
	}
	return ""
}

// Log writes a line to the call's log, which carries the call's id. Outside a
// call it goes to the standard logger.
func Log(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if inv, ok := ctx.Value(invocationKey{}).(*invocation); ok && inv.log != nil {
		inv.log(msg)
		return
	}
	log.Print(msg)
}
