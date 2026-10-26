package resolve

import "testing"

func TestLocalAddress(t *testing.T) {
	for v, want := range map[string]bool{
		":8082": true, "127.0.0.1:50051": true, "localhost": true, "0.0.0.0": true, "http://localhost:8080": true,
		"http://localhost": true, "[::1]:443": true,
		"exports.parts.dispatch.low": false, "/kiosk/video": false, "/": false, "otel-collector:4318": false,
		"http://cart:8081": false, "": false,
	} {
		if got := localAddress(v); got != want {
			t.Errorf("localAddress(%q) = %v, want %v", v, got, want)
		}
	}
}
