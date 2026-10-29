package main

import (
	"context"
	"net/http"
	"time"
)

func shutdownOnCancel(ctx context.Context, server *http.Server, budget time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		drain, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		if server.Shutdown(drain) != nil {
			_ = server.Close()
		}
	}()
	return done
}
