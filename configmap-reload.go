package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	fsnotify "github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "configmap_reload"

var (
	volumeDirs        volumeDirsFlag
	webhook           webhookFlag
	webhookMethod     = flag.String("webhook-method", "POST", "the HTTP method url to use to send the webhook")
	webhookStatusCode = flag.Int("webhook-status-code", 200, "the HTTP status code indicating successful triggering of reload")
	webhookRetries    = flag.Int("webhook-retries", 1, "the amount of times to retry the webhook reload request")
	webhookTimeout    = flag.Duration("webhook-timeout", 10*time.Second, "maximum duration of each webhook request")
	listenAddress     = flag.String("web.listen-address", ":9533", "Address to listen on for web interface and telemetry.")
	metricPath        = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	lastReloadError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_reload_error",
		Help:      "Whether the last reload resulted in an error (1 for error, 0 for success)",
	}, []string{"webhook"})
	requestDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_request_duration_seconds",
		Help:      "Duration of last webhook request",
	}, []string{"webhook"})
	successReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "success_reloads_total",
		Help:      "Total success reload calls",
	}, []string{"webhook"})
	requestErrorsByReason = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "request_errors_total",
		Help:      "Total request errors by reason",
	}, []string{"webhook", "reason"})
	watcherErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "watcher_errors_total",
		Help:      "Total filesystem watcher errors",
	})
	requestsByStatusCode = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "requests_total",
		Help:      "Total requests by response status code",
	}, []string{"webhook", "status_code"})
)

func init() {
	prometheus.MustRegister(lastReloadError)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(successReloads)
	prometheus.MustRegister(requestErrorsByReason)
	prometheus.MustRegister(watcherErrors)
	prometheus.MustRegister(requestsByStatusCode)
}

func main() {
	flag.Var(&volumeDirs, "volume-dir", "the config map volume directory to watch for updates; may be used multiple times")
	flag.Var(&webhook, "webhook-url", "the url to send a request to when the specified config map volume directory has been updated")
	flag.Parse()

	if len(volumeDirs) < 1 {
		log.Println("Missing volume-dir")
		log.Println()
		flag.Usage()
		os.Exit(1)
	}

	if len(webhook) < 1 {
		log.Println("Missing webhook-url")
		log.Println()
		flag.Usage()
		os.Exit(1)
	}
	if *webhookRetries < 1 {
		log.Fatal("webhook-retries must be at least 1")
	}
	if *webhookTimeout <= 0 {
		log.Fatal("webhook-timeout must be greater than zero")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	for _, d := range volumeDirs {
		log.Printf("Watching directory: %q", d)
		err = watcher.Add(d)
		if err != nil {
			log.Fatal(err)
		}
	}

	updates := make(chan struct{}, 1)
	go watchEvents(watcher.Events, watcher.Errors, updates)
	go processUpdates(updates, &http.Client{Timeout: *webhookTimeout}, webhook, *webhookMethod, *webhookStatusCode, *webhookRetries, 10*time.Second)

	log.Fatal(serverMetrics(*listenAddress, *metricPath))
}

func watchEvents(events <-chan fsnotify.Event, errors <-chan error, updates chan<- struct{}) {
	defer close(updates)
	for events != nil || errors != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if !isValidEvent(event) {
				continue
			}
			log.Println("config map updated")
			select {
			case updates <- struct{}{}:
			default: // A reload is already pending; one follow-up reload is enough.
			}
		case err, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			watcherErrors.Inc()
			log.Println("error:", err)
		}
	}
}

func processUpdates(updates <-chan struct{}, client *http.Client, webhooks webhookFlag, method string, expectedStatus, retries int, retryDelay time.Duration) {
	for range updates {
		for _, h := range webhooks {
			triggerReload(client, h, method, expectedStatus, retries, retryDelay)
		}
	}
}

func triggerReload(client *http.Client, h *url.URL, method string, expectedStatus, retries int, retryDelay time.Duration) {
	begun := time.Now()
	req, err := http.NewRequest(method, h.String(), nil)
	if err != nil {
		setFailureMetrics(h.Redacted(), "client_request_create")
		log.Println("error:", err)
		return
	}
	if h.User != nil {
		if password, passwordSet := h.User.Password(); passwordSet {
			req.SetBasicAuth(h.User.Username(), password)
		}
	}

	for attempt := 1; attempt <= retries; attempt++ {
		log.Printf("performing webhook request (%d/%d)", attempt, retries)
		resp, err := client.Do(req)
		if err != nil {
			setFailureMetrics(h.Redacted(), "client_request_do")
			log.Println("error:", err)
		} else {
			resp.Body.Close()
			requestsByStatusCode.WithLabelValues(h.Redacted(), strconv.Itoa(resp.StatusCode)).Inc()
			if resp.StatusCode == expectedStatus {
				setSuccessMetrics(h.Redacted(), begun)
				log.Println("successfully triggered reload")
				return
			}
			setFailureMetrics(h.Redacted(), "client_response")
			log.Println("error:", "Received response code", resp.StatusCode, ", expected", expectedStatus)
		}
		if attempt < retries {
			time.Sleep(retryDelay)
		}
	}
	setFailureMetrics(h.Redacted(), "retries_exhausted")
	log.Println("error:", "Webhook reload retries exhausted")
}

func setFailureMetrics(h, reason string) {
	requestErrorsByReason.WithLabelValues(h, reason).Inc()
	lastReloadError.WithLabelValues(h).Set(1.0)
}

func setSuccessMetrics(h string, begun time.Time) {
	requestDuration.WithLabelValues(h).Set(time.Since(begun).Seconds())
	successReloads.WithLabelValues(h).Inc()
	lastReloadError.WithLabelValues(h).Set(0.0)
}

func isValidEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Create != fsnotify.Create && event.Op&fsnotify.Write != fsnotify.Write {
		return false
	}
	if filepath.Base(event.Name) != "..data" {
		return false
	}
	return true
}

func serverMetrics(listenAddress, metricsPath string) error {
	http.Handle(metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`
			<html>
			<head><title>ConfigMap Reload Metrics</title></head>
			<body>
			<h1>ConfigMap Reload</h1>
			<p><a href='` + metricsPath + `'>Metrics</a></p>
			</body>
			</html>
		`))
	})
	return http.ListenAndServe(listenAddress, nil)
}

type volumeDirsFlag []string

type webhookFlag []*url.URL

func (v *volumeDirsFlag) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func (v *volumeDirsFlag) String() string {
	return fmt.Sprint(*v)
}

func (v *webhookFlag) Set(value string) error {
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	*v = append(*v, u)
	return nil
}

func (v *webhookFlag) String() string {
	return fmt.Sprint(*v)
}
