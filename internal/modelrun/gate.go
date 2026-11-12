package modelrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The gate is the launcher's one point of control (design: governed work
// classes, section 6, after OpenAI's Control Room). Every process the launcher
// starts, a model call or a validation run, asks the gate first and is refused
// on any answer but yes. While the process runs the launcher asks again every
// gateInterval and kills the process group when the gate refuses. A Box that
// registered no gate cannot start anything: no answer means no.
//
// The Box registers a gate that reads the emergency state, so an Owner freeze
// stops model work that is already running within a bounded time, and not
// only at the next Attempt lease.

// ErrRefused is what a gate returns, wrapped, when its answer is a definite
// no (for example break glass is FROZEN). Any other error means the gate
// could not answer.
var ErrRefused = errors.New("refused by the emergency gate")

// Gate answers whether a process may start or keep running.
type Gate func(ctx context.Context) error

var (
	gateMu sync.RWMutex
	gate   Gate

	// gateInterval is how often a running process is checked. With the kill
	// path it bounds the time from a freeze to the end of a model process.
	gateInterval = 5 * time.Second
	// gateFailureLimit is how many checks in a row may fail to answer before
	// a running process is stopped. A definite refusal stops it at once.
	gateFailureLimit = 3
)

// SetGate registers the gate. The Box does this once at startup.
func SetGate(g Gate) {
	gateMu.Lock()
	defer gateMu.Unlock()
	gate = g
}

func currentGate() Gate {
	gateMu.RLock()
	defer gateMu.RUnlock()
	return gate
}

// checkGateAtStart refuses unless a gate is registered and says yes.
func checkGateAtStart(ctx context.Context) error {
	g := currentGate()
	if g == nil {
		return fmt.Errorf("modelrun: %w: no gate is registered, so nothing may start", ErrRefused)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := g(checkCtx); err != nil {
		if errors.Is(err, ErrRefused) {
			return fmt.Errorf("modelrun: %w", err)
		}
		return fmt.Errorf("modelrun: %w: the gate could not answer: %v", ErrRefused, err)
	}
	return nil
}

// watchGate stops the process when the gate refuses, or cannot answer
// gateFailureLimit times in a row. It ends with the call.
//
// The gate and the timing are read by Start, while the call still owns them:
// this goroutine can start late, and it must not read package state that a
// later test writes.
func (c *Cmd) watchGate(g Gate, interval time.Duration, limit int) {
	// Wait waits for this goroutine, so nothing of this call reads package
	// state after the call has returned.
	defer close(c.gateDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		// The check ends with the call, so Wait never waits for a slow gate.
		checkCtx, cancel := context.WithTimeout(context.Background(), interval)
		stop := context.AfterFunc(doneContext(c.done), cancel)
		err := g(checkCtx)
		stop()
		cancel()
		select {
		case <-c.done:
			return
		default:
		}
		switch {
		case err == nil:
			failures = 0
			continue
		case errors.Is(err, ErrRefused):
		default:
			if failures++; failures < limit {
				continue
			}
			err = fmt.Errorf("%w: the gate could not answer %d times in a row: %v", ErrRefused, failures, err)
		}
		c.gateErr.Store(&err)
		if cancelFn := c.Cmd.Cancel; cancelFn != nil {
			_ = cancelFn()
		}
		return
	}
}

// doneContext is a context that ends when done is closed.
func doneContext(done <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-done
		cancel()
	}()
	return ctx
}
