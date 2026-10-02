package serve

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
)

const usage = "usage: hivepaas-runtime serve|invoke|call|health"

// Exit codes: a result, a failure, a command or input that cannot be used.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	// exitUnavailable: call found no serve answering.
	exitUnavailable = 3
)

const healthTimeout = 3 * time.Second

// Main runs h as the command line asks, and exits.
func Main(h hivepaas.HandlerFunc) {
	os.Exit(run(h, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

func run(h hivepaas.HandlerFunc, args []string, getenv func(string) string,
	stdin io.Reader, stdout, stderr io.Writer,
) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	if command != "serve" && command != "invoke" && command != "call" && command != "health" {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	cfg, err := LoadConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitUsage
	}
	switch command {
	case "invoke":
		return New(h, cfg, stdout, stderr).Invoke(stdin)
	case "call": // the handler runs in serve, not here
		return Call(cfg, stdin, stdout, stderr)
	case "health":
		return Health(cfg, stderr)
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := New(h, cfg, stdout, stderr).Serve(ctx, listener); err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// Health asks the server on this container for its health: 0 when it is
// healthy, 1 otherwise, with the reason on errOut.
func Health(cfg Config, errOut io.Writer) int {
	client := &http.Client{Timeout: healthTimeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/_hivepaas/health", cfg.Port))
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "hivepaas: %v\n", err)
		return exitFailed
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(errOut, "hivepaas: health is %s\n", resp.Status)
		return exitFailed
	}
	return exitOK
}
