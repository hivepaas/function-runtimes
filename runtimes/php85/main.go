// Command hivepaas-runtime is the PHP runtime: the Go runtime's server, which
// hands each call to PHP processes that keep the function's handler loaded,
// one call at a time each. See CONTRACT.md in this repository.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

const usage = "usage: hivepaas-runtime serve|invoke|call|health|deps"

// Exit codes, as the contract says.
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 2
	exitNotLoaded = 3
)

// The contract's defaults for PHP.
const (
	defaultEntrypoint = "index.php"
	defaultHandler    = "handler"
)

// Where the image keeps what the runtime runs.
const (
	appDir       = "/app"
	workerScript = "/hivepaas/php/worker.php"
	phpBinary    = "php"
)

// How long a process may take to load the handler, and how long it is waited
// for once asked to stop.
const (
	loadTimeout = time.Minute
	stopTimeout = 2 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	if command == "deps" {
		return deps(appDir, stdout, stderr)
	}
	if command != "serve" && command != "invoke" && command != "call" && command != "health" {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	cfg, err := serve.LoadConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitUsage
	}
	switch command {
	case "call":
		return serve.Call(cfg, stdin, stdout, stderr)
	case "health":
		return serve.Health(cfg, stderr)
	}

	sz, err := sizeFor(getenv, cfg, cgroupRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitUsage
	}
	env, err := withFunctionINI(workerEnv(environ, getenv, cfg), getenv, appDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	wc := workerConfig{command: []string{phpBinary, workerScript}, dir: appDir, env: env}
	if command == "invoke" {
		return invoke(cfg, wc, stdin, stdout, stderr)
	}
	return serveCalls(cfg, wc, sz, stdout, stderr)
}

// workerEnv is the PHP processes' environment: the function's own, with the
// configuration as the server reads it.
func workerEnv(environ []string, getenv func(string) string, cfg serve.Config) []string {
	entrypoint := getenv("HP_FN_ENTRYPOINT")
	if entrypoint == "" {
		entrypoint = defaultEntrypoint
	}
	handler := getenv("HP_FN_HANDLER")
	if handler == "" {
		handler = defaultHandler
	}
	return append(append([]string{}, environ...),
		"HP_FN_ENTRYPOINT="+entrypoint,
		"HP_FN_HANDLER="+handler,
		"HP_FN_MAX_BODY_SIZE="+strconv.FormatInt(cfg.MaxBodySize, 10),
	)
}

// iniMode is the copy of the function's php.ini: the runtime's user reads it.
const iniMode = 0o600

// withFunctionINI adds the function's own php.ini, when its code has one, to
// the settings PHP reads after the runtime's: PHP reads the files of the
// directories PHP_INI_SCAN_DIR names, the empty one being its own conf.d. A
// PHP_INI_SCAN_DIR the function sets is its own to keep.
func withFunctionINI(env []string, getenv func(string) string, dir string) ([]string, error) {
	ini, err := os.ReadFile(filepath.Join(dir, "php.ini"))
	if errors.Is(err, fs.ErrNotExist) || getenv("PHP_INI_SCAN_DIR") != "" {
		return env, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the function's php.ini: %w", err)
	}
	scan, err := os.MkdirTemp("", "hivepaas-php-ini-")
	if err != nil {
		return nil, fmt.Errorf("a directory for the function's php.ini: %w", err)
	}
	copied := filepath.Join(scan, "function.ini")
	if err := os.WriteFile(copied, ini, iniMode); err != nil { //nolint:gosec // in a directory of its own
		return nil, fmt.Errorf("copying the function's php.ini: %w", err)
	}
	return append(env, "PHP_INI_SCAN_DIR=:"+scan), nil
}

// invoke calls the handler once, in one PHP process.
func invoke(cfg serve.Config, wc workerConfig, stdin io.Reader, stdout, stderr io.Writer) int {
	p := newPool(poolConfig{min: 1, max: 1, worker: wc, loadTimeout: loadTimeout})
	rt := serve.New(p.handle, cfg, stdout, stderr)
	p.out = rt
	defer p.stop(stopTimeout)
	if err := p.start(); err != nil {
		rt.ErrorOutput("hivepaas: the handler could not be loaded: " + err.Error())
		return exitNotLoaded
	}
	return rt.Invoke(stdin)
}

// serveCalls answers HTTP until it is told to stop, then lets the running calls
// finish and stops the PHP processes.
func serveCalls(cfg serve.Config, wc workerConfig, sz size, stdout, stderr io.Writer) int {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	p := newPool(poolConfig{min: sz.min, max: sz.max, worker: wc, loadTimeout: loadTimeout,
		idleTimeout: idleTimeout})
	rt := serve.New(p.handle, cfg, stdout, stderr)
	p.out = rt
	rt.SetLoaded(p.isLoaded)
	rt.ErrorOutput("hivepaas: serving with " + sz.String())
	go func() {
		if err := p.start(); err != nil {
			rt.ErrorOutput("hivepaas: the handler could not be loaded: " + err.Error())
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	err = rt.Serve(ctx, listener)
	p.stop(stopTimeout)
	if err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	return exitOK
}
