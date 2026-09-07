package gateway

import "sync"

// accountConcurrencyLimiter tracks in-flight upstream attempts. A zero limit
// means unlimited; callers use TryAcquire so a busy account can be skipped in
// favour of another scheduler candidate without blocking the request.
type accountConcurrencyLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

func newAccountConcurrencyLimiter() *accountConcurrencyLimiter {
	return &accountConcurrencyLimiter{active: make(map[string]int)}
}

func (l *accountConcurrencyLimiter) tryAcquire(key string, limit int) (func(), bool) {
	if l == nil || limit <= 0 || key == "" {
		return func() {}, true
	}
	l.mu.Lock()
	if l.active[key] >= limit {
		l.mu.Unlock()
		return nil, false
	}
	l.active[key]++
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
			l.mu.Unlock()
		})
	}, true
}
