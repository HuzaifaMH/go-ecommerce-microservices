package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrMissingID is returned when publishing a message without an ID.
var ErrMissingID = errors.New("message ID is required")

// Connect opens a NATS connection and a JetStream context.
func Connect(url, name string, opts ...nats.Option) (*nats.Conn, jetstream.JetStream, error) {
	opts = append([]nats.Option{nats.Name(name), nats.MaxReconnects(-1)}, opts...)
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to nats: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("create jetstream context: %w", err)
	}
	return nc, js, nil
}

// StreamConfig describes a stream to create or update.
type StreamConfig struct {
	Name     string
	Subjects []string
	// MaxAge bounds how long messages are kept. Zero keeps them until limits are hit.
	MaxAge time.Duration
	// DuplicateWindow is how long published message IDs are remembered for
	// de-duplication. Defaults to 2 minutes.
	DuplicateWindow time.Duration
}

// EnsureStream creates the stream or updates it to match cfg.
func EnsureStream(ctx context.Context, js jetstream.JetStream, cfg StreamConfig) (jetstream.Stream, error) {
	if cfg.DuplicateWindow == 0 {
		cfg.DuplicateWindow = 2 * time.Minute
	}
	s, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       cfg.Name,
		Subjects:   cfg.Subjects,
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		MaxAge:     cfg.MaxAge,
		Duplicates: cfg.DuplicateWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure stream %s: %w", cfg.Name, err)
	}
	return s, nil
}

// JetStreamPublisher publishes to JetStream, using the message ID for broker-side de-duplication.
type JetStreamPublisher struct {
	js jetstream.JetStream
}

// NewJetStreamPublisher returns a Publisher backed by js.
func NewJetStreamPublisher(js jetstream.JetStream) *JetStreamPublisher {
	return &JetStreamPublisher{js: js}
}

// Publish sends m and waits for the broker's acknowledgement.
func (p *JetStreamPublisher) Publish(ctx context.Context, m Message) error {
	if m.ID == "" {
		return ErrMissingID
	}
	msg := nats.NewMsg(m.Subject)
	msg.Data = m.Data
	for k, v := range m.Headers {
		msg.Header.Set(k, v)
	}
	if _, err := p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(m.ID)); err != nil {
		return fmt.Errorf("publish %s: %w", m.Subject, err)
	}
	return nil
}

// ConsumerConfig describes a durable pull consumer.
type ConsumerConfig struct {
	Stream         string
	Durable        string
	FilterSubjects []string
	// MaxDeliver is the maximum number of delivery attempts. Defaults to 5.
	MaxDeliver int
	// AckWait is how long the broker waits for an ack before redelivering. Defaults to 30s.
	AckWait time.Duration
	// RetryDelay is the delay before a failed message is redelivered. Defaults to 2s.
	RetryDelay time.Duration
}

func (c *ConsumerConfig) setDefaults() {
	if c.MaxDeliver == 0 {
		c.MaxDeliver = 5
	}
	if c.AckWait == 0 {
		c.AckWait = 30 * time.Second
	}
	if c.RetryDelay == 0 {
		c.RetryDelay = 2 * time.Second
	}
}

// Consume delivers messages from the stream to h until ctx is cancelled.
//
// Outcomes: h returns nil -> Ack; h wraps ErrPermanent -> Term (never
// redelivered); any other error or a panic -> NakWithDelay (retried up to MaxDeliver).
func Consume(ctx context.Context, js jetstream.JetStream, cfg ConsumerConfig, log *slog.Logger, h Handler) error {
	cfg.setDefaults()

	cons, err := js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:        cfg.Durable,
		FilterSubjects: cfg.FilterSubjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		MaxDeliver:     cfg.MaxDeliver,
		AckWait:        cfg.AckWait,
	})
	if err != nil {
		return fmt.Errorf("create consumer %s: %w", cfg.Durable, err)
	}

	cc, err := cons.Consume(func(m jetstream.Msg) {
		dispatch(ctx, m, cfg, log, h)
	})
	if err != nil {
		return fmt.Errorf("start consumer %s: %w", cfg.Durable, err)
	}
	defer cc.Stop()

	<-ctx.Done()
	return nil
}

func dispatch(ctx context.Context, m jetstream.Msg, cfg ConsumerConfig, log *slog.Logger, h Handler) {
	msg := Message{
		ID:      m.Headers().Get(jetstream.MsgIDHeader),
		Subject: m.Subject(),
		Data:    m.Data(),
		Headers: flatten(m.Headers()),
	}
	log = log.With("consumer", cfg.Durable, "subject", msg.Subject, "message_id", msg.ID)
	if id := msg.Headers[HeaderCorrelationID]; id != "" {
		ctx = WithCorrelationID(ctx, id)
		log = log.With("correlation_id", id)
	}

	err := safeHandle(ctx, h, msg)
	switch {
	case err == nil:
		if err := m.Ack(); err != nil {
			log.Error("ack failed", "error", err)
		}
	case errors.Is(err, ErrPermanent):
		log.Error("terminating message", "error", err)
		if err := m.Term(); err != nil {
			log.Error("term failed", "error", err)
		}
	default:
		attempt := uint64(0)
		if md, mdErr := m.Metadata(); mdErr == nil {
			attempt = md.NumDelivered
		}
		log.Warn("handler failed, will retry", "error", err, "attempt", attempt, "max_deliver", cfg.MaxDeliver)
		if err := m.NakWithDelay(cfg.RetryDelay); err != nil {
			log.Error("nak failed", "error", err)
		}
	}
}

func safeHandle(ctx context.Context, h Handler, m Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, m)
}

func flatten(h nats.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
