// Package natsbus connects to NATS JetStream, the durable event backbone.
package natsbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bus is a JetStream publisher bound to one stream.
type Bus struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// StreamConfig describes the event stream the relay publishes into.
type StreamConfig struct {
	Name          string        // e.g. SYSTERA_EVENTS
	SubjectPrefix string        // e.g. systera.events (the stream captures prefix.>)
	MaxAge        time.Duration // retention; default 7 days
	DedupWindow   time.Duration // Nats-Msg-Id de-duplication window; default 2 minutes
}

// Connect dials NATS (reconnecting forever) and creates or updates the event stream.
func Connect(ctx context.Context, url, clientName string, sc StreamConfig, logger *slog.Logger) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.Name(clientName),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) { logger.Info("nats reconnected", "url", c.ConnectedUrl()) }),
	)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if sc.MaxAge == 0 {
		sc.MaxAge = 7 * 24 * time.Hour
	}
	if sc.DedupWindow == 0 {
		sc.DedupWindow = 2 * time.Minute
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       sc.Name,
		Subjects:   []string{sc.SubjectPrefix + ".>"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     sc.MaxAge,
		Duplicates: sc.DedupWindow,
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("ensure stream %s: %w", sc.Name, err)
	}
	return &Bus{nc: nc, js: js}, nil
}

// Publish sends data and waits for the JetStream ack. msgID enables de-duplication.
func (b *Bus) Publish(ctx context.Context, subject, msgID string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := b.js.Publish(ctx, subject, data, jetstream.WithMsgID(msgID))
	return err
}

// JetStream exposes the underlying client (consumers, tests).
func (b *Bus) JetStream() jetstream.JetStream { return b.js }

// Healthy reports whether the connection is up.
func (b *Bus) Healthy(context.Context) error {
	if !b.nc.IsConnected() {
		return errors.New("nats not connected")
	}
	return nil
}

func (b *Bus) Close() {
	if b != nil && b.nc != nil {
		_ = b.nc.Drain()
	}
}
