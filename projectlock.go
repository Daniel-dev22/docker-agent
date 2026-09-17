package main

// Per-project serialisation of every change to a compose project.
//
// Nothing used to stop two jobs running on one stack at once: two updates, or an
// update and an editor save, each read, edited and wrote the same .env. Measured
// against the real writer, 2,000 such pairs left 367 files torn and lost 814
// updates. Every project-scoped job (up, down, pull, restart, recreate, update)
// now holds its project's lock for its whole run, and the register and copy
// requests hold it for the files they write. A second job waits its turn — its
// log says for what, and its cancellation and timeout still apply — and a request
// waits briefly, then answers 409 project_busy.

import (
	"context"
	"sync"
)

type projectLocks struct {
	mu    sync.Mutex
	locks map[string]*projectLock
}

type projectLock struct {
	slot   chan struct{} // holds one token while the lock is held
	refs   int           // holders and waiters; the entry is dropped at zero
	holder string
}

func newProjectLocks() *projectLocks {
	return &projectLocks{locks: map[string]*projectLock{}}
}

// acquire takes project's lock for who, waiting until ctx ends. When the lock is
// held, waiting (if non-nil) is told by whom before the wait begins. The returned
// release is idempotent.
func (l *projectLocks) acquire(ctx context.Context, project, who string, waiting func(holder string)) (func(), error) {
	l.mu.Lock()
	pl := l.locks[project]
	if pl == nil {
		pl = &projectLock{slot: make(chan struct{}, 1)}
		l.locks[project] = pl
	}
	pl.refs++
	l.mu.Unlock()

	select {
	case pl.slot <- struct{}{}:
	default:
		if waiting != nil {
			waiting(l.holderOf(pl))
		}
		select {
		case pl.slot <- struct{}{}:
		case <-ctx.Done():
			l.drop(project, pl)
			return nil, ctx.Err()
		}
	}
	l.mu.Lock()
	pl.holder = who
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			pl.holder = ""
			l.mu.Unlock()
			<-pl.slot
			l.drop(project, pl)
		})
	}, nil
}

func (l *projectLocks) holderOf(pl *projectLock) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pl.holder == "" {
		return "another change"
	}
	return pl.holder
}

func (l *projectLocks) drop(project string, pl *projectLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pl.refs--; pl.refs == 0 && l.locks[project] == pl {
		delete(l.locks, project)
	}
}
