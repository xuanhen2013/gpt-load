package gateway

import (
	"context"
	"sync"
	"time"
)

// accountConcurrencyLimiter tracks in-flight upstream attempts. A zero limit
// means unlimited; callers use TryAcquire so a busy account can be skipped in
// favour of another scheduler candidate without blocking the request.
type accountConcurrencyLimiter struct {
	mu     sync.Mutex
	active map[string]int
	wake   map[string]chan struct{}
}

func newAccountConcurrencyLimiter() *accountConcurrencyLimiter {
	return &accountConcurrencyLimiter{active: make(map[string]int), wake: make(map[string]chan struct{})}
}

func (l *accountConcurrencyLimiter) tryAcquire(ctx context.Context, key string, limit int, timeout time.Duration) (func(), bool) {
	if l == nil || limit <= 0 || key == "" {
		return func() {}, true
	}
	deadline := time.Now().Add(timeout)
	for {
		l.mu.Lock()
		if l.active[key] < limit {
			l.active[key]++
			if l.wake[key] == nil {
				l.wake[key] = make(chan struct{})
			}
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					if l.active[key] > 1 {
						l.active[key]--
					} else {
						delete(l.active, key)
					}
					ch := l.wake[key]
					close(ch)
					l.wake[key] = make(chan struct{})
					l.mu.Unlock()
				})
			}, true
		}
		if l.wake[key] == nil {
			l.wake[key] = make(chan struct{})
		}
		ch := l.wake[key]
		l.mu.Unlock()
		remaining := time.Until(deadline)
		if timeout <= 0 || remaining <= 0 {
			return nil, false
		}
		t := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C
			}
			return nil, false
		case <-ch:
			if !t.Stop() {
				<-t.C
			}
		}
	}
}
