package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRouteSelectorDisabled verifies that an empty LanDirectAddr keeps the
// selector in tunnel mode and never switches.
func TestRouteSelectorDisabled(t *testing.T) {
	rs := newRouteSelector("", "https://mc.dozoyen.com:40816")
	if rs.enabled() {
		t.Fatal("expected selector to be disabled with empty LanDirectAddr")
	}
	if got := rs.currentRoute(); got != RouteTunnel {
		t.Fatalf("expected RouteTunnel, got %s", got)
	}
	if got := rs.status(); got != "disabled" {
		t.Fatalf("expected status 'disabled', got %q", got)
	}
}

// TestRouteSelectorInitialState verifies the selector starts on tunnel and
// reports the correct host/port.
func TestRouteSelectorInitialState(t *testing.T) {
	rs := newRouteSelector("192.168.31.201:8443", "https://mc.dozoyen.com:40816")
	if !rs.enabled() {
		t.Fatal("expected selector to be enabled")
	}
	if got := rs.currentRoute(); got != RouteTunnel {
		t.Fatalf("expected initial RouteTunnel, got %s", got)
	}
	if rs.serverHost != "mc.dozoyen.com" {
		t.Fatalf("expected serverHost mc.dozoyen.com, got %s", rs.serverHost)
	}
}

// TestRouteSelectorLANSwitch verifies the selector switches to LAN after
// consecutive successful probes.
func TestRouteSelectorLANSwitch(t *testing.T) {
	// Create a test HTTPS server that responds 200 to /readyz.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Extract host:port from test server URL.
	lanAddr := srv.Listener.Addr().String()

	rs := newRouteSelector(lanAddr, "https://mc.dozoyen.com:40816")

	// Override transport to trust the test server's cert.
	rs.lanTransport = srv.Client().Transport

	// Run probe directly.
	ctx := context.Background()
	rs.doProbe(ctx)
	if rs.currentRoute() != RouteTunnel {
		t.Fatalf("expected still RouteTunnel after 1 probe, got %s", rs.currentRoute())
	}
	rs.doProbe(ctx)
	if rs.currentRoute() != RouteLAN {
		t.Fatalf("expected RouteLAN after 2 successful probes, got %s", rs.currentRoute())
	}
}

// TestRouteSelectorTunnelFallback verifies the selector falls back to tunnel
// after consecutive failed probes.
func TestRouteSelectorTunnelFallback(t *testing.T) {
	// Use an address that will fail quickly.
	rs := newRouteSelector("127.0.0.1:1", "https://mc.dozoyen.com:40816")

	// Force LAN route first.
	rs.setRoute(RouteLAN)

	// Now probe should fail and eventually switch back.
	ctx := context.Background()
	rs.doProbe(ctx)
	if rs.currentRoute() != RouteLAN {
		t.Fatalf("expected still RouteLAN after 1 failure, got %s", rs.currentRoute())
	}
	rs.doProbe(ctx)
	if rs.currentRoute() != RouteTunnel {
		t.Fatalf("expected RouteTunnel after 2 failures, got %s", rs.currentRoute())
	}
}

// TestRouteSelectorRouteChangeCallback verifies listeners are notified on
// route changes.
func TestRouteSelectorRouteChangeCallback(t *testing.T) {
	rs := newRouteSelector("", "https://mc.dozoyen.com:40816")

	var called atomic.Int32
	rs.onRouteChange(func(r Route) {
		called.Add(1)
	})

	// Set to LAN (simulates a successful probe).
	rs.setRoute(RouteLAN)
	if called.Load() != 1 {
		t.Fatalf("expected 1 callback, got %d", called.Load())
	}

	// Setting again should not trigger.
	rs.setRoute(RouteLAN)
	if called.Load() != 1 {
		t.Fatalf("expected still 1 callback, got %d", called.Load())
	}

	// Switch back to tunnel.
	rs.setRoute(RouteTunnel)
	if called.Load() != 2 {
		t.Fatalf("expected 2 callbacks, got %d", called.Load())
	}
}

// TestRouteSelectorRouteChangeWatcher verifies the pub/sub mechanism works.
func TestRouteSelectorRouteChangeWatcher(t *testing.T) {
	rs := newRouteSelector("", "https://mc.dozoyen.com:40816")

	ch, unsub := rs.routeChanges.Subscribe()
	defer unsub()

	rs.setRoute(RouteLAN)

	select {
	case <-ch:
		// success
	case <-time.After(time.Second):
		t.Fatal("expected route change notification")
	}
}

// TestRouteSelectorCloseIdleConnections verifies closeIdleConnections
// does not panic when called.
func TestRouteSelectorCloseIdleConnections(t *testing.T) {
	rs := newRouteSelector("192.168.31.201:8443", "https://mc.dozoyen.com:40816")
	rs.closeIdleConnections() // should not panic
}

// TestRouteSelectorDialContextTunnel verifies the tunnel dial context
// dials the original address.
func TestRouteSelectorDialContextTunnel(t *testing.T) {
	// Start a test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rs := newRouteSelector("192.168.31.201:8443", srv.URL)

	// On tunnel route, dialContext should dial the original addr.
	ctx := context.Background()
	conn, err := rs.dialContext()(ctx, "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("tunnel dial failed: %v", err)
	}
	conn.Close()
}

// TestRouteSelectorDialContextLAN verifies the LAN dial context
// dials the LAN address regardless of the input addr.
func TestRouteSelectorDialContextLAN(t *testing.T) {
	// Start a test server (simulates the LAN endpoint).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lanAddr := srv.Listener.Addr().String()
	rs := newRouteSelector(lanAddr, "https://mc.dozoyen.com:40816")
	rs.setRoute(RouteLAN)

	// On LAN route, dialContext should dial the LAN addr even when
	// asked for a different address.
	ctx := context.Background()
	conn, err := rs.dialContext()(ctx, "tcp", "10.255.255.1:9999")
	if err != nil {
		t.Fatalf("LAN dial failed: %v", err)
	}
	conn.Close()
}

// TestRouteSelectorTransportSelection verifies the transport method returns
// the correct transport for each route.
func TestRouteSelectorTransportSelection(t *testing.T) {
	rs := newRouteSelector("192.168.31.201:8443", "https://mc.dozoyen.com:40816")

	if rs.transport() != rs.tunnelTransport {
		t.Fatal("expected tunnel transport on tunnel route")
	}
	rs.setRoute(RouteLAN)
	if rs.transport() != rs.lanTransport {
		t.Fatal("expected LAN transport on LAN route")
	}
}
