package github

import "sync"

// keyedMutex serializes operations per key, so one chat's read-verdict-act (which spans a GitHub call
// and can't be one SQL transaction) never races itself while unrelated chats never wait.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

// refMutex counts holders plus waiters, so the map entry is dropped once nobody needs it.
type refMutex struct {
	sync.Mutex
	n int
}

// Lock blocks until key's lock is held, returning the func that releases it.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*refMutex{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &refMutex{}
		k.locks[key] = l
	}
	l.n++
	k.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.n--; l.n == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
