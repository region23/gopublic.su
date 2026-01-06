package server

import (
	"sync"
	"testing"

	"github.com/hashicorp/yamux"
)

func TestTunnelRegistry_RegisterUnregister(t *testing.T) {
	registry := NewTunnelRegistry()

	// Use nil session for basic registry operations
	var session *yamux.Session = nil

	// Register with userID
	registry.Register("test.example.com", session, 1, false)

	// Verify registered
	got, ok := registry.GetSession("test.example.com")
	if !ok {
		t.Error("Expected session to be registered")
	}
	if got != session {
		t.Error("Expected same session to be returned")
	}

	// Unregister
	registry.Unregister("test.example.com")

	// Verify unregistered
	_, ok = registry.GetSession("test.example.com")
	if ok {
		t.Error("Expected session to be unregistered")
	}
}

func TestTunnelRegistry_ConcurrentAccess(t *testing.T) {
	registry := NewTunnelRegistry()

	var wg sync.WaitGroup
	numGoroutines := 100

	// Concurrent registrations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			domain := "test" + string(rune('0'+id%10)) + ".example.com"
			registry.Register(domain, nil, uint(id%10+1), false)
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			domain := "test" + string(rune('0'+id%10)) + ".example.com"
			registry.GetSession(domain)
		}(i)
	}

	// Concurrent unregistrations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			domain := "test" + string(rune('0'+id%10)) + ".example.com"
			registry.Unregister(domain)
		}(i)
	}

	wg.Wait()
}

func TestTunnelRegistry_GetSession_NotFound(t *testing.T) {
	registry := NewTunnelRegistry()

	_, ok := registry.GetSession("nonexistent.example.com")
	if ok {
		t.Error("Expected session not to be found")
	}
}

func TestTunnelRegistry_MultipleHosts(t *testing.T) {
	registry := NewTunnelRegistry()

	hosts := []string{"a.example.com", "b.example.com", "c.example.com"}

	// Register all
	for i, host := range hosts {
		registry.Register(host, nil, uint(i+1), false)
	}

	// Verify all registered
	for _, host := range hosts {
		_, ok := registry.GetSession(host)
		if !ok {
			t.Errorf("Expected %s to be registered", host)
		}
	}

	// Unregister one
	registry.Unregister("b.example.com")

	// Verify only b is gone
	_, ok := registry.GetSession("b.example.com")
	if ok {
		t.Error("Expected b.example.com to be unregistered")
	}

	// Others should still exist
	_, ok = registry.GetSession("a.example.com")
	if !ok {
		t.Error("Expected a.example.com to still be registered")
	}
}
