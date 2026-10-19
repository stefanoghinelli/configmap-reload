package main

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	triggerReload(&http.Client{Timeout: 25 * time.Millisecond}, webhookURL, "0", http.MethodPost, http.StatusOK, 1, 2*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("reload took %s; timeout or final retry sleep was not respected", elapsed)
	}
}

func TestParseWebhooksRejectsInvalidURLs(t *testing.T) {
	var configured webhookFlag
	if err := configured.Set("http://example.com/reload?token=secret"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configured.String(), "secret") {
		t.Fatal("flag description exposed the webhook URL")
	}

	for _, value := range []string{
		"/reload",
		"ftp://example.com/reload",
		"http://",
		"http://example.com/reload#fragment",
		"http://[invalid?token=secret",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := parseWebhooks(webhookFlag{value})
			if err == nil {
				t.Fatal("expected invalid webhook URL to be rejected")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("validation error exposed the webhook URL")
			}
		})
	}

	urls, err := parseWebhooks(webhookFlag{"https://user:password@example.com/reload?token=secret"})
	if err != nil || len(urls) != 1 {
		t.Fatalf("valid webhook URL rejected: urls=%v, err=%v", urls, err)
	}
}

func TestValidateRequestOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		status  int
		retries int
		timeout time.Duration
	}{
		{"empty method", "", 200, 1, time.Second},
		{"invalid method", "GET SPACE", 200, 1, time.Second},
		{"invalid status", "POST", 600, 1, time.Second},
		{"zero retries", "POST", 200, 0, time.Second},
		{"negative retries", "POST", 200, -1, time.Second},
		{"zero timeout", "POST", 200, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRequestOptions(tc.method, tc.status, tc.retries, tc.timeout); err == nil {
				t.Fatal("expected invalid option to be rejected")
			}
		})
	}
	if err := validateRequestOptions("POST", 204, 2, time.Second); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWebhookSecretsStayOutOfLogsAndMetrics(t *testing.T) {
	const secret = "p1-private-webhook-token"
	webhookURL, err := url.Parse("http://user:password@example.invalid/reload?token=" + secret)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failure with " + secret)
	})}
	triggerReload(client, webhookURL, "test-webhook", http.MethodPost, http.StatusOK, 1, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	successURL, err := url.Parse(server.URL + "/reload?token=" + secret)
	if err != nil {
		t.Fatal(err)
	}
	successURL.User = url.UserPassword("user", "password")
	triggerReload(server.Client(), successURL, "test-success", http.MethodPost, http.StatusNoContent, 1, 0)

	metrics := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, output := range []string{logs.String(), metrics.Body.String()} {
		for _, credential := range []string{"user:password", "user@", secret} {
			if strings.Contains(output, credential) {
				t.Fatalf("webhook credential %q exposed in output", credential)
			}
		}
	}
}
