package meter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeSink records each batch it receives.
type fakeSink struct {
	mu      sync.Mutex
	batches [][]Event
}

func (f *fakeSink) Write(_ context.Context, events []Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]Event, len(events))
	copy(cp, events)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeSink) snapshot() [][]Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]Event, len(f.batches))
	copy(out, f.batches)
	return out
}

func (f *fakeSink) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// Flush when the batch reaches its size threshold, before the timer fires.
func TestFlushBySize(t *testing.T) {
	sink := &fakeSink{}
	w := New(sink, 3, time.Hour) // long interval so only size triggers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	for i := 0; i < 3; i++ {
		if !w.Enqueue(Event{TenantID: int64(i)}) {
			t.Fatal("enqueue dropped")
		}
	}
	waitFor(t, func() bool { return sink.total() == 3 })

	batches := sink.snapshot()
	if len(batches) != 1 || len(batches[0]) != 3 {
		t.Fatalf("expected one batch of 3, got %v", batches)
	}
}

// Flush a partial batch when the timer fires.
func TestFlushByTimer(t *testing.T) {
	sink := &fakeSink{}
	w := New(sink, 100, 20*time.Millisecond) // size won't trigger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	w.Enqueue(Event{TenantID: 1})
	w.Enqueue(Event{TenantID: 2})
	waitFor(t, func() bool { return sink.total() == 2 })
}

// Graceful shutdown drains and flushes buffered events.
func TestShutdownFlush(t *testing.T) {
	sink := &fakeSink{}
	w := New(sink, 100, time.Hour) // neither size nor timer will fire
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	for i := 0; i < 5; i++ {
		w.Enqueue(Event{TenantID: int64(i)})
	}
	cancel()  // trigger shutdown
	w.Wait()  // block until final flush completes

	if sink.total() != 5 {
		t.Fatalf("expected 5 events flushed on shutdown, got %d", sink.total())
	}
}

// Enqueue is non-blocking and drops when the buffer is full.
func TestEnqueueDropsWhenFull(t *testing.T) {
	sink := &fakeSink{}
	w := New(sink, 100, time.Hour) // no Run; channel never drained
	dropped := false
	for i := 0; i < bufferSize+10; i++ {
		if !w.Enqueue(Event{TenantID: int64(i)}) {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Fatal("expected a drop once buffer filled")
	}
}
