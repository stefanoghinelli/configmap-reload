package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	webhookCAFile     = flag.String("webhook-ca-file", "", "PEM CA certificate file for HTTPS webhook requests")
	webhookCertFile   = flag.String("webhook-client-cert-file", "", "PEM client certificate file for HTTPS webhook requests")
	webhookKeyFile    = flag.String("webhook-client-key-file", "", "PEM client key file for HTTPS webhook requests")
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
	if err := validateRequestOptions(*webhookMethod, *webhookStatusCode, *webhookRetries, *webhookTimeout); err != nil {
		log.Fatal(err)
	}
	webhookURLs, err := parseWebhooks(webhook)
	if err != nil {
		log.Fatal(err)
	}
	client, err := newWebhookClient(*webhookTimeout, webhookTLSOptions{
		caFile: *webhookCAFile, certFile: *webhookCertFile, keyFile: *webhookKeyFile,
	}, webhookURLs)
	if err != nil {
		log.Fatal(err)
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
	go processUpdates(updates, client, webhookURLs, *webhookMethod, *webhookStatusCode, *webhookRetries, 10*time.Second)

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

func processUpdates(updates <-chan struct{}, client *http.Client, webhooks []*url.URL, method string, expectedStatus, retries int, retryDelay time.Duration) {
	for range updates {
		for i, h := range webhooks {
			triggerReload(client, h, strconv.Itoa(i), method, expectedStatus, retries, retryDelay)
		}
	}
}

func triggerReload(client *http.Client, h *url.URL, label, method string, expectedStatus, retries int, retryDelay time.Duration) {
	begun := time.Now()
	req, err := http.NewRequest(method, h.String(), nil)
	if err != nil {
		setFailureMetrics(label, "client_request_create")
		log.Println("error: could not create webhook request")
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
			setFailureMetrics(label, "client_request_do")
			log.Printf("error: webhook request failed (%T)", err)
		} else {
			resp.Body.Close()
			requestsByStatusCode.WithLabelValues(label, strconv.Itoa(resp.StatusCode)).Inc()
			if resp.StatusCode == expectedStatus {
				setSuccessMetrics(label, begun)
				log.Println("successfully triggered reload")
				return
			}
			setFailureMetrics(label, "client_response")
			log.Println("error:", "Received response code", resp.StatusCode, ", expected", expectedStatus)
		}
		if attempt < retries {
			time.Sleep(retryDelay)
		}
	}
	setFailureMetrics(label, "retries_exhausted")
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

type webhookFlag []string

func (v *volumeDirsFlag) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func (v *volumeDirsFlag) String() string {
	return fmt.Sprint(*v)
}

func (v *webhookFlag) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func (v *webhookFlag) String() string {
	return fmt.Sprintf("%d configured", len(*v))
}

func parseWebhooks(values webhookFlag) ([]*url.URL, error) {
	webhooks := make([]*url.URL, 0, len(values))
	for i, value := range values {
		u, err := url.Parse(value)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Fragment != "" {
			return nil, fmt.Errorf("webhook-url %d must be an absolute HTTP(S) URL without a fragment", i+1)
		}
		webhooks = append(webhooks, u)
	}
	return webhooks, nil
}

func validateRequestOptions(method string, status, retries int, timeout time.Duration) error {
	if strings.TrimSpace(method) != method || method == "" {
		return fmt.Errorf("webhook-method must be a valid HTTP method")
	}
	if _, err := http.NewRequest(method, "http://example.invalid/", nil); err != nil {
		return fmt.Errorf("webhook-method must be a valid HTTP method")
	}
	if status < 100 || status > 599 {
		return fmt.Errorf("webhook-status-code must be between 100 and 599")
	}
	if retries < 1 {
		return fmt.Errorf("webhook-retries must be at least 1")
	}
	if timeout <= 0 {
		return fmt.Errorf("webhook-timeout must be greater than zero")
	}
	return nil
}

type webhookTLSOptions struct {
	caFile   string
	certFile string
	keyFile  string
}

func (o webhookTLSOptions) enabled() bool {
	return o.caFile != "" || o.certFile != "" || o.keyFile != ""
}

func newWebhookClient(timeout time.Duration, options webhookTLSOptions, webhooks []*url.URL) (*http.Client, error) {
	if !options.enabled() {
		return &http.Client{Timeout: timeout}, nil
	}
	if (options.certFile == "") != (options.keyFile == "") {
		return nil, fmt.Errorf("webhook client certificate and key files must be configured together")
	}
	for i, webhook := range webhooks {
		if webhook.Scheme != "https" {
			return nil, fmt.Errorf("webhook-url %d must use HTTPS when webhook TLS files are configured", i+1)
		}
	}

	material, err := readWebhookTLSMaterial(options)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := material.config()
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{
		Timeout: timeout,
		Transport: &reloadingWebhookTLSTransport{
			options: options, fingerprint: material.fingerprint, transport: transport,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("webhook TLS redirect must stay on the same HTTPS endpoint")
			}
			return nil
		},
	}, nil
}

type webhookTLSMaterial struct {
	caPEM       []byte
	certPEM     []byte
	keyPEM      []byte
	fingerprint [sha256.Size]byte
}

func readWebhookTLSMaterial(options webhookTLSOptions) (webhookTLSMaterial, error) {
	var material webhookTLSMaterial
	var err error
	if options.caFile != "" {
		material.caPEM, err = os.ReadFile(options.caFile)
		if err != nil {
			return material, fmt.Errorf("read webhook CA file: %w", err)
		}
	}
	if options.certFile != "" {
		material.certPEM, err = os.ReadFile(options.certFile)
		if err != nil {
			return material, fmt.Errorf("read webhook client certificate file: %w", err)
		}
		material.keyPEM, err = os.ReadFile(options.keyFile)
		if err != nil {
			return material, fmt.Errorf("read webhook client key file: %w", err)
		}
	}
	hash := sha256.New()
	for _, part := range [][]byte{material.caPEM, material.certPEM, material.keyPEM} {
		fmt.Fprintf(hash, "%d:", len(part))
		hash.Write(part)
	}
	copy(material.fingerprint[:], hash.Sum(nil))
	return material, nil
}

func (m webhookTLSMaterial) config() (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if m.caPEM != nil {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(m.caPEM) {
			return nil, fmt.Errorf("webhook CA file contains no valid PEM certificates")
		}
		config.RootCAs = roots
	}
	if m.certPEM != nil {
		certificate, err := tls.X509KeyPair(m.certPEM, m.keyPEM)
		if err != nil {
			return nil, fmt.Errorf("invalid webhook client certificate or key: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

type reloadingWebhookTLSTransport struct {
	options     webhookTLSOptions
	mu          sync.Mutex
	fingerprint [sha256.Size]byte
	transport   *http.Transport
}

func (t *reloadingWebhookTLSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("webhook TLS requires HTTPS")
	}
	material, err := readWebhookTLSMaterial(t.options)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if material.fingerprint != t.fingerprint {
		config, err := material.config()
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = config
		old := t.transport
		t.transport = transport
		t.fingerprint = material.fingerprint
		old.CloseIdleConnections()
	}
	transport := t.transport
	t.mu.Unlock()
	return transport.RoundTrip(req)
}
