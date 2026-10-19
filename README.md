# Kubernetes ConfigMap Reload

[![license](https://img.shields.io/github/license/stefanoghinelli/configmap-reload.svg)](LICENSE.txt)

**configmap-reload** is a simple binary to trigger a reload when Kubernetes ConfigMaps or Secrets, mounted into pods,
are updated.
It watches mounted volume dirs and notifies the target process that the config map has been changed.
It currently only supports sending an HTTP request, but in future it is expected to support sending OS
(e.g. SIGHUP) once Kubernetes supports pod PID namespaces.

Images built from this fork are published at <https://github.com/stefanoghinelli/configmap-reload/pkgs/container/configmap-reload>.
Use `ghcr.io/stefanoghinelli/configmap-reload:<version>` for a tagged release.

### Releases

Push a `v*` tag in this fork after the change is merged into `main`. The release workflow tests the code, publishes a multi-architecture image to the fork's GitHub Container Registry, verifies its digest, and creates a GitHub Release containing the immutable image reference. Pushes to `main` do not publish images.

### Usage

```
Usage of ./out/configmap-reload:
  -volume-dir value
        the config map volume directory to watch for updates; may be used multiple times
  -web.listen-address string
    	  address to listen on for web interface and telemetry. (default ":9533")
  -web.telemetry-path string
    	  path under which to expose metrics. (default "/metrics")
  -webhook-ca-file string
        PEM CA certificate file for HTTPS webhook requests
  -webhook-client-cert-file string
        PEM client certificate file for HTTPS webhook requests
  -webhook-client-key-file string
        PEM client key file for HTTPS webhook requests
  -webhook-method string
        the HTTP method url to use to send the webhook (default "POST")
  -webhook-retries int
        the amount of times to retry the webhook reload request (default 1)
  -webhook-status-code int
        the HTTP status code indicating successful triggering of reload (default 200)
  -webhook-timeout duration
        maximum duration of each webhook request (default 10s)
  -webhook-url value
        the url to send a request to when the specified config map volume directory has been updated
```

Webhook URLs must use `http` or `https` and include a host. The HTTP method, expected status code, retry count, and request timeout are validated at startup.
Metrics use `webhook="0"`, `webhook="1"`, and so on in the order of the `-webhook-url` flags. URLs are omitted from metrics and logs because they may contain credentials or tokens.

### HTTPS webhook with mTLS

For a reload endpoint that requires a client certificate, mount a CA certificate, client certificate, and private key in the sidecar and pass their paths:

```sh
configmap-reload \
  -volume-dir=/etc/config \
  -webhook-url=https://localhost:9093/-/reload \
  -webhook-ca-file=/etc/webhook-tls/ca.crt \
  -webhook-client-cert-file=/etc/webhook-tls/tls.crt \
  -webhook-client-key-file=/etc/webhook-tls/tls.key
```

The CA file adds trusted certificates to the system roots. The client certificate and key must be configured together. TLS flags require HTTPS URLs, and all configured webhook URLs use the same TLS files. Files must be valid at startup and are checked again before each request, including after Kubernetes Secret symlink rotation. Server certificate verification remains enabled. When no TLS flags are set, the existing HTTP client behavior is unchanged.

### License

This project is [Apache Licensed](LICENSE.txt)
