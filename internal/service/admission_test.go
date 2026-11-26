package service

import (
	"context"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/governor"
)

func TestAdmissionIsFirstComeFirstServed(t *testing.T) {
	a := newAdmission(1, governor.New(0, 0))
	ctx := context.Background()
	release, err := a.acquire(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 3)
	for _, name := range []string{"first", "second", "third"} {
		go func() {
			r, err := a.acquire(ctx, -1)
			if err != nil {
				order <- "error: " + err.Error()
				return
			}
			order <- name
			time.Sleep(20 * time.Millisecond)
			r()
		}()
		// Each waiter joins the queue before the next one asks.
		for {
			if q, _ := a.waiting(); q > 0 && q == map[string]int{"first": 1, "second": 2, "third": 3}[name] {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	release()
	for _, want := range []string{"first", "second", "third"} {
		if got := <-order; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
}

func TestAdmissionWaitsUntilFreeOrTimesOut(t *testing.T) {
	a := newAdmission(1, governor.New(0, 0))
	ctx := context.Background()
	release, _ := a.acquire(ctx, time.Second)

	_, err := a.acquire(ctx, 50*time.Millisecond)
	var de *domain.Error
	if de, _ = err.(*domain.Error); de == nil || de.Code != domain.ErrProviderUnavailable || !de.Retryable {
		t.Fatalf("timed-out wait: %v", err)
	}
	if q, _ := a.waiting(); q != 0 {
		t.Fatalf("a timed-out waiter stayed in the queue (%d)", q)
	}

	got := make(chan error, 1)
	go func() {
		r, err := a.acquire(ctx, -1)
		if err == nil {
			r()
		}
		got <- err
	}()
	time.Sleep(150 * time.Millisecond) // longer than the timed-out wait above
	release()
	if err := <-got; err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	hold, _ := a.acquire(ctx, time.Second)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := a.acquire(cctx, -1); domain.CodeOf(err) != domain.ErrCancelled {
		t.Fatalf("cancelled wait: %v", err)
	}
	hold()
	if _, active := a.waiting(); active != 0 {
		t.Fatalf("%d slots still held", active)
	}
}
