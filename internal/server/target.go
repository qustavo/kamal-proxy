package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const (
	DefaultAddTimeout   = time.Second * 5
	DefaultDrainTimeout = time.Second * 5

	DefaultHealthCheckPath     = "/up"
	DefaultHealthCheckInterval = time.Second
	DefaultHealthCheckTimeout  = time.Second * 5

	MaxIdleConnsPerHost = 100
)

var (
	ErrorInvalidHostPattern = errors.New("invalid host pattern")

	hostRegex = regexp.MustCompile(`^(\w[-_.\w+]+)(:\d+)?$`)
)

type HealthCheckConfig struct {
	Path     string        `json:"path"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
}

type TargetOptions struct {
	MaxRequestBodySize int64  `json:"max_request_body_size"`
	TLSHostname        string `json:"tls_hostname"`
	ACMEDirectory      string `json:"acme_directory"`
	ACMECachePath      string `json:"acme_cache_path"`
}

func (to TargetOptions) RequireTLS() bool {
	return to.TLSHostname != ""
}

type TargetState int

const (
	TargetStateAdding TargetState = iota
	TargetStateDraining
	TargetStateHealthy
)

func (ts TargetState) String() string {
	switch ts {
	case TargetStateAdding:
		return "adding"
	case TargetStateDraining:
		return "draining"
	case TargetStateHealthy:
		return "healthy"
	}
	return ""
}

type inflightMap map[*http.Request]context.CancelFunc

type Target struct {
	targetURL         *url.URL
	healthCheckConfig HealthCheckConfig
	options           TargetOptions
	proxyHandler      http.Handler
	certManager       *autocert.Manager

	state        TargetState
	inflight     inflightMap
	inflightLock sync.Mutex

	healthcheck   *HealthCheck
	becameHealthy chan (bool)
}

func NewTarget(targetURL string, healthCheckConfig HealthCheckConfig, options TargetOptions) (*Target, error) {
	uri, err := parseTargetURL(targetURL)
	if err != nil {
		return nil, err
	}

	service := &Target{
		targetURL:         uri,
		healthCheckConfig: healthCheckConfig,
		options:           options,

		state:    TargetStateAdding,
		inflight: inflightMap{},
	}

	service.proxyHandler = service.createProxyHandler()
	service.certManager = service.createCertManager()

	return service, nil
}

func (t *Target) Target() string {
	return t.targetURL.Host
}

func (t *Target) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req = t.beginInflightRequest(req)
	if req == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer t.endInflightRequest(req)

	wasTLS := req.TLS != nil
	if t.options.RequireTLS() && !wasTLS {
		t.redirectToHTTPS(w, req)
		return
	}

	t.proxyHandler.ServeHTTP(w, req)
}

func (t *Target) Rewrite(req *httputil.ProxyRequest) {
	// Preserve & append X-Forwarded-For
	req.Out.Header["X-Forwarded-For"] = req.In.Header["X-Forwarded-For"]
	req.SetXForwarded()

	req.SetURL(t.targetURL)
	req.Out.Host = req.In.Host
}

func (t *Target) Drain(timeout time.Duration) {
	t.updateState(TargetStateDraining)

	deadline := time.After(timeout)
	toCancel := t.pendingRequestsToCancel()

WAIT_FOR_REQUESTS_TO_COMPLETE:
	for req := range toCancel {
		select {
		case <-req.Context().Done():
		case <-deadline:
			break WAIT_FOR_REQUESTS_TO_COMPLETE
		}
	}

	for _, cancel := range toCancel {
		cancel()
	}
}

func (t *Target) BeginHealthChecks() {
	t.becameHealthy = make(chan bool)
	t.healthcheck = NewHealthCheck(t,
		t.targetURL.JoinPath(t.healthCheckConfig.Path),
		t.healthCheckConfig.Interval,
		t.healthCheckConfig.Timeout,
	)
}

func (t *Target) StopHealthChecks() {
	if t.healthcheck != nil {
		t.healthcheck.Close()
		t.healthcheck = nil
	}
}

func (t *Target) WaitUntilHealthy(timeout time.Duration) bool {
	select {
	case <-time.After(timeout):
		return false
	case <-t.becameHealthy:
		return true
	}
}

// HealthCheckConsumer

func (t *Target) HealthCheckCompleted(success bool) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if success && t.state == TargetStateAdding {
		t.state = TargetStateHealthy
		close(t.becameHealthy)
	}

	slog.Info("Target health updated", "target", t.targetURL.Host, "success", success, "state", t.state.String())
}

// Private

func (t *Target) createProxyHandler() http.Handler {
	var handler http.Handler

	handler = &httputil.ReverseProxy{
		Rewrite:      t.Rewrite,
		ErrorHandler: t.handleProxyError,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		},
	}

	if t.options.MaxRequestBodySize > 0 {
		handler = http.MaxBytesHandler(handler, t.options.MaxRequestBodySize)
		slog.Debug("Using max request body limit", "target", t.Target(), "size", t.options.MaxRequestBodySize)
	}

	return handler
}

func (s *Target) createCertManager() *autocert.Manager {
	if s.options.TLSHostname == "" {
		return nil
	}

	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(s.options.ACMECachePath),
		HostPolicy: autocert.HostWhitelist(s.options.TLSHostname),
		Client:     &acme.Client{DirectoryURL: s.options.ACMEDirectory},
	}
}

func (t *Target) handleProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, context.Canceled) {
		slog.Error("Error while proxying", "target", t.Target(), "path", r.URL.Path, "error", err)
	}

	if t.isRequestEntityTooLarge(err) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	} else {
		w.WriteHeader(http.StatusBadGateway)
	}
}

func (t *Target) isRequestEntityTooLarge(err error) bool {
	var maxBytesError *http.MaxBytesError
	return errors.As(err, &maxBytesError)
}

func (t *Target) updateState(state TargetState) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	t.state = state
}

func (t *Target) beginInflightRequest(req *http.Request) *http.Request {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if t.state == TargetStateDraining {
		return nil
	}

	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	t.inflight[req] = cancel
	return req
}

func (t *Target) endInflightRequest(req *http.Request) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	cancel := t.inflight[req]
	cancel() // If Drain is waiting on us, let it know we're done

	delete(t.inflight, req)
}

func (t *Target) pendingRequestsToCancel() inflightMap {
	// We use a copy of the inflight map to iterate over while draining, so that
	// we don't need to lock it the whole time, which could interfere with the
	// locking that happens when requests end.
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	result := inflightMap{}
	for k, v := range t.inflight {
		result[k] = v
	}
	return result
}

func (t *Target) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	url := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, url, http.StatusMovedPermanently)
}

func parseTargetURL(targetURL string) (*url.URL, error) {
	if !hostRegex.MatchString(targetURL) {
		return nil, fmt.Errorf("%s :%w", targetURL, ErrorInvalidHostPattern)
	}

	uri, _ := url.Parse("http://" + targetURL)
	return uri, nil
}