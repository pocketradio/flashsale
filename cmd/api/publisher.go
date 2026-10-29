package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rabbitmq/amqp091-go"
)

type job struct {
	TrackingID     string    `json:"tracking_id"`
	UserID         string    `json:"user_id"`
	ProductID      string    `json:"product_id"`
	Quantity       int       `json:"quantity"`
	IdempotencyKey string    `json:"idempotency_key"`
	Fingerprint    string    `json:"request_fingerprint"`
	CreatedAt      time.Time `json:"created_at"`
}

type publisher struct {
	conn     *amqp091.Connection
	ch       *amqp091.Channel
	confirms <-chan amqp091.Confirmation
	mu       sync.Mutex
}

func newPublisher(url string) (*publisher, error) {
	conn, err := amqp091.Dial(url)
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = ch.ExchangeDeclare("checkout.dlx", "direct", true, false, false, false, nil); err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}
	if _, err = ch.QueueDeclare("checkout.dead", true, false, false, false, amqp091.Table{"x-queue-type": "quorum"}); err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}
	if _, err = ch.QueueDeclare("checkout.process", true, false, false, false, amqp091.Table{"x-queue-type": "quorum", "x-dead-letter-exchange": "checkout.dlx"}); err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}
	if err = ch.Confirm(false); err != nil {
		ch.Close()
		conn.Close()
		return nil, err
	}
	return &publisher{conn: conn, ch: ch, confirms: ch.NotifyPublish(make(chan amqp091.Confirmation, 1024))}, nil
}

func (p *publisher) publish(ctx context.Context, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ch.PublishWithContext(ctx, "", "checkout.process", false, false, amqp091.Publishing{DeliveryMode: amqp091.Persistent, ContentType: "application/json", Body: body}); err != nil {
		return err
	}
	select {
	case c, ok := <-p.confirms:
		if !ok || !c.Ack {
			return errors.New("rabbitmq publish was not confirmed")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *publisher) close() {
	if p.ch != nil {
		p.ch.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
}

func deterministicID(seed string) string {
	var raw [16]byte
	h := fmt.Sprintf("%x", sha256Bytes([]byte(seed)))
	b, _ := hex.DecodeString(h[:32])
	copy(raw[:], b)
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func sha256Bytes(v []byte) []byte { h := sha256.Sum256(v); return h[:] }
