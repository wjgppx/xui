package nodeagent

import (
	"context"
	"errors"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

type UsageOutbox interface {
	Pending(limit int) ([]agentwire.UsageBatch, error)
	Ack(batchID, digest string) error
}

func (c *Client) bindUsageIdentity() error {
	if binder, ok := c.Outbox.(interface{ BindIdentity(string) error }); ok {
		if c.State.NodeID == "" {
			return errors.New("usage delivery requires an enrolled identity")
		}
		if err := binder.BindIdentity(c.State.Controller + "\x00" + c.State.NodeID); err != nil {
			return errors.New("usage ledger belongs to a different controller or node")
		}
	}
	return nil
}

// FlushUsage only deletes local evidence after the matching durable remote ack.
// A bounded flush prevents a large backlog from indefinitely blocking shutdown.
func (c *Client) FlushUsage(ctx context.Context) error {
	if c.Outbox == nil {
		return nil
	}
	if err := c.bindUsageIdentity(); err != nil {
		return err
	}
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	batches, err := c.Outbox.Pending(32)
	if err != nil {
		return errors.New("usage outbox read failed")
	}
	for _, batch := range batches {
		var ack agentwire.UsageAck
		if err = c.request(flushCtx, "POST", "/agent/v1/usage-batches", batch, &ack); err != nil {
			return err
		}
		if ack.BatchID != batch.BatchID {
			return errors.New("usage acknowledgement identifies another batch")
		}
		if err = c.Outbox.Ack(batch.BatchID, ack.Digest); err != nil {
			return errors.New("usage acknowledgement did not match durable batch")
		}
	}
	remaining, err := c.Outbox.Pending(1)
	if err != nil {
		return errors.New("usage outbox read failed")
	}
	if len(remaining) > 0 {
		return errors.New("usage backlog remains; defer authorization renewal")
	}
	return nil
}
