package storage

import "sync"

// Per-key locks between client writes of an object's plain (unversioned)
// data on this node and replica copies of it arriving from other nodes.
//
// A client write holds the key shared from the engine write to the metadata
// commit; a replica copy holds it exclusively from checking the metadata to
// its own write. Without it, a copy of the previous object that was checked
// before a client overwrite could land after it and replace the new bytes.
// Client writes never wait for each other: a client may start a second
// upload of a key while the first is still sending (S3 allows it, and the
// conformance suite does it), and a lock held across the first body would
// never be released.

// LockObjectKeyShared is the lock a client write takes. It returns the
// unlock.
func LockObjectKeyShared(bucket, key string) func() {
	l, release := keyLockFor(bucket, key)
	l.mu.Lock()
	for l.copying {
		l.cond.Wait()
	}
	l.writers++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		if l.writers--; l.writers == 0 {
			l.cond.Broadcast()
		}
		l.mu.Unlock()
		release()
	}
}

// LockObjectKey is the lock a replica copy takes. It returns the unlock.
func LockObjectKey(bucket, key string) func() {
	l, release := keyLockFor(bucket, key)
	l.mu.Lock()
	for l.copying || l.writers > 0 {
		l.cond.Wait()
	}
	l.copying = true
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.copying = false
		l.cond.Broadcast()
		l.mu.Unlock()
		release()
	}
}

func keyLockFor(bucket, key string) (*keyLock, func()) {
	id := bucket + "\x00" + key
	keyLocks.mu.Lock()
	if keyLocks.m == nil {
		keyLocks.m = map[string]*keyLock{}
	}
	l := keyLocks.m[id]
	if l == nil {
		l = &keyLock{}
		l.cond = sync.NewCond(&l.mu)
		keyLocks.m[id] = l
	}
	l.refs++
	keyLocks.mu.Unlock()
	return l, func() {
		keyLocks.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(keyLocks.m, id)
		}
		keyLocks.mu.Unlock()
	}
}

// keyLock lets any number of client writes in at once, or one copy. A
// waiting copy does not hold new client writes back (sync.RWMutex would):
// a client sending two uploads of a key, the second before the first ends,
// would then wait for itself.
type keyLock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	writers int
	copying bool
	refs    int // guarded by keyLocks.mu
}

var keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}
