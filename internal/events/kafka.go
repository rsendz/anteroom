package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/segmentio/kafka-go"
)

// bufferSize bounds how many events may be waiting to be written before new
// ones are dropped. It is generous enough to absorb an admission burst and
// small enough that a dead broker cannot grow memory without limit.
const bufferSize = 1024

// writer is the part of kafka.Writer this package needs, so tests can stand in
// for a broker.
type writer interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Kafka publishes events to a Kafka topic through a bounded buffer.
type Kafka struct {
	ch   chan Event
	w    writer
	log  *slog.Logger
	done chan struct{}
	// mu guards closed so that an Emit racing with Close cannot send on a
	// closed channel. Emit only ever takes the read side.
	mu     sync.RWMutex
	closed bool

	dropped atomic.Int64
	// lastDropLog throttles the drop warning so a long outage cannot itself
	// become the thing that floods the logs.
	lastDropLog atomic.Int64
}

// NewKafka starts a publisher for the given brokers and topic.
func NewKafka(brokers []string, topic string, log *slog.Logger) *Kafka {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireOne,
		AllowAutoTopicCreation: true,
		BatchTimeout:           100 * time.Millisecond,
		Async:                  true,
		Completion: func(_ []kafka.Message, err error) {
			if err != nil {
				log.Warn("anteroom: kafka delivery failed", "err", err)
			}
		},
	}
	return newKafka(w, log)
}

func newKafka(w writer, log *slog.Logger) *Kafka {
	k := &Kafka{
		ch:   make(chan Event, bufferSize),
		w:    w,
		log:  log,
		done: make(chan struct{}),
	}
	go k.run()
	return k
}

// Emit queues an event, dropping it if the buffer is full. It never blocks.
func (k *Kafka) Emit(e Event) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.closed {
		return
	}
	select {
	case k.ch <- e:
	default:
		k.noteDrop()
	}
}

func (k *Kafka) noteDrop() {
	total := k.dropped.Add(1)
	now := time.Now().Unix()
	if last := k.lastDropLog.Load(); now-last >= 10 && k.lastDropLog.CompareAndSwap(last, now) {
		k.log.Warn("anteroom: dropping events, kafka is not keeping up", "dropped_total", total)
	}
}

// Dropped reports how many events have been discarded.
func (k *Kafka) Dropped() int64 { return k.dropped.Load() }

func (k *Kafka) run() {
	defer close(k.done)
	for e := range k.ch {
		msg, err := encode(e)
		if err != nil {
			k.log.Warn("anteroom: encoding event failed", "type", e.Type, "err", err)
			continue
		}
		// The writer is async, so this returns as soon as the message is
		// batched; delivery errors surface through the Completion callback.
		if err := k.w.WriteMessages(context.Background(), msg); err != nil {
			k.log.Warn("anteroom: queueing event to kafka failed", "type", e.Type, "err", err)
		}
	}
}

func encode(e Event) (kafka.Message, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return kafka.Message{}, err
	}
	// Keying by room and visitor keeps one visitor's events in order.
	return kafka.Message{Key: []byte(e.Room + ":" + e.VisitorID), Value: body}, nil
}

// Close drains the buffer and shuts the writer down. It is safe to call twice.
func (k *Kafka) Close() error {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil
	}
	k.closed = true
	close(k.ch)
	k.mu.Unlock()

	<-k.done
	if n := k.dropped.Load(); n > 0 {
		k.log.Warn("anteroom: events dropped during this run", "total", n)
	}
	return k.w.Close()
}
