// Package fixture is the function the conformance suite calls: each path shows
// one thing the contract promises.
package fixture

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"example.com/fixturelib"
	"github.com/hivepaas/function-runtimes/hivepaas"
)

// Handle answers by path.
func Handle(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
	q := req.Query
	switch req.Path {
	case "/":
		return nil, nil
	case "/json":
		return hivepaas.JSON(200, map[string]string{"hello": q.Get("name")})
	case "/text":
		return hivepaas.Text(200, "hello"), nil
	case "/bytes":
		return &hivepaas.Response{Body: []byte{0, 1, 2, 255}}, nil
	case "/echo":
		return hivepaas.JSON(200, map[string]any{
			"method":    req.Method,
			"path":      req.Path,
			"query":     q.Get("a"),
			"queryAll":  q["a"],
			"header":    req.Headers.Get("X-Custom"),
			"body":      req.Text(),
			"requestId": hivepaas.RequestID(ctx),
		})
	case "/status":
		code, _ := strconv.Atoi(q.Get("code"))
		return &hivepaas.Response{Status: code}, nil
	case "/error":
		return nil, errors.New("fixture failure")
	case "/slow":
		ms, _ := strconv.Atoi(q.Get("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond) // past the timeout on purpose
		return hivepaas.Text(200, "slow done"), nil
	case "/big":
		n, _ := strconv.Atoi(q.Get("bytes"))
		return hivepaas.Text(200, strings.Repeat("x", n)), nil
	case "/invalid":
		return &hivepaas.Response{Status: 42}, nil
	case "/env":
		return hivepaas.Text(200, os.Getenv("GREETING")), nil
	case "/deadline":
		deadline, _ := ctx.Deadline()
		return hivepaas.JSON(200, map[string]int64{"remainingMs": time.Until(deadline).Milliseconds()})
	case "/log":
		hivepaas.Log(ctx, "fixture log line")
		return hivepaas.Text(200, "logged"), nil
	case "/headers":
		names := make([]string, 0, len(req.Headers))
		for name := range req.Headers {
			names = append(names, strings.ToLower(name))
		}
		slices.Sort(names)
		return hivepaas.JSON(200, map[string][]string{"names": names})
	case "/whoami":
		return hivepaas.JSON(200, map[string]int{"uid": os.Getuid(), "gid": os.Getgid()})
	case "/lib":
		return hivepaas.Text(200, fixturelib.Name()), nil
	}
	return hivepaas.JSON(404, map[string]string{"error": "no route"})
}
