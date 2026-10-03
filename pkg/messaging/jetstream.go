package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/retry"
)

// ErrMissingID is returned when publishing a message without an ID.
var ErrMissingID = errors.New("message ID is required")

// startupWait is how long Connect keeps trying while the broker is not up yet.
const startupWait = 30 * time.Second

// Connect opens a NATS connection and a JetStream context. If the broker is
// not reachable yet it keeps trying for up to 30 seconds, so the order in which
// containers start does not matter.
func Connect(url, name string, opts ...nats.Option) (*nats.Conn, jetstream.JetStream, error) {
	opts = append([]nats.Option{nats.Name(name), nats.MaxReconnects(-1)}, opts...)

	var nc *nats.Conn
	err := retry.Do(context.Background(), startupWait, func(context.Context) error {
		var err error
		nc, err = nats.Connect(url, opts...)
		return err
	})
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
	js       jetstream.JetStream
	observer Observer
}

// PublisherOption configures a JetStreamPublisher.
type PublisherOption func(*JetStreamPublisher)

// WithObserver reports every publish attempt to o (for metrics).
func WithObserver(o Observer) PublisherOption {
	return func(p *JetStreamPublisher) { p.observer = orNoop(o) }
}

// NewJetStreamPublisher returns a Publisher backed by js.
func NewJetStreamPublisher(js jetstream.JetStream, opts ...PublisherOption) *JetStreamPublisher {
	p := &JetStreamPublisher{js: js, observer: noopObserver{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Publish sends m and waits for the broker's acknowledgement. It records a
// producer span that continues the trace stored in m.Headers and hands its own
// context on to consumers.
func (p *JetStreamPublisher) Publish(ctx context.Context, m Message) error {
	if m.ID == "" {
		p.observer.Published(m.Subject, false, 0)
		return ErrMissingID
	}
	start := time.Now()
	ctx, span, headers := startPublishSpan(ctx, m)
	defer span.End()

	msg := nats.NewMsg(m.Subject)
	msg.Data = m.Data
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	if _, err := p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(m.ID)); err != nil {
		markFailed(span, err)
		p.observer.Published(m.Subject, false, time.Since(start))
		return fmt.Errorf("publish %s: %w", m.Subject, err)
	}
	p.observer.Published(m.Subject, true, time.Since(start))
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
	// Observer, if set, is told about every delivery (for metrics).
	Observer Observer
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

	// The span continues the trace of whoever published the message, and the
	// handler runs inside it, so its own spans and log lines belong to that trace.
	ctx, span := startConsumeSpan(ctx, cfg.Durable, msg)
	defer span.End()
	start := time.Now()

	err := safeHandle(ctx, h, msg)
	outcome := OutcomeAck
	switch {
	case err == nil:
		if err := m.Ack(); err != nil {
			log.ErrorContext(ctx, "ack failed", "error", err)
		}
	case errors.Is(err, ErrPermanent):
		outcome = OutcomeTerminated
		markFailed(span, err)
		log.ErrorContext(ctx, "terminating message", "error", err)
		if err := m.Term(); err != nil {
			log.ErrorContext(ctx, "term failed", "error", err)
		}
	default:
		outcome = OutcomeRetry
		markFailed(span, err)
		attempt := uint64(0)
		if md, mdErr := m.Metadata(); mdErr == nil {
			attempt = md.NumDelivered
		}
		log.WarnContext(ctx, "handler failed, will retry", "error", err, "attempt", attempt, "max_deliver", cfg.MaxDeliver)
		if err := m.NakWithDelay(cfg.RetryDelay); err != nil {
			log.ErrorContext(ctx, "nak failed", "error", err)
		}
	}
	orNoop(cfg.Observer).Consumed(cfg.Durable, msg.Subject, outcome, time.Since(start))
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
