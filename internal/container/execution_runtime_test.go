package container

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type poolLifecycleFake struct {
	starts, shutdowns atomic.Int64
	done              chan struct{}
}

func (f *poolLifecycleFake) Start(context.Context) error    { f.starts.Add(1); return nil }
func (f *poolLifecycleFake) BeginShutdown() <-chan struct{} { f.shutdowns.Add(1); return f.done }

func TestExecutionRuntimeStartsAndDrainsBothProviders(t *testing.T) {
	primary := &poolLifecycleFake{done: make(chan struct{})}
	cpa := &poolLifecycleFake{done: make(chan struct{})}
	runtime := &providerExecutionRuntime{primary: primary, cpa: cpa}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := runtime.BeginShutdown()
	if runtime.BeginShutdown() != done || primary.shutdowns.Load() != 1 || cpa.shutdowns.Load() != 1 || primary.starts.Load() != 1 {
		t.Fatal("lifecycle dispatched more than once")
	}
	close(primary.done)
	select {
	case <-done:
		t.Fatal("did not wait for CPA")
	default:
	}
	close(cpa.done)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
}
