package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// blockingWriter stalls until released, so tests can fill the buffer.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	got     []kafka.Message
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{release: make(chan struct{})}
}

func (w *blockingWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, msgs...)
	return nil
}

func (w *blockingWriter) Close() error { return nil }

func (w *blockingWriter) messages() []kafka.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]kafka.Message(nil), w.got...)
}

// collectWriter records everything written without blocking.
type collectWriter struct {
	mu     sync.Mutex
	got    []kafka.Message
	err    error
	closed bool
}

func (w *collectWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, msgs...)
	return w.err
}

func (w *collectWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *collectWriter) messages() []kafka.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]kafka.Message(nil), w.got...)
}

func TestEmitPublishesJSON(t *testing.T) {
	w := &collectWriter{}
	k := newKafka(w, discardLogger())

	k.Emit(New(TypeVisitorAdmitted, "shop", "v1", map[string]any{"position": 3}))
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}

	msgs := w.messages()
	if len(msgs) != 1 {
		t.Fatalf("wrote %d messages, want 1", len(msgs))
	}
	if got := string(msgs[0].Key); got != "shop:v1" {
		t.Errorf("key = %q, want shop:v1", got)
	}
	var e Event
	if err := json.Unmarshal(msgs[0].Value, &e); err != nil {
		t.Fatalf("value is not valid JSON: %v", err)
	}
	if e.Type != TypeVisitorAdmitted || e.Room != "shop" || e.VisitorID != "v1" {
		t.Errorf("decoded %+v", e)
	}
	if e.Meta["position"] != float64(3) {
		t.Errorf("meta = %v", e.Meta)
	}
	if e.TS.IsZero() {
		t.Error("timestamp not stamped")
	}
	if !w.closed {
		t.Error("Close did not close the underlying writer")
	}
}

func TestEmitNeverBlocksAndDropsOnOverflow(t *testing.T) {
	w := newBlockingWriter()
	k := newKafka(w, discardLogger())
	t.Cleanup(func() { close(w.release); k.Close() })

	// Far more events than the buffer can hold, with the writer wedged. If
	// Emit blocked, this would deadlock rather than fail.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range bufferSize * 3 {
			k.Emit(New(TypeVisitorJoined, "shop", "v", nil))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked when the buffer was full")
	}
	if k.Dropped() == 0 {
		t.Error("no events were counted as dropped despite overflowing the buffer")
	}
}

func TestEmitSurvivesWriterErrors(t *testing.T) {
	// A broker that rejects everything must not stop anteroom emitting.
	w := &collectWriter{err: errors.New("broker unavailable")}
	k := newKafka(w, discardLogger())
	for range 10 {
		k.Emit(New(TypeVisitorJoined, "shop", "v", nil))
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(w.messages()); got != 10 {
		t.Errorf("attempted %d writes, want 10", got)
	}
}

func TestCloseDrainsBuffer(t *testing.T) {
	w := &collectWriter{}
	k := newKafka(w, discardLogger())
	for i := range 50 {
		k.Emit(New(TypeVisitorJoined, "shop", string(rune('a'+i%26)), nil))
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(w.messages()); got != 50 {
		t.Errorf("drained %d events, want all 50", got)
	}
}

func TestCloseIsIdempotentAndEmitAfterCloseIsSafe(t *testing.T) {
	k := newKafka(&collectWriter{}, discardLogger())
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// A late event from an in-flight request must not panic on a closed channel.
	k.Emit(New(TypeVisitorJoined, "shop", "v", nil))
}

func TestConcurrentEmitAndClose(t *testing.T) {
	// Run under -race: this is the shutdown window where a naive
	// implementation sends on a closed channel.
	k := newKafka(&collectWriter{}, discardLogger())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				k.Emit(New(TypeVisitorJoined, "shop", "v", nil))
			}
		}()
	}
	time.Sleep(time.Millisecond)
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestNopEmitter(t *testing.T) {
	var e Emitter = Nop{}
	e.Emit(New(TypeVisitorJoined, "shop", "v", nil))
	if err := e.Close(); err != nil {
		t.Errorf("Nop.Close: %v", err)
	}
}
