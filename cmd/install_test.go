package cmd

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestWaitForServeRequiresExpectedProcess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const serverPID = 12345
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"pid": serverPID})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	port := listener.Addr().(*net.TCPAddr).Port
	if err := waitForServe(port, serverPID, time.Second); err != nil {
		t.Fatalf("expected process was not accepted: %v", err)
	}
	if err := waitForServe(port, serverPID+1, 20*time.Millisecond); err == nil {
		t.Fatal("different process on the configured port was accepted as ready")
	}
}
