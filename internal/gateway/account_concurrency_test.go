package gateway

import (
	"context"
	"testing"
	"time"
)

func TestAccountConcurrencyLimiterTryAcquire(t *testing.T) {
	limiter := newAccountConcurrencyLimiter()
	release, ok := limiter.tryAcquire(context.Background(), "account", 1, 0)
	if !ok || release == nil {
		t.Fatal("first request was not admitted")
	}
	if _, ok := limiter.tryAcquire(context.Background(), "account", 1, 0); ok {
		t.Fatal("second request exceeded account limit")
	}
	if _, ok := limiter.tryAcquire(context.Background(), "other", 1, 0); !ok {
		t.Fatal("different account was blocked")
	}
	release()
	releaseAgain, ok := limiter.tryAcquire(context.Background(), "account", 1, 0)
	if !ok {
		t.Fatal("account was not released")
	}
	releaseAgain()
}

func TestAccountConcurrencyLimiterUnlimited(t *testing.T) {
	limiter := newAccountConcurrencyLimiter()
	for i := 0; i < 100; i++ {
		release, ok := limiter.tryAcquire(context.Background(), "account", 0, 0)
		if !ok {
			t.Fatal("unlimited account was blocked")
		}
		release()
	}
}

func TestAccountConcurrencyLimiterWaitsForRelease(t *testing.T) {
	l := newAccountConcurrencyLimiter()
	release, _ := l.tryAcquire(context.Background(), "a", 1, 0)
	done := make(chan bool, 1)
	go func() {
		r, ok := l.tryAcquire(context.Background(), "a", 1, time.Second)
		if ok {
			r()
		}
		done <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	release()
	if !<-done {
		t.Fatal("waiter did not acquire after release")
	}
}
