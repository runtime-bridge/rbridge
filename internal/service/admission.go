package service

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/governor"
)

// admission lets sessions start in the order they asked, as slots free up.
// The number of slots is execution.max_concurrent_sessions, lowered by the
// governor while the machine is busy (never to zero). Waiting sessions form
// a queue: only the one at its head may take a slot, so a session that asked
// later never overtakes one that has waited longer.
type admission struct {
	limit int
	gov   *governor.Governor

	mu     sync.Mutex
	active int
	queue  []chan struct{}
}

func newAdmission(limit int, gov *governor.Governor) *admission {
	return &admission{limit: max(1, limit), gov: gov}
}

// acquire waits for a slot and returns the function that frees it. wait is
// how long to queue: above zero that long, below zero until a slot frees.
func (a *admission) acquire(ctx context.Context, wait time.Duration) (func(), error) {
	ticket := make(chan struct{}, 1)
	a.mu.Lock()
	a.queue = append(a.queue, ticket)
	a.mu.Unlock()

	var deadline <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		deadline = t.C
	}
	// The governor's reading changes without any session ending, so a
	// waiting head looks again now and then.
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		a.mu.Lock()
		permitted, _ := a.gov.Permitted(a.limit)
		if a.queue[0] == ticket && a.active < permitted {
			a.queue = a.queue[1:]
			a.active++
			a.wakeHead()
			a.mu.Unlock()
			var once sync.Once
			return func() { once.Do(a.release) }, nil
		}
		a.mu.Unlock()
		select {
		case <-ticket:
		case <-poll.C:
		case <-deadline:
			a.leave(ticket)
			return nil, a.busy()
		case <-ctx.Done():
			a.leave(ticket)
			return nil, domain.Errorf(domain.ErrCancelled, "waiting for a free session slot: %v", ctx.Err())
		}
	}
}

func (a *admission) release() {
	a.mu.Lock()
	a.active--
	a.wakeHead()
	a.mu.Unlock()
}

// leave takes a ticket out of the queue, letting the next one move up.
func (a *admission) leave(ticket chan struct{}) {
	a.mu.Lock()
	if i := slices.Index(a.queue, ticket); i >= 0 {
		a.queue = slices.Delete(a.queue, i, i+1)
	}
	a.wakeHead()
	a.mu.Unlock()
}

// wakeHead tells the session at the head of the queue to look again. The
// caller holds mu.
func (a *admission) wakeHead() {
	if len(a.queue) > 0 {
		select {
		case a.queue[0] <- struct{}{}:
		default:
		}
	}
}

// waiting reports how many sessions are queued and running.
func (a *admission) waiting() (queued, active int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue), a.active
}

func (a *admission) busy() error {
	permitted, load := a.gov.Permitted(a.limit)
	msg := fmt.Sprintf("all %d session slots are busy (execution.max_concurrent_sessions); try again later", a.limit)
	if permitted < a.limit {
		msg = fmt.Sprintf("the machine is busy (%.0f%% against a %.0f%% ceiling), so only %d of %d sessions may run; try again later",
			a.gov.Peak(load), a.gov.CeilingPercent, permitted, a.limit)
	}
	return &domain.Error{Code: domain.ErrProviderUnavailable, Retryable: true, Message: msg}
}
