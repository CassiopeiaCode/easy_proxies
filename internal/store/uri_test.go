package store

import "testing"

func TestHostPortFromURI_HTTPRequiresPort(t *testing.T) {
	_, _, _, err := HostPortFromURI("http://example.com")
	if err == nil {
		t.Fatalf("expected error for http uri without port")
	}
}

func TestHostPortFromURI_HTTPSRequiresPort(t *testing.T) {
	_, _, _, err := HostPortFromURI("https://example.com")
	if err == nil {
		t.Fatalf("expected error for https uri without port")
	}
}

func TestHostPortFromURI_HTTPWithPort(t *testing.T) {
	host, port, protocol, err := HostPortFromURI("http://example.com:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if protocol != "http" {
		t.Fatalf("expected protocol=http, got %q", protocol)
	}
	if host != "example.com" || port != 8080 {
		t.Fatalf("expected example.com:8080, got %s:%d", host, port)
	}
}

