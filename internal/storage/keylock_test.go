package storage

import (
	"sync"
	"testing"
	"time"
)

func TestLockObjectKeySerializesOneKeyOnly(t *testing.T) {
	unlockA := LockObjectKey("b", "a")
	got := make(chan struct{})
	go func() {
		defer LockObjectKey("b", "other")()
		close(got)
	}()
	<-got // another key is not held up
	done := make(chan struct{})
	go func() {
		defer LockObjectKey("b", "a")()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second holder of the same key got in")
	default:
	}
	unlockA()
	<-done
	var wg sync.WaitGroup
	n := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer LockObjectKey("b", "c")()
			n++
		}()
	}
	wg.Wait()
	if n != 50 {
		t.Fatalf("n = %d", n)
	}
	keyLocks.mu.Lock()
	left := len(keyLocks.m)
	keyLocks.mu.Unlock()
	if left != 0 {
		t.Errorf("%d locks left behind", left)
	}
}

// Two client writes of one key run at once (a client may start the second
// while still sending the first); a replica copy waits for both.
func TestClientWritesShareAKeyAndACopyWaits(t *testing.T) {
	first := LockObjectKeyShared("b", "k")
	second := make(chan func())
	go func() { second <- LockObjectKeyShared("b", "k") }()
	unlockSecond := <-second // did not wait for the first
	copied := make(chan struct{})
	go func() {
		defer LockObjectKey("b", "k")()
		close(copied)
	}()
	first()
	select {
	case <-copied:
		t.Fatal("a replica copy ran during a client write")
	default:
	}
	unlockSecond()
	<-copied
}

// A copy waiting for a client write does not hold back that client's next
// write of the key, or the client would wait for itself.
func TestAWaitingCopyDoesNotBlockClientWrites(t *testing.T) {
	first := LockObjectKeyShared("b", "w")
	copyDone := make(chan struct{})
	go func() {
		defer LockObjectKey("b", "w")()
		close(copyDone)
	}()
	time.Sleep(20 * time.Millisecond) // the copy is waiting now
	second := make(chan func())
	go func() { second <- LockObjectKeyShared("b", "w") }()
	select {
	case u := <-second:
		u()
	case <-time.After(2 * time.Second):
		t.Fatal("a client write waited behind a waiting copy")
	}
	first()
	<-copyDone
}
