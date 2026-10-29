package daemon

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multica-ai/multica/server/pkg/httptransport"
)

// Route identifies which network path the daemon's server connections use.
type Route string

const (
	// RouteTunnel means connections go through the normal DNS/proxy path
	// (the existing behaviour, e.g. via NATFRP tunnel).
	RouteTunnel Route = "tunnel"
	// RouteLAN means connections are redirected to the LAN direct address,
	// bypassing any environment proxy.
	RouteLAN Route = "lan"
)

const (
	// lanProbeInterval is how often the LAN endpoint is probed.
	lanProbeInterval = 5 * time.Second
	// lanProbeTimeout is the per-probe timeout.
	lanProbeTimeout = 500 * time.Millisecond
	// lanProbeConfirmCount is the number of consecutive successes/failures
	// required before switching routes.
	lanProbeConfirmCount = 2
)

// routeSelector probes a LAN direct address and reports the current route.
// It is safe for concurrent use.
type routeSelector struct {
	// lanAddr is the LAN direct address (e.g. "192.168.31.201:8443").
	// Empty means the feature is disabled.
	lanAddr string
	// serverHost is the hostname from MULTICA_SERVER_URL (e.g. "mc.dozoyen.com").
	// Used as TLS ServerName and HTTP Host header.
	serverHost string

	// current is the currently active route.
	current atomic.Value // Route

	// consecutiveSuccess / consecutiveFailure track probe results.
	consecutiveSuccess atomic.Int64
	consecutiveFailure atomic.Int64

	// lastResult is the most recent probe result for diagnostics.
	lastResult atomic.Value // string

	// listeners are notified on route change.
	mu        sync.Mutex
	listeners []func(Route)

	// routeChanges is a pub/sub channel for route changes. The wakeup loop
	// subscribes so it can immediately reconnect the WebSocket when the route
	// changes rather than waiting for the next backoff tick.
	routeChanges *routeChangeWatcher

	// tunnelTransport is the RoundTripper for the tunnel route (existing behaviour:
	// environment proxy + DNS).
	tunnelTransport http.RoundTripper
	// lanTransport is the RoundTripper for the LAN route (direct dial to lanAddr,
	// TLS ServerName = serverHost, no proxy).
	lanTransport http.RoundTripper
}

// newRouteSelector creates a routeSelector. If lanAddr is empty the selector
// is disabled and always reports RouteTunnel.
func newRouteSelector(lanAddr, serverBaseURL string) *routeSelector {
	rs := &routeSelector{
		lanAddr: strings.TrimSpace(lanAddr),
	}
	if u, err := url.Parse(serverBaseURL); err == nil {
		rs.serverHost = u.Hostname()
	}
	if rs.lanAddr == "" {
		rs.current.Store(RouteTunnel)
		rs.tunnelTransport = httptransport.CloneDefault()
	} else {
		rs.current.Store(RouteTunnel) // start on tunnel until LAN is confirmed
		rs.tunnelTransport = httptransport.CloneDefault()
		rs.lanTransport = rs.buildLANTransport()
	}
	rs.routeChanges = newRouteChangeWatcher()
	return rs
}

// buildLANTransport creates the http.RoundTripper for LAN connections.
// It dials the LAN address directly (ignoring the request's addr), sets
// TLS ServerName to the original hostname, and never uses a proxy.
func (rs *routeSelector) buildLANTransport() *http.Transport {
	serverName := rs.serverHost
	if serverName == "" {
		host, _, err := net.SplitHostPort(rs.lanAddr)
		if err != nil {
			host = rs.lanAddr
		}
		serverName = host
	}

	lanDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return lanDialer.DialContext(ctx, network, rs.lanAddr)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: false,
		},
		Proxy: nil, // never use proxy for LAN
	}
}

// enabled reports whether LAN direct routing is configured.
func (rs *routeSelector) enabled() bool {
	return rs.lanAddr != ""
}

// currentRoute returns the currently active route.
func (rs *routeSelector) currentRoute() Route {
	if v := rs.current.Load(); v != nil {
		return v.(Route)
	}
	return RouteTunnel
}

// onRouteChange registers a callback invoked whenever the active route changes.
func (rs *routeSelector) onRouteChange(fn func(Route)) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.listeners = append(rs.listeners, fn)
}

// notifyLocked calls all registered listeners.
// Callers must hold rs.mu.
func (rs *routeSelector) notifyLocked(old, new Route) {
	for _, fn := range rs.listeners {
		fn(new)
	}
}

// setRoute atomically updates the current route and notifies listeners if changed.
func (rs *routeSelector) setRoute(r Route) {
	old := rs.currentRoute()
	if old == r {
		return
	}
	rs.current.Store(r)
	// Stale keep-alive sockets from the old route must not be reused after
	// the switch; drop them before any request observes the new route.
	rs.closeIdleConnections()
	rs.mu.Lock()
	rs.notifyLocked(old, r)
	rs.mu.Unlock()
	// Wake the WS reconnect waiters immediately so they don't sit out a backoff.
	if rs.routeChanges != nil {
		rs.routeChanges.notify()
	}
}

// transport returns the appropriate http.RoundTripper for the current route.
func (rs *routeSelector) transport() http.RoundTripper {
	if rs.currentRoute() == RouteLAN {
		return rs.lanTransport
	}
	return rs.tunnelTransport
}

// roundTripper returns a RoundTripper that always delegates to the current
// route's transport. This is used as http.Client.Transport so that route
// switches take effect on new connections immediately.
func (rs *routeSelector) roundTripper() http.RoundTripper {
	return &routeRoundTripper{rs: rs}
}

// routeRoundTripper delegates each request to the route's current transport.
type routeRoundTripper struct {
	rs *routeSelector
}

func (rt *routeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt.rs.transport().RoundTrip(req)
}

// dialContext returns a dialContext function for the WebSocket dialer.
// When on LAN it dials the LAN address; otherwise it uses the default dialer
// (which respects ProxyFromEnvironment).
func (rs *routeSelector) dialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	defaultDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if rs.currentRoute() == RouteLAN {
			return defaultDialer.DialContext(ctx, network, rs.lanAddr)
		}
		return defaultDialer.DialContext(ctx, network, addr)
	}
}

// probe performs a single HTTPS GET /readyz against the LAN address.
// It uses the original hostname for TLS ServerName and certificate verification.
func (rs *routeSelector) probe(ctx context.Context) bool {
	if !rs.enabled() {
		return false
	}

	probeURL := fmt.Sprintf("https://%s/readyz", rs.lanAddr)

	// Use the pre-built LAN transport (allows tests to inject a custom transport).
	transport := rs.lanTransport
	client := &http.Client{
		Transport: transport,
		Timeout:   lanProbeTimeout,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	// Set Host header to the original hostname.
	if rs.serverHost != "" {
		req.Host = rs.serverHost
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// run starts the probe loop. It blocks until ctx is cancelled.
func (rs *routeSelector) run(ctx context.Context) {
	if !rs.enabled() {
		return
	}

	ticker := time.NewTicker(lanProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rs.doProbe(ctx)
		}
	}
}

// doProbe runs one probe cycle and updates the route if needed.
func (rs *routeSelector) doProbe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, lanProbeTimeout)
	defer cancel()

	ok := rs.probe(probeCtx)

	if ok {
		rs.consecutiveFailure.Store(0)
		n := rs.consecutiveSuccess.Add(1)
		rs.lastResult.Store(fmt.Sprintf("success #%d", n))
		if n >= lanProbeConfirmCount && rs.currentRoute() != RouteLAN {
			rs.setRoute(RouteLAN)
		}
	} else {
		rs.consecutiveSuccess.Store(0)
		n := rs.consecutiveFailure.Add(1)
		rs.lastResult.Store(fmt.Sprintf("failure #%d", n))
		if n >= lanProbeConfirmCount && rs.currentRoute() != RouteTunnel {
			rs.setRoute(RouteTunnel)
		}
	}
}

// status returns a human-readable status string for logging/health output.
func (rs *routeSelector) status() string {
	if !rs.enabled() {
		return "disabled"
	}
	route := rs.currentRoute()
	result, _ := rs.lastResult.Load().(string)
	if result == "" {
		result = "no probe yet"
	}
	return fmt.Sprintf("route=%s last_probe=%s", route, result)
}

// closeIdleConnections closes idle connections on both route transports.
// Called when the route changes so stale keep-alive sockets from the old
// route are not reused.
func (rs *routeSelector) closeIdleConnections() {
	if rs.tunnelTransport != nil {
		if t, ok := rs.tunnelTransport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
	if rs.lanTransport != nil {
		if t, ok := rs.lanTransport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

// routeChangeWatcher is a tiny pub/sub for route changes. The wakeup loop
// subscribes so it can immediately reconnect the WebSocket when the route
// changes rather than waiting for the next backoff tick.
type routeChangeWatcher struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func newRouteChangeWatcher() *routeChangeWatcher {
	return &routeChangeWatcher{subscribers: make(map[chan struct{}]struct{})}
}

// Subscribe returns a channel that receives a non-blocking nudge whenever
// the route changes, and an unsubscribe func.
func (w *routeChangeWatcher) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subscribers[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subscribers, ch)
		w.mu.Unlock()
	}
}

// notify sends a non-blocking nudge to all subscribers.
func (w *routeChangeWatcher) notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
