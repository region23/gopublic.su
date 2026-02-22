package tunnel

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"gopublic/internal/client/events"
	"gopublic/internal/client/stats"
)

func TestNewTunnel(t *testing.T) {
	tun := NewTunnel("localhost:4443", "test-token", "3000")

	if tun.ServerAddr != "localhost:4443" {
		t.Errorf("expected ServerAddr localhost:4443, got %s", tun.ServerAddr)
	}
	if tun.Token != "test-token" {
		t.Errorf("expected Token test-token, got %s", tun.Token)
	}
	if tun.LocalPort != "3000" {
		t.Errorf("expected LocalPort 3000, got %s", tun.LocalPort)
	}
	if tun.activeConns == nil {
		t.Error("activeConns should be initialized")
	}
}

func TestTunnel_SetEventBus(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	bus := events.NewBus()

	tun.SetEventBus(bus)

	if tun.eventBus != bus {
		t.Error("eventBus should be set")
	}
}

func TestTunnel_SetStats(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	s := stats.New()

	tun.SetStats(s)

	if tun.stats != s {
		t.Error("stats should be set")
	}
}

func TestTunnel_SetTLSConfig(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	cfg := &TLSConfig{
		InsecureSkipVerify: true,
		ServerName:         "example.com",
	}

	tun.SetTLSConfig(cfg)

	if tun.TLSConfig != cfg {
		t.Error("TLSConfig should be set")
	}
	if !tun.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true")
	}
}

func TestTunnel_BoundDomains(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	// Initially empty
	domains := tun.BoundDomains()
	if len(domains) != 0 {
		t.Errorf("expected 0 domains, got %d", len(domains))
	}

	// Set domains directly (simulating handleSession)
	tun.mu.Lock()
	tun.boundDomains = []string{"test.example.com", "test2.example.com"}
	tun.mu.Unlock()

	domains = tun.BoundDomains()
	if len(domains) != 2 {
		t.Errorf("expected 2 domains, got %d", len(domains))
	}
}

func TestTunnel_TrackUntrackConn(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	// Create mock connections using pipe
	conn1, _ := net.Pipe()
	conn2, _ := net.Pipe()
	defer conn1.Close()
	defer conn2.Close()

	// Track connections
	tun.trackConn(conn1)
	tun.trackConn(conn2)

	tun.mu.Lock()
	count := len(tun.activeConns)
	tun.mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 tracked connections, got %d", count)
	}

	// Untrack one
	tun.untrackConn(conn1)

	tun.mu.Lock()
	count = len(tun.activeConns)
	tun.mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 tracked connection, got %d", count)
	}
}

func TestTunnel_PublishEvent(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	bus := events.NewBus()
	tun.SetEventBus(bus)

	ch := bus.Subscribe()

	// Publish event
	tun.publishEvent(events.EventConnecting, nil)

	select {
	case event := <-ch:
		if event.Type != events.EventConnecting {
			t.Errorf("expected EventConnecting, got %v", event.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestTunnel_PublishEvent_NilBus(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	// Should not panic with nil eventBus
	tun.publishEvent(events.EventConnecting, nil)
}

func TestTunnel_Shutdown_NotStarted(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Shutdown without starting should work
	err := tun.Shutdown(ctx)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestTunnel_Shutdown_AlreadyClosed(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	ctx := context.Background()

	// Close once
	tun.Shutdown(ctx)

	// Close again should be idempotent
	err := tun.Shutdown(ctx)
	if err != nil {
		t.Errorf("expected no error on second shutdown, got %v", err)
	}
}

func TestTunnel_Shutdown_WithActiveConns(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	// Create mock connections
	server, client := net.Pipe()
	defer server.Close()

	tun.trackConn(client)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := tun.Shutdown(ctx)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Connection should be closed
	_, err = client.Read(make([]byte, 1))
	if err == nil {
		t.Error("expected connection to be closed")
	}
}

func TestTunnel_CopyBidirectional(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	// Create two pipe pairs
	// local1 <-> remote1 are passed to copyBidirectional
	// We write to local2/remote2 to communicate
	local1, local2 := net.Pipe()
	remote1, remote2 := net.Pipe()

	defer local1.Close()
	defer local2.Close()
	defer remote1.Close()
	defer remote2.Close()

	// Start bidirectional copy in background
	done := make(chan struct{})
	go func() {
		tun.copyBidirectional(local1, remote1)
		close(done)
	}()

	// Test: local2 -> local1 -> remote1 -> remote2
	testData := []byte("hello from local")
	go func() {
		local2.Write(testData)
	}()

	// Read from remote2 (data should flow through)
	buf := make([]byte, 100)
	remote2.SetReadDeadline(time.Now().Add(time.Second))
	n, err := remote2.Read(buf)
	if err != nil {
		t.Logf("Read error (may be expected): %v", err)
	}
	if n > 0 && string(buf[:n]) != "hello from local" {
		t.Errorf("expected 'hello from local', got '%s'", string(buf[:n]))
	}

	// Close connections to stop the copy
	local2.Close()
	remote2.Close()

	// Wait for copyBidirectional to finish
	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Error("copyBidirectional did not finish in time")
	}
}

func TestTunnel_StatsIntegration(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	s := stats.New()
	tun.SetStats(s)

	// Simulate what proxyStream does
	if tun.stats != nil {
		tun.stats.IncrementConnections()
		tun.stats.RecordRequest(50*time.Millisecond, 1024)
		tun.stats.DecrementOpenConnections()
	}

	snap := s.Snapshot()
	if snap.TotalConnections != 1 {
		t.Errorf("expected 1 total connection, got %d", snap.TotalConnections)
	}
	if snap.TotalRequests != 1 {
		t.Errorf("expected 1 total request, got %d", snap.TotalRequests)
	}
	if snap.OpenConnections != 0 {
		t.Errorf("expected 0 open connections, got %d", snap.OpenConnections)
	}
}

func TestTunnel_EventsIntegration(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")
	bus := events.NewBus()
	tun.SetEventBus(bus)

	ch := bus.Subscribe()

	// Simulate connection events
	tun.publishEvent(events.EventConnecting, nil)
	tun.publishEvent(events.EventConnected, events.ConnectedData{
		ServerAddr:   "localhost:4443",
		BoundDomains: []string{"test.example.com"},
		Latency:      50 * time.Millisecond,
	})

	// Verify events
	event1 := <-ch
	if event1.Type != events.EventConnecting {
		t.Errorf("expected EventConnecting, got %v", event1.Type)
	}

	event2 := <-ch
	if event2.Type != events.EventConnected {
		t.Errorf("expected EventConnected, got %v", event2.Type)
	}
	data, ok := event2.Data.(events.ConnectedData)
	if !ok {
		t.Fatal("expected ConnectedData")
	}
	if data.ServerAddr != "localhost:4443" {
		t.Errorf("expected localhost:4443, got %s", data.ServerAddr)
	}
}

func TestTunnel_ConcurrentTrackUntrack(t *testing.T) {
	tun := NewTunnel("localhost:4443", "token", "3000")

	var wg sync.WaitGroup
	conns := make([]net.Conn, 100)

	// Create connections
	for i := 0; i < 100; i++ {
		conn, _ := net.Pipe()
		conns[i] = conn
	}

	// Track concurrently
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			tun.trackConn(conn)
		}(conns[i])
	}
	wg.Wait()

	tun.mu.Lock()
	count := len(tun.activeConns)
	tun.mu.Unlock()

	if count != 100 {
		t.Errorf("expected 100 connections, got %d", count)
	}

	// Untrack concurrently
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			tun.untrackConn(conn)
			conn.Close()
		}(conns[i])
	}
	wg.Wait()

	tun.mu.Lock()
	count = len(tun.activeConns)
	tun.mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 connections after untrack, got %d", count)
	}
}

func TestTLSConfig(t *testing.T) {
	cfg := &TLSConfig{
		InsecureSkipVerify: false,
		ServerName:         "secure.example.com",
	}

	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be false")
	}
	if cfg.ServerName != "secure.example.com" {
		t.Errorf("expected secure.example.com, got %s", cfg.ServerName)
	}
}

func TestResolveLocalAddr(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"3000", "localhost:3000"},
		{"8080", "localhost:8080"},
		{"example.test:80", "example.test:80"},
		{"127.0.0.1:3000", "127.0.0.1:3000"},
		{"myapp.local:8888", "myapp.local:8888"},
	}

	for _, tt := range tests {
		got := resolveLocalAddr(tt.input)
		if got != tt.want {
			t.Errorf("resolveLocalAddr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
