package cpa

import "strconv"

// RetireCredential is called by the provider registry on identity removal/change.
func (a *Adapter) RetireCredential(id uint) {
	if a == nil {
		return
	}
	for _, provider := range a.providers {
		if retire, ok := provider.(interface{ RetireCredential(string) }); ok {
			retire.RetireCredential(strconv.FormatUint(uint64(id), 10))
		}
	}
}

// BeginShutdown starts draining owned resources without consuming the caller's
// shutdown deadline. The application already cancels active request contexts.
func (a *Adapter) BeginShutdown() <-chan struct{} {
	pending := make([]<-chan struct{}, 0)
	if a != nil {
		for _, provider := range a.providers {
			if shutdown, ok := provider.(interface{ BeginShutdown() <-chan struct{} }); ok {
				pending = append(pending, shutdown.BeginShutdown())
			}
		}
	}
	done := make(chan struct{})
	go func() {
		for _, wait := range pending {
			<-wait
		}
		close(done)
	}()
	return done
}
