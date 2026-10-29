package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestShutdownDrainsInFlightRequestsBeforeCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := shutdownOnCancel(ctx, server.Config, 2*time.Second)
	response := make(chan error, 1)
	go func() {
		r, err := server.Client().Get(server.URL)
		if err == nil {
			defer r.Body.Close()
			var b []byte
			b, err = io.ReadAll(r.Body)
			if err == nil && string(b) != "finished" {
				err = io.ErrUnexpectedEOF
			}
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case <-done:
		close(release)
		t.Fatal("shutdown finished with an in-flight handler")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal("drained response failed", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestShutdownDeadlineClosesStuckConnection(t *testing.T) {
	entered, disconnected := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(disconnected)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := shutdownOnCancel(ctx, server.Config, 30*time.Millisecond)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, err := server.Client().Get(server.URL)
		if err == nil {
			r.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadline did not bound drain")
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck connection not closed")
	}
	<-requestDone
}
