package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Queue is a durable first-in, first-out buffer of encoded reports.
type Queue interface {
	Push(ctx context.Context, item []byte) error
	// Claim waits up to wait for the oldest item and marks it in flight. It returns nil when none arrived.
	Claim(ctx context.Context, wait time.Duration) ([]byte, error)
	// Ack forgets a claimed item.
	Ack(ctx context.Context, item []byte) error
	// Release puts a claimed item back at the front of the queue.
	Release(ctx context.Context, item []byte) error
	// Recover returns the items a previous run left in flight to the front of the queue.
	Recover(ctx context.Context) error
}

// Sender delivers a report to the backend.
type Sender interface {
	Send(ctx context.Context, report Report) error
}

// Outbox buffers reports so a backend restart or outage loses none, and delivers them in order.
type Outbox struct {
	queue      Queue
	sender     Sender
	log        *slog.Logger
	claimWait  time.Duration
	retryDelay time.Duration
}

// NewOutbox builds an outbox over a queue. Call Run to start delivering.
func NewOutbox(queue Queue, sender Sender, log *slog.Logger) *Outbox {
	return &Outbox{queue: queue, sender: sender, log: log, claimWait: 5 * time.Second, retryDelay: 2 * time.Second}
}

// Enqueue stores a report for delivery. It fails only if the report could not be stored.
func (o *Outbox) Enqueue(ctx context.Context, report Report) error {
	item, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	if err := o.queue.Push(ctx, item); err != nil {
		return fmt.Errorf("buffering report: %w", err)
	}
	return nil
}

// Run delivers buffered reports one at a time until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context) {
	if err := o.queue.Recover(ctx); err != nil {
		o.log.Error("recovering in-flight reports", "error", err)
	}
	for ctx.Err() == nil {
		if !o.deliverNext(ctx) {
			o.pause(ctx)
		}
	}
}

// deliverNext handles the oldest report and returns false when the caller should wait before trying again.
func (o *Outbox) deliverNext(ctx context.Context) bool {
	item, err := o.queue.Claim(ctx, o.claimWait)
	if err != nil {
		if ctx.Err() == nil {
			o.log.Error("reading the report queue", "error", err)
		}
		return false
	}
	if item == nil {
		return true
	}
	var report Report
	if err := json.Unmarshal(item, &report); err != nil {
		o.log.Error("dropping an unreadable report", "error", err)
		return o.settle(ctx, o.queue.Ack, item)
	}
	switch err := o.sender.Send(ctx, report); {
	case err == nil:
		return o.settle(ctx, o.queue.Ack, item)
	case errors.Is(err, ErrRejected):
		o.log.Warn("dropping a report the backend refused", "path", report.Path, "error", err)
		return o.settle(ctx, o.queue.Ack, item)
	default:
		o.log.Warn("report not delivered, will retry", "path", report.Path, "error", err)
		// Released with a fresh context so a shutdown mid-delivery still returns the item to the queue
		o.settle(context.WithoutCancel(ctx), o.queue.Release, item)
		return false
	}
}

func (o *Outbox) settle(ctx context.Context, action func(context.Context, []byte) error, item []byte) bool {
	if err := action(ctx, item); err != nil {
		o.log.Error("updating the report queue", "error", err)
		return false
	}
	return true
}

func (o *Outbox) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(o.retryDelay):
	}
}
