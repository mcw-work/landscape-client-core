package snapd

import (
	"context"
	"time"
)

// NotifyWhenRetryWaitStarts reports entry to the retry wait for client.
func NotifyWhenRetryWaitStarts(client Client, entered chan<- struct{}) {
	realClient := client.(*RealClient)
	retryWait := realClient.retryWait
	realClient.retryWait = func(ctx context.Context, timer *time.Timer) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		return retryWait(ctx, timer)
	}
}
