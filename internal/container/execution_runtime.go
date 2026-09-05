package container

import (
	"context"
	"sync"

	"gpt-load/internal/app"
)

type executionShutdown interface{ BeginShutdown() <-chan struct{} }

// providerExecutionRuntime includes CPA resources in the same lifecycle as
// Bifrost; waiting remains asynchronous and bounded by the application.
type providerExecutionRuntime struct {
	primary app.ExecutionRuntime
	cpa     executionShutdown
	once    sync.Once
	done    chan struct{}
}

func (r *providerExecutionRuntime) Start(ctx context.Context) error { return r.primary.Start(ctx) }

func (r *providerExecutionRuntime) BeginShutdown() <-chan struct{} {
	r.once.Do(func() {
		r.done = make(chan struct{})
		primary, cpa := r.primary.BeginShutdown(), r.cpa.BeginShutdown()
		go func() { <-primary; <-cpa; close(r.done) }()
	})
	return r.done
}
