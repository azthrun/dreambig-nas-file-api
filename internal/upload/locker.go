package upload

import (
	"context"
	"sync"

	"github.com/tus/tusd/v2/pkg/handler"
)

// locker is tusd's in-memory Upload lock, with memorylocker's semantics (a contender
// asks the holder to release, then waits), plus tryLock for expiry. memorylocker itself
// cannot be used: every Lock asks the holder to release, so expiry probing an Upload
// would cut off a Caller's request in progress.
type locker struct {
	mu   sync.Mutex
	held map[string]*lockEntry
}

type lockEntry struct {
	released       chan struct{}
	requestRelease func()
}

func newLocker() *locker { return &locker{held: map[string]*lockEntry{}} }

func (l *locker) NewLock(id string) (handler.Lock, error) { return &uploadLock{l: l, id: id}, nil }

// tryLock takes id's lock only if nobody holds it, never disturbing a holder. A
// request wanting the lock meanwhile waits for unlock.
func (l *locker) tryLock(id string) (unlock func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.held[id]; busy {
		return nil, false
	}
	l.held[id] = &lockEntry{released: make(chan struct{}), requestRelease: func() {}}
	k := &uploadLock{l: l, id: id}
	return func() { _ = k.Unlock() }, true
}

type uploadLock struct {
	l  *locker
	id string
}

func (k *uploadLock) Lock(ctx context.Context, requestRelease func()) error {
	for {
		k.l.mu.Lock()
		e, busy := k.l.held[k.id]
		if !busy {
			k.l.held[k.id] = &lockEntry{released: make(chan struct{}), requestRelease: requestRelease}
			k.l.mu.Unlock()
			return nil
		}
		k.l.mu.Unlock()
		e.requestRelease()
		select {
		case <-ctx.Done():
			return handler.ErrLockTimeout
		case <-e.released:
		}
	}
}

func (k *uploadLock) Unlock() error {
	k.l.mu.Lock()
	defer k.l.mu.Unlock()
	if e, ok := k.l.held[k.id]; ok {
		delete(k.l.held, k.id)
		close(e.released)
	}
	return nil
}
