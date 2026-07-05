// Package meter is the async metering path: request events are buffered on a
// channel and flushed to a Sink in batches (N rows or T ms, whichever first).
// This keeps Postgres writes OUT of the request hot path (DESIGN.md). The buffer
// is lossy on kill -9 by design (≤1 batch); graceful shutdown flushes.
package meter

import (
	"context"
	"log"
	"time"
)

// Event is a single metering row.
type Event struct {
	TenantID     int64
	Feature      string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	Estimated    bool
	Status       int
	LatencyMS    int
	CreatedAt    time.Time
}

// Sink persists a batch of events. Postgres is the real one; tests use a fake.
type Sink interface {
	Write(ctx context.Context, events []Event) error
}

// Writer batches events onto a Sink.
type Writer struct {
	ch        chan Event
	sink      Sink
	batchSize int
	interval  time.Duration
	done      chan struct{}
}

// Defaults per DESIGN.md: flush at 100 rows or 500ms.
const (
	DefaultBatchSize = 100
	DefaultInterval  = 500 * time.Millisecond
	bufferSize       = 4096
)

// New constructs a Writer. batchSize<=0 and interval<=0 fall back to defaults.
func New(sink Sink, batchSize int, interval time.Duration) *Writer {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Writer{
		ch:        make(chan Event, bufferSize),
		sink:      sink,
		batchSize: batchSize,
		interval:  interval,
		done:      make(chan struct{}),
	}
}

// Enqueue submits an event without blocking the caller. If the buffer is full
// the event is dropped (metering, not billing — documented tradeoff).
// ponytail: drop-on-full keeps the hot path non-blocking; add a dropped counter
// if we ever need to observe loss.
func (w *Writer) Enqueue(e Event) bool {
	select {
	case w.ch <- e:
		return true
	default:
		return false
	}
}

// Run consumes events until ctx is cancelled, then drains and flushes. Call once
// in its own goroutine. Closes w.done when fully flushed.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	batch := make([]Event, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Detached context: shutdown must still flush even after ctx is done.
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.sink.Write(fctx, batch); err != nil {
			log.Printf("meter: flush of %d events failed: %v", len(batch), err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-w.ch:
			batch = append(batch, e)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			// Drain whatever is buffered, then final flush.
			for {
				select {
				case e := <-w.ch:
					batch = append(batch, e)
					if len(batch) >= w.batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

// Wait blocks until Run has finished its shutdown flush.
func (w *Writer) Wait() { <-w.done }
