package cluster

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent read barriers share questions to the leader, and every answer
// still covers the writes the leader had acknowledged when the read began.
// The first version shared a question only with readers that came before it
// started, so under load each reader sent its own (5.0.14 review).
func TestReadBarriersShareLeaderQuestions(t *testing.T) {
	var leaderIndex, asked atomic.Uint64
	n := &Node{}
	n.askLeader = func(time.Duration) (uint64, error) {
		asked.Add(1)
		idx := leaderIndex.Load() // what the leader has when the question arrives
		time.Sleep(20 * time.Millisecond)
		return idx, nil
	}
	stop := make(chan struct{})
	go func() { // writes keep landing on the leader
		for {
			select {
			case <-stop:
				return
			default:
				leaderIndex.Add(1)
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	var wg sync.WaitGroup
	var stale atomic.Int32
	const readers = 400
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i%40) * time.Millisecond)
			atStart := leaderIndex.Load()
			idx, err := n.leaderAppliedIndexSince(time.Now(), time.Second)
			if err != nil {
				t.Error(err)
			}
			if idx < atStart {
				stale.Add(1)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	if stale.Load() > 0 {
		t.Errorf("%d reads got an index older than the leader's when they began", stale.Load())
	}
	// About 40ms of arrivals over 20ms questions: a handful, not 400.
	if a := asked.Load(); a > 12 {
		t.Errorf("%d readers sent %d questions to the leader", readers, a)
	}
	t.Logf("%d readers, %d questions", readers, asked.Load())
}
