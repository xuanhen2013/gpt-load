package gateway

import (
	"context"
	"testing"
	"testing/synctest"
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

// 虚拟时钟确保验证的是账号等待预算，而不是请求 deadline 或真实调度延迟。
func TestAccountConcurrencyLimiterWaitDeadline(t *testing.T) {
	for _, test := range []struct {
		name           string
		waitTimeout    time.Duration
		contextTimeout time.Duration
		wantContextErr bool
	}{
		{"account timeout", 20 * time.Millisecond, 200 * time.Millisecond, false},
		{"context timeout", 200 * time.Millisecond, 20 * time.Millisecond, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limiter := newAccountConcurrencyLimiter()
				release, ok := limiter.tryAcquire(t.Context(), "account", 1, 0)
				if !ok {
					t.Fatal("could not occupy account")
				}
				defer release()
				ctx, cancel := context.WithTimeout(t.Context(), test.contextTimeout)
				defer cancel()
				started := time.Now()
				if _, admitted := limiter.tryAcquire(ctx, "account", 1, test.waitTimeout); admitted {
					t.Fatal("occupied account admitted a waiter")
				}
				if elapsed := time.Since(started); elapsed != 20*time.Millisecond {
					t.Fatalf("wait elapsed = %s, want 20ms", elapsed)
				}
				if got := ctx.Err() != nil; got != test.wantContextErr {
					t.Fatalf("context canceled = %t, want %t", got, test.wantContextErr)
				}
				if _, admitted := limiter.tryAcquire(t.Context(), "account", 1, 0); admitted {
					t.Fatal("timed-out waiter released the holder's slot")
				}
				release()
				releaseNext, admitted := limiter.tryAcquire(t.Context(), "account", 1, 0)
				if !admitted {
					t.Fatal("timed-out waiter leaked an account slot")
				}
				releaseNext()
			})
		})
	}
}
