package snapd

import (
	"context"
	"net/http"
	"time"
)

// ClientTimeouts returns the response-header and overall request timeouts of client.
func ClientTimeouts(client Client) (header, overall time.Duration) {
	realClient := client.(*RealClient)
	return realClient.http.Transport.(*http.Transport).ResponseHeaderTimeout, realClient.http.Timeout
}

// SetResponseHeaderTimeout shortens the response-header timeout so stall tests run fast.
func SetResponseHeaderTimeout(client Client, d time.Duration) {
	client.(*RealClient).http.Transport.(*http.Transport).ResponseHeaderTimeout = d
}

// ShortenRetryDelay makes WaitForChange re-poll after 1ms instead of changeRetryDelay.
func ShortenRetryDelay(client Client) {
	realClient := client.(*RealClient)
	retryWait := realClient.retryWait
	realClient.retryWait = func(ctx context.Context, timer *time.Timer) error {
		timer.Reset(time.Millisecond)
		return retryWait(ctx, timer)
	}
}

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
