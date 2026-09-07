package gateway

import (
	"testing"
)

func TestAccountConcurrencyLimiterTryAcquire(t *testing.T) {
	limiter := newAccountConcurrencyLimiter()
	release, ok := limiter.tryAcquire("account", 1)
	if !ok || release == nil { t.Fatal("first request was not admitted") }
	if _, ok := limiter.tryAcquire("account", 1); ok { t.Fatal("second request exceeded account limit") }
	if _, ok := limiter.tryAcquire("other", 1); !ok { t.Fatal("different account was blocked") }
	release()
	releaseAgain, ok := limiter.tryAcquire("account", 1)
	if !ok { t.Fatal("account was not released") }
	releaseAgain()
}

func TestAccountConcurrencyLimiterUnlimited(t *testing.T) {
	limiter := newAccountConcurrencyLimiter()
	for i := 0; i < 100; i++ {
		release, ok := limiter.tryAcquire("account", 0)
		if !ok { t.Fatal("unlimited account was blocked") }
		release()
	}
}
