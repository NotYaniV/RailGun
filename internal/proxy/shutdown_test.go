package proxy

import (
	"testing"
	"time"

	"github.com/NotYaniV/RailGun/internal/config"
)

// Reproduces the shutdown sequence cmd/railgun/main.go actually performs:
// StopAccepting, then drain the listeners, then Shutdown.
func TestShutdownAfterStopAcceptingStillDrains(t *testing.T) {
	snap, err := config.NewSnapshot(1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var store config.Store
	store.Store(snap)
	s := New(&store, Options{})

	// One in-flight connection, exactly what Shutdown is supposed to wait for.
	s.conns.Add(1)

	s.StopAccepting() // main.go:156

	never := make(chan struct{}) // no deadline: Shutdown must block on the conn
	returned := make(chan struct{})
	go func() {
		s.Shutdown(never) // main.go:172
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("Shutdown returned while a connection was still in flight — " +
			"in-flight requests are cut, not drained")
	case <-time.After(200 * time.Millisecond):
	}

	s.conns.Done()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown never returned after the connection finished")
	}
}
