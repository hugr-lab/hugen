package models

import (
	"context"
	"errors"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/hugr-lab/query-engine/types"
	"golang.org/x/sync/errgroup"
)

// BatchHandler processes a single Arrow RecordBatch for a given subscription path.
type BatchHandler func(ctx context.Context, batch arrow.RecordBatch) error

var ErrStopReading = errors.New("stop reading subscription")

// ReadSubscription reads all events from a Hugr subscription, dispatching
// each RecordBatch to the handler registered for the event's Path.
//
// Each event's RecordReader is consumed in its own goroutine (via errgroup).
// The context controls cancellation — if ctx is cancelled, reading stops
// and the subscription is not left hanging.
//
// Returns an error if:
//   - no handler is registered for an event path
//   - any handler returns an error
//   - any RecordReader reports an error
//   - the subscription itself errors
//   - the context is cancelled
func ReadSubscription(ctx context.Context, sub *types.Subscription, handlers map[string]BatchHandler) error {
	eg, ctx := errgroup.WithContext(ctx)

	for event := range sub.Events {
		handler, ok := handlers[event.Path]
		if !ok {
			event.Reader.Release()
			return fmt.Errorf("no handler for subscription path %q", event.Path)
		}

		reader := event.Reader
		h := handler
		eg.Go(func() error {
			defer reader.Release()
			for reader.Next() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					// continue processing
				}

				if err := h(ctx, reader.RecordBatch()); err != nil {
					return err
				}
			}
			return reader.Err()
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}
	// sub.Err() is safe here: sub.Events is closed, readLoop has finished.
	return sub.Err()
}
