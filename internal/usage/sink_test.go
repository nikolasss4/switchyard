package usage

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nikolasss4/switchyard/internal/store"
)

func TestWriterBatchesAndFlushesOnClose(t *testing.T) {
	var mu sync.Mutex
	var batches []int
	w := NewWriter(func(_ context.Context, r []store.UsageRecord) error {
		mu.Lock()
		batches = append(batches, len(r))
		mu.Unlock()
		return nil
	}, slog.New(slog.DiscardHandler), 100, 10, time.Hour)
	for range 25 {
		w.Record(store.UsageRecord{})
	}
	w.Close()
	if len(batches) != 3 || batches[0] != 10 || batches[2] != 5 {
		t.Fatalf("batches = %v", batches)
	}
}

func TestWriterDropsWhenFull(t *testing.T) {
	block := make(chan struct{})
	w := NewWriter(func(context.Context, []store.UsageRecord) error { <-block; return nil }, slog.New(slog.DiscardHandler), 2, 1, time.Hour)
	for range 10 {
		w.Record(store.UsageRecord{})
	}
	if w.Dropped() < 7 {
		t.Fatalf("dropped %d", w.Dropped())
	}
	close(block)
	w.Close()
}
