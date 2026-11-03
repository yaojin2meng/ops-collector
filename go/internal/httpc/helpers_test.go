package httpc

import (
	"context"
	"time"
)

func testCancelAfter(d time.Duration) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(d)
		cancel()
	}()
	return ctx
}
