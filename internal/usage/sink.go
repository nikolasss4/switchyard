// Package usage writes usage records to Postgres off the request path.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nikolasss4/switchyard/internal/store"
)

// Sink accepts usage records.
type Sink interface {
	Record(r store.UsageRecord)
}

// Writer batches records into COPYs. Record never blocks: when the buffer is
// full the record is dropped and counted, so a slow database cannot stall
// requests. Budgets do not depend on these rows (they use the spend counter).
type Writer struct {
	ch      chan store.UsageRecord
	insert  func(context.Context, []store.UsageRecord) error
	log     *slog.Logger
	dropped atomic.Int64
	OnDrop  func()
	wg      sync.WaitGroup
}

// NewWriter starts a writer that flushes every interval or batch records.
func NewWriter(insert func(context.Context, []store.UsageRecord) error, log *slog.Logger, buffer, batch int, interval time.Duration) *Writer {
	w := &Writer{ch: make(chan store.UsageRecord, buffer), insert: insert, log: log}
	w.wg.Add(1)
	go w.run(batch, interval)
	return w
}

// Record queues a record.
func (w *Writer) Record(r store.UsageRecord) {
	select {
	case w.ch <- r:
	default:
		w.dropped.Add(1)
		if w.OnDrop != nil {
			w.OnDrop()
		}
	}
}

// Dropped is the number of records dropped because the buffer was full.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Close flushes what is queued and stops the writer.
func (w *Writer) Close() {
	close(w.ch)
	w.wg.Wait()
}

func (w *Writer) run(batch int, interval time.Duration) {
	defer w.wg.Done()
	buf := make([]store.UsageRecord, 0, batch)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := w.insert(ctx, buf); err != nil {
			w.log.Error("usage write failed", "records", len(buf), "err", err)
		}
		cancel()
		buf = buf[:0]
	}
	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, r)
			if len(buf) >= batch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// Discard drops every record.
type Discard struct{}

func (Discard) Record(store.UsageRecord) {}

// Memory keeps records in memory, for tests.
type Memory struct {
	mu   sync.Mutex
	recs []store.UsageRecord
}

func (m *Memory) Record(r store.UsageRecord) {
	m.mu.Lock()
	m.recs = append(m.recs, r)
	m.mu.Unlock()
}

// Records returns a copy of what was recorded.
func (m *Memory) Records() []store.UsageRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.UsageRecord(nil), m.recs...)
}
