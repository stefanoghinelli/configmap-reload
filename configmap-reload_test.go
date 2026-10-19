package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestWatchEventsDoesNotBlockOnPendingReload(t *testing.T) {
	events := make(chan fsnotify.Event, 10)
	errors := make(chan error)
	updates := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		watchEvents(events, errors, updates)
		close(done)
	}()

	for i := 0; i < cap(events); i++ {
		events <- fsnotify.Event{Name: "/config/..data", Op: fsnotify.Create}
	}
	close(events)
	close(errors)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher blocked while a reload was pending")
	}
	if len(updates) != 1 {
		t.Fatalf("got %d pending reloads, want 1", len(updates))
	}
}

func TestWebhookTimeoutDoesNotWaitAfterLastAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	webhookURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	triggerReload(&http.Client{Timeout: 25 * time.Millisecond}, webhookURL, http.MethodPost, http.StatusOK, 1, 2*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("reload took %s; timeout or final retry sleep was not respected", elapsed)
	}
}
