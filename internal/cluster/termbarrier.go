package cluster

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
)

// errCatchingUp is a leader read that came before the leader caught up.
var errCatchingUp = errors.New("cluster: the leader is still applying the previous term's writes")

// termBarrier makes a newly elected leader apply every entry committed
// before it took over, once per term, before it serves a consistent read.
// raft.Barrier's timeout bounds only queueing the entry, not waiting for it
// to apply, so a leader cut off from its peers held its first read until it
// stepped down; the wait here is the caller's own timeout, and concurrent
// first reads share one barrier.
type termBarrier struct {
	ready atomic.Uint64 // the term in which this leader last caught up
	mu    sync.Mutex
	cur   *barrierCall
}

type barrierCall struct {
	term uint64
	done chan struct{}
	err  error
}

func (b *termBarrier) caughtUp(r *raft.Raft, timeout time.Duration) error {
	term := r.CurrentTerm()
	if b.ready.Load() == term {
		return nil
	}
	b.mu.Lock()
	c := b.cur
	if c == nil || c.term != term || finishedWithError(c) {
		c = &barrierCall{term: term, done: make(chan struct{})}
		b.cur = c
		go func() {
			c.err = r.Barrier(0).Error()
			if c.err == nil {
				b.ready.Store(term)
			}
			close(c.done)
		}()
	}
	b.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.done:
		return c.err
	case <-t.C:
		return errCatchingUp
	}
}

func finishedWithError(c *barrierCall) bool {
	select {
	case <-c.done:
		return c.err != nil
	default:
		return false
	}
}
