package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

// A PHP process and the server talk over two pipes, the process's file
// descriptors 3 (calls) and 4 (replies). Each message is a line of JSON, then
// as many bytes of body as the line says. The process's standard output and
// error are the function's own: they are passed on line by line. After a call
// the process writes outputSync and the call's number on its standard output,
// so that what the call printed is passed on before the call is answered.
const outputSync = "\x1fhivepaas-sync:"

// How long a call's answer waits for what it printed to be passed on; a
// process whose standard output was closed never says it was.
const syncWait = 100 * time.Millisecond

// maxLine bounds a line of the function's output: a longer one is passed on
// in pieces of this size. replyBuffer is how much of the replies is read at
// once, and syncedCalls how many calls' syncs are kept for a call to read.
const (
	maxLine     = 64 << 10
	replyBuffer = 64 << 10
	syncedCalls = 16
)

var (
	errExited        = errors.New("the PHP process exited")
	errRepliesClosed = errors.New("the PHP process closed its replies")
	errLoadTimeout   = errors.New("loading the handler took longer")
	errNotALoadReply = errors.New("the PHP process said something else than whether it loaded the handler")
	errOutOfStep     = errors.New("the PHP process answered another call")
	errCallTimedOut  = errors.New("the call outlived its timeout")
)

// phpError is what the PHP process said went wrong, in its words.
type phpError struct{ msg string }

func (e *phpError) Error() string { return e.msg }

// callMessage is a call, as the PHP process reads it.
type callMessage struct {
	Seq        uint64              `json:"seq"`
	ID         string              `json:"id"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      map[string][]string `json:"query"`
	Headers    map[string][]string `json:"headers"`
	Deadline   float64             `json:"deadline"`
	BodyLength int                 `json:"bodyLength"`
}

// replyMessage is what the PHP process answers: whether it loaded the handler,
// first; then each call's response, or why it failed.
type replyMessage struct {
	Loaded     *bool               `json:"loaded,omitempty"`
	Seq        uint64              `json:"seq"`
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	BodyLength int                 `json:"bodyLength"`
	Error      string              `json:"error"`
	Outcome    string              `json:"outcome"`
}

// output is where the function's output goes: the runtime's own.
type output interface {
	Output(line string)
	ErrorOutput(line string)
}

// workerConfig is how a PHP process is started.
type workerConfig struct {
	command []string
	dir     string
	env     []string
}

// worker is one PHP process.
type worker struct {
	pid     int
	cmd     *exec.Cmd
	calls   *os.File
	replies *bufio.Reader
	// synced are the calls whose output has been passed on, by number.
	synced chan uint64
	// exited is closed once the process has exited, its state then said.
	exited chan struct{}
	state  string
	seq    uint64
	// stopped says the server stopped it, at a call's timeout or as the
	// server stops: its exit says nothing of its own.
	stopped atomic.Bool
	// timedOut: it was stopped because its call outlived the timeout.
	timedOut atomic.Bool
	// unsynced: its output was found closed; calls no longer wait for it.
	unsynced  atomic.Bool
	closeOnce sync.Once
}

// startWorker starts a PHP process, its output passed on to out.
func startWorker(wc workerConfig, out output) (*worker, error) {
	var pipes [4][2]*os.File // calls, replies, stdout, stderr: read and write ends
	closeAll := func() {
		for _, p := range pipes {
			for _, f := range p {
				if f != nil {
					_ = f.Close()
				}
			}
		}
	}
	for i := range pipes {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("a pipe for the PHP process: %w", err)
		}
		pipes[i] = [2]*os.File{r, w}
	}
	cmd := exec.Command(wc.command[0], wc.command[1:]...) //nolint:gosec // the image's own PHP and script
	cmd.Dir = wc.dir
	cmd.Env = wc.env
	cmd.Stdout, cmd.Stderr = pipes[2][1], pipes[3][1]
	cmd.ExtraFiles = []*os.File{pipes[0][0], pipes[1][1]}
	// A process group of its own: stopping it stops what it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("starting PHP: %w", err)
	}
	// The process's ends are its own now.
	for _, f := range []*os.File{pipes[0][0], pipes[1][1], pipes[2][1], pipes[3][1]} {
		_ = f.Close()
	}
	w := &worker{
		pid:     cmd.Process.Pid,
		cmd:     cmd,
		calls:   pipes[0][1],
		replies: bufio.NewReaderSize(pipes[1][0], replyBuffer),
		synced:  make(chan uint64, syncedCalls),
		exited:  make(chan struct{}),
	}
	go passOutput(pipes[2][0], out.Output, w.synced)
	go passOutput(pipes[3][0], out.ErrorOutput, nil)
	go func() {
		_ = cmd.Wait()
		w.state = exitState(cmd.ProcessState)
		_ = pipes[1][0].Close()
		close(w.exited)
	}()
	return w, nil
}

// exitState says how a process ended: "code 1", "signal 9".
func exitState(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "signal " + strconv.Itoa(int(ws.Signal()))
	}
	return "code " + strconv.Itoa(ps.ExitCode())
}

// passOutput passes on what r carries, line by line, to pass; the call numbers
// of the sync lines among them go to synced.
func passOutput(r *os.File, pass func(string), synced chan<- uint64) {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			passLine(line, pass, synced)
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func passLine(line []byte, pass func(string), synced chan<- uint64) {
	text := string(line)
	if n := len(text); n > 0 && text[n-1] == '\n' {
		text = text[:n-1]
	}
	if synced != nil {
		if at := strings.LastIndex(text, outputSync); at >= 0 {
			if seq, err := strconv.ParseUint(text[at+len(outputSync):], 10, 64); err == nil {
				if at > 0 {
					pass(text[:at]) // the end of a line the function did not finish
				}
				select {
				case synced <- seq:
				default: // no call waits for it any more
				}
				return
			}
		}
	}
	pass(text)
}

// load waits for the process to say it loaded the handler, at most for
// timeout.
func (w *worker) load(timeout time.Duration) error {
	read := make(chan error, 1)
	go func() {
		reply, err := w.readReply()
		switch {
		case err != nil:
			read <- err
		case reply.Loaded == nil:
			read <- errNotALoadReply
		case !*reply.Loaded:
			read <- &phpError{reply.Error}
		default:
			read <- nil
		}
	}()
	select {
	case err := <-read:
		if err != nil {
			w.stop()
			return w.why(err)
		}
		return nil
	case <-time.After(timeout):
		w.stop()
		return fmt.Errorf("%w than %v", errLoadTimeout, timeout)
	}
}

// why says what ended an exchange with the process: its own words, or its
// exit.
func (w *worker) why(err error) error {
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, os.ErrClosed) &&
		!errors.Is(err, syscall.EPIPE) {
		return err
	}
	select {
	case <-w.exited:
		return fmt.Errorf("%w (%s)", errExited, w.state)
	case <-time.After(time.Second):
		return errRepliesClosed
	}
}

// call runs one call in the process, and answers its response. At the call's
// end - its context's deadline - the process is stopped: PHP cannot be
// interrupted, and the call it runs cannot be let run on.
func (w *worker) call(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
	w.seq++
	msg := callMessage{
		Seq:        w.seq,
		ID:         hivepaas.RequestID(ctx),
		Method:     req.Method,
		Path:       req.Path,
		Query:      req.Query,
		Headers:    lowerHeaders(req.Headers),
		BodyLength: len(req.Body),
	}
	if deadline, ok := ctx.Deadline(); ok {
		msg.Deadline = float64(deadline.UnixMicro()) / float64(time.Second/time.Microsecond)
	}
	type exchanged struct {
		resp *hivepaas.Response
		err  error
	}
	done := make(chan exchanged, 1)
	go func() {
		resp, err := w.exchange(&msg, req.Body)
		done <- exchanged{resp, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		w.waitForOutput(msg.Seq)
		return r.resp, nil
	case <-ctx.Done():
		w.timedOut.Store(true)
		w.stop()
		return nil, fmt.Errorf("%w: %w", errCallTimedOut, ctx.Err())
	}
}

// exchange sends a call and reads its reply.
func (w *worker) exchange(msg *callMessage, body []byte) (*hivepaas.Response, error) {
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("the call cannot be sent: %w", err)
	}
	if _, err := w.calls.Write(append(append(line, '\n'), body...)); err != nil {
		return nil, w.why(err)
	}
	reply, err := w.readReply()
	if err != nil {
		return nil, w.why(err)
	}
	if reply.Seq != msg.Seq {
		w.stop()
		return nil, fmt.Errorf("%w: %d, to call %d", errOutOfStep, reply.Seq, msg.Seq)
	}
	if reply.Error != "" {
		if reply.Outcome == "response_too_large" {
			return nil, fmt.Errorf("%w: %s", serve.ErrResponseTooLarge, reply.Error)
		}
		return nil, &phpError{reply.Error}
	}
	return &hivepaas.Response{Status: reply.Status, Headers: reply.headers(), Body: reply.body}, nil
}

// readReply reads a reply and its body.
func (w *worker) readReply() (*reply, error) {
	line, err := w.replies.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("reading the PHP process's reply: %w", err)
	}
	var r reply
	if err := json.Unmarshal(line, &r.replyMessage); err != nil {
		return nil, fmt.Errorf("the PHP process's reply is not JSON: %w", err)
	}
	if r.BodyLength > 0 {
		r.body = make([]byte, r.BodyLength)
		if _, err := io.ReadFull(w.replies, r.body); err != nil {
			return nil, fmt.Errorf("reading the reply's body: %w", err)
		}
	}
	return &r, nil
}

type reply struct {
	replyMessage
	body []byte
}

func (r *reply) headers() http.Header {
	h := http.Header{}
	for name, values := range r.Headers {
		for _, value := range values {
			h.Add(name, value)
		}
	}
	return h
}

// waitForOutput waits for what call seq printed to be passed on.
func (w *worker) waitForOutput(seq uint64) {
	if w.unsynced.Load() {
		return
	}
	timeout := time.NewTimer(syncWait)
	defer timeout.Stop()
	for {
		select {
		case got := <-w.synced:
			if got >= seq {
				return
			}
		case <-timeout.C:
			w.unsynced.Store(true)
			return
		case <-w.exited:
			return
		}
	}
}

// stop kills the process and what it started.
func (w *worker) stop() {
	w.stopped.Store(true)
	_ = syscall.Kill(-w.pid, syscall.SIGKILL)
	w.closeCalls()
}

// finish asks the process to exit once its call is done: its calls end.
func (w *worker) finish() {
	w.stopped.Store(true)
	w.closeCalls()
}

func (w *worker) closeCalls() {
	w.closeOnce.Do(func() { _ = w.calls.Close() })
}

func (w *worker) alive() bool {
	select {
	case <-w.exited:
		return false
	default:
		return !w.stopped.Load()
	}
}

// lowerHeaders are the request's headers by name in lower case.
func lowerHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		key := strings.ToLower(name)
		out[key] = append(out[key], values...)
	}
	return out
}
