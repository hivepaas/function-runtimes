package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivepaas/function-runtimes/hivepaas"
	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

// restartEvery bounds how often a process that exited, or could not load the
// handler, is replaced: a handler that ends its process each time does not
// spin.
const restartEvery = time.Second

// poolConfig is how many PHP processes run calls, and how they are started.
type poolConfig struct {
	// min processes are kept; up to max are started as calls need them.
	min, max    int
	worker      workerConfig
	loadTimeout time.Duration
	// idleTimeout: how often the processes above min that the calls did not
	// need are stopped; 0 for never.
	idleTimeout time.Duration
}

// pool runs calls in PHP processes, one call at a time each.
type pool struct {
	cfg poolConfig
	out output

	mu sync.Mutex
	// idle are the processes waiting for a call, the one waiting longest first;
	// waiters the calls waiting for a process, the first to come first.
	idle        []*worker
	waiters     []chan *worker
	live        map[*worker]struct{}
	starting    int
	stopping    bool
	lastRestart time.Time
	// busy are the calls running or waiting now, and peak the most there were
	// at once since the processes were last counted.
	busy, peak int

	loaded   chan struct{} // closed once the first process loaded the handler, or failed to
	loadErr  error
	loadOnce sync.Once
	ready    atomic.Bool
	done     chan struct{}
}

func newPool(cfg poolConfig) *pool {
	return &pool{
		cfg:    cfg,
		live:   map[*worker]struct{}{},
		loaded: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// start starts the first process, and returns once it loaded the handler or
// failed to: then the reason. The other processes min asks for start in the
// background.
func (p *pool) start() error {
	p.mu.Lock()
	p.starting++
	p.mu.Unlock()
	w, err := p.launch()
	p.loadOnce.Do(func() {
		p.loadErr = err
		p.ready.Store(err == nil)
		close(p.loaded)
	})
	if err != nil {
		return err
	}
	p.putIdle(w)
	for range p.cfg.min - 1 {
		p.grow()
	}
	if p.cfg.idleTimeout > 0 && p.cfg.min < p.cfg.max {
		go p.shrinkEvery(p.cfg.idleTimeout)
	}
	return nil
}

// isLoaded says the handler is loaded: health answers 200.
func (p *pool) isLoaded() bool {
	return p.ready.Load()
}

// launch starts a process, counted in starting by the caller, and waits for it
// to load the handler.
func (p *pool) launch() (*worker, error) {
	w, err := startWorker(p.cfg.worker, p.out)
	p.mu.Lock()
	p.starting--
	if err == nil {
		p.live[w] = struct{}{}
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go p.watch(w)
	if err := w.load(p.cfg.loadTimeout); err != nil {
		return nil, err
	}
	return w, nil
}

// grow starts a process in the background when there are fewer than max; it
// goes to idle, or to a waiting call, once it loaded the handler.
func (p *pool) grow() {
	p.mu.Lock()
	if p.stopping || len(p.live)+p.starting >= p.cfg.max {
		p.mu.Unlock()
		return
	}
	p.starting++
	p.mu.Unlock()
	go p.launchIdle()
}

func (p *pool) launchIdle() {
	w, err := p.launch()
	if err != nil {
		p.out.ErrorOutput("hivepaas: a PHP process could not load the handler: " + err.Error())
		return
	}
	p.putIdle(w)
}

// watch waits for a process to exit, says so when it exited on its own, and
// replaces it when fewer than min are left. A process stopped at a call's
// timeout is replaced at once; one that exited, or could not load the handler,
// at most once every restartEvery.
func (p *pool) watch(w *worker) {
	<-w.exited
	p.mu.Lock()
	delete(p.live, w)
	p.idle = slices.DeleteFunc(p.idle, func(idle *worker) bool { return idle == w })
	if p.stopping || !p.ready.Load() {
		// Stopping, or the first process could not load the handler: no other
		// is started.
		p.mu.Unlock()
		return
	}
	short := len(p.live)+p.starting < p.cfg.min
	wait := time.Duration(0)
	if !w.timedOut.Load() {
		wait = max(0, time.Until(p.lastRestart.Add(restartEvery)))
		p.lastRestart = time.Now().Add(wait)
	}
	p.mu.Unlock()
	if !w.stopped.Load() {
		p.out.ErrorOutput("hivepaas: process " + strconv.Itoa(w.pid) + " exited (" + w.state + "); starting another")
	}
	if short {
		time.AfterFunc(wait, p.grow)
	}
}

// handle runs a call in a PHP process: the runtime's handler.
func (p *pool) handle(ctx context.Context, req *hivepaas.Request) (*hivepaas.Response, error) {
	select {
	case <-p.loaded:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the handler to be loaded: %w", ctx.Err())
	}
	if p.loadErr != nil {
		return nil, fmt.Errorf("%w: %w", serve.ErrNotLoaded, p.loadErr)
	}
	w, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := w.call(ctx, req)
	p.release(w)
	return resp, err
}

// acquire takes an idle process, starting one when none is and fewer than max
// are, and waits for one while the call may.
func (p *pool) acquire(ctx context.Context) (*worker, error) {
	p.mu.Lock()
	p.busy++
	p.peak = max(p.peak, p.busy)
	for len(p.idle) > 0 {
		w := p.idle[0]
		p.idle = p.idle[1:]
		if w.alive() {
			p.mu.Unlock()
			return w, nil
		}
	}
	given := make(chan *worker, 1)
	p.waiters = append(p.waiters, given)
	start := !p.stopping && len(p.live)+p.starting < p.cfg.max
	if start {
		p.starting++
	}
	p.mu.Unlock()
	if start {
		go p.launchIdle()
	}

	select {
	case w := <-given:
		return w, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.busy--
		waiting := slices.Contains(p.waiters, given)
		p.waiters = slices.DeleteFunc(p.waiters, func(c chan *worker) bool { return c == given })
		p.mu.Unlock()
		if !waiting {
			// A process was given as the call ended: it goes to another.
			p.putIdle(<-given)
		}
		return nil, fmt.Errorf("waiting for a PHP process: %w", ctx.Err())
	}
}

// release gives a process back after its call. One stopped at the call's
// timeout is said to have been.
func (p *pool) release(w *worker) {
	p.mu.Lock()
	p.busy--
	p.mu.Unlock()
	if w.timedOut.Load() {
		p.out.ErrorOutput("hivepaas: process " + strconv.Itoa(w.pid) + " stopped: its call outlived the timeout")
		return
	}
	p.putIdle(w)
}

// putIdle gives a process to the call waiting longest, or keeps it idle.
func (p *pool) putIdle(w *worker) {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		w.finish()
		return
	}
	if !w.alive() {
		p.mu.Unlock()
		return
	}
	if len(p.waiters) > 0 {
		given := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.mu.Unlock()
		given <- w
		return
	}
	p.idle = append(p.idle, w)
	p.mu.Unlock()
}

// shrinkEvery stops, every interval, the idle processes above what the calls
// needed at once since the last time, and above min.
func (p *pool) shrinkEvery(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.shrink()
		}
	}
}

func (p *pool) shrink() {
	p.mu.Lock()
	keep := max(p.cfg.min, p.peak)
	p.peak = p.busy
	var retired []*worker
	for excess := len(p.live) - keep; excess > 0 && len(p.idle) > 0; excess-- {
		retired = append(retired, p.idle[0])
		p.idle = p.idle[1:]
	}
	p.mu.Unlock()
	for _, w := range retired {
		w.finish()
	}
}

// stop asks every process to exit once its call is done, and kills those that
// have not within timeout.
func (p *pool) stop(timeout time.Duration) {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	workers := make([]*worker, 0, len(p.live))
	for w := range p.live {
		workers = append(workers, w)
	}
	p.idle = nil
	p.mu.Unlock()
	close(p.done)
	for _, w := range workers {
		w.finish()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, w := range workers {
		select {
		case <-w.exited:
		case <-deadline.C:
			for _, left := range workers {
				left.stop()
			}
			return
		}
	}
}
