package gateway

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/config"
)

// Fork account limits and upstream group limits must release independently,
// including when group admission rejects after acquiring an account slot.
func TestAccountAndGroupConcurrencyReleaseTogether(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := &scriptedForwarder{results: []UpstreamResult{{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       []byte(`{"choices":[],"error":{"message":"test rejection"}}`),
			}}}
			h, manager, _ := newHandlerForTest(t, f, "test-upstream")
			publishHandlerPolicySettings(t, h, manager, 1,
				config.Settings{"account_concurrency_wait_timeout": 0},
				config.Settings{"account_concurrency_limit": 1, "concurrency_limit": 1})
			if status == http.StatusTooManyRequests {
				release, admitted := manager.Concurrency().TryAcquireGroup(1, 1)
				if !admitted {
					t.Fatal("could not saturate the group")
				}
				defer release()
			}
			engine := gin.New()
			bindGatewayRoutesForTest(t, engine, h)
			response := concurrencyRequest(engine, false)
			if response.Code != status {
				t.Fatalf("status = %d, want %d", response.Code, status)
			}
			h.accountConcurrency.mu.Lock()
			remaining := len(h.accountConcurrency.active)
			h.accountConcurrency.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("account slots leaked: %d", remaining)
			}
			counts := manager.Concurrency().Snapshot()
			wantGroup := int64(0)
			if status == http.StatusTooManyRequests {
				wantGroup = 1
				if len(f.inputs) != 0 {
					t.Fatal("saturated group dispatched upstream")
				}
			}
			if counts.Global != 0 || len(counts.AccessKeys) != 0 || counts.Groups[1] != wantGroup {
				t.Fatalf("upstream concurrency slots leaked: %+v", counts)
			}
		})
	}
}

func TestAccountConcurrencyRejectsNormalRequestWithoutLeakingSlots(t *testing.T) {
	f := &scriptedForwarder{results: []UpstreamResult{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       []byte(`{"choices":[]}`),
	}}}
	h, manager, _ := newHandlerForTest(t, f, "test-upstream")
	publishHandlerPolicySettings(t, h, manager, 1,
		config.Settings{"account_concurrency_wait_timeout": 0},
		config.Settings{"account_concurrency_limit": 1, "concurrency_limit": 1})
	release, admitted := h.accountConcurrency.tryAcquire(t.Context(), "1", 1, 0)
	if !admitted {
		t.Fatal("could not occupy account capacity")
	}
	defer release()
	engine := gin.New()
	bindGatewayRoutesForTest(t, engine, h)
	response := concurrencyRequest(engine, false)
	if response.Code != http.StatusTooManyRequests || len(f.inputs) != 0 || !bytes.Contains(response.Body.Bytes(), []byte("account_concurrency_limited")) {
		t.Fatalf("normal request bypassed account limit: status=%d calls=%d body=%s", response.Code, len(f.inputs), response.Body)
	}
	if counts := manager.Concurrency().Snapshot(); counts.Global != 0 || len(counts.AccessKeys) != 0 || len(counts.Groups) != 0 {
		t.Fatalf("rejected request leaked capacity: %+v", counts)
	}
	h.accountConcurrency.mu.Lock()
	remaining := h.accountConcurrency.active["1"]
	h.accountConcurrency.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("rejected request changed holder capacity: %d", remaining)
	}
	release()
	response = concurrencyRequest(engine, false)
	if response.Code != http.StatusOK || len(f.inputs) != 1 {
		t.Fatalf("released account did not recover: status=%d calls=%d", response.Code, len(f.inputs))
	}
	h.accountConcurrency.mu.Lock()
	remaining = len(h.accountConcurrency.active)
	h.accountConcurrency.mu.Unlock()
	if counts := manager.Concurrency().Snapshot(); remaining != 0 || counts.Global != 0 || len(counts.AccessKeys) != 0 || len(counts.Groups) != 0 {
		t.Fatalf("successful request leaked capacity: accounts=%d counts=%+v", remaining, counts)
	}
}
