package monitor

import (
	"testing"

	M "github.com/sagernet/sing/common/metadata"
)

func TestParseProbeTarget_URLHTTPS_Defaults(t *testing.T) {
	dst, useTLS, hostHeader, serverName, path, insecure, ok := parseProbeTarget("https://api.openai.com/", false)
	if !ok {
		t.Fatalf("expected ok")
	}
	if !useTLS {
		t.Fatalf("expected useTLS=true")
	}
	if insecure {
		t.Fatalf("expected insecure=false")
	}
	if serverName != "api.openai.com" {
		t.Fatalf("serverName = %q", serverName)
	}
	if hostHeader != "api.openai.com" {
		t.Fatalf("hostHeader = %q", hostHeader)
	}
	if path != "/generate_204" {
		t.Fatalf("path = %q", path)
	}
	if dst != M.ParseSocksaddrHostPort("api.openai.com", 443) {
		t.Fatalf("dst = %v", dst)
	}
}

func TestParseProbeTarget_URLHTTP_WithPathAndPort(t *testing.T) {
	dst, useTLS, hostHeader, serverName, path, insecure, ok := parseProbeTarget("http://example.com:8080/health?x=1", true)
	if !ok {
		t.Fatalf("expected ok")
	}
	if useTLS {
		t.Fatalf("expected useTLS=false")
	}
	if !insecure {
		t.Fatalf("expected insecure=true")
	}
	if serverName != "example.com" {
		t.Fatalf("serverName = %q", serverName)
	}
	if hostHeader != "example.com:8080" {
		t.Fatalf("hostHeader = %q", hostHeader)
	}
	if path != "/health?x=1" {
		t.Fatalf("path = %q", path)
	}
	if dst != M.ParseSocksaddrHostPort("example.com", 8080) {
		t.Fatalf("dst = %v", dst)
	}
}

func TestParseProbeTarget_HostPort_Defaults(t *testing.T) {
	dst, useTLS, hostHeader, serverName, path, insecure, ok := parseProbeTarget("www.apple.com:80", false)
	if !ok {
		t.Fatalf("expected ok")
	}
	if useTLS {
		t.Fatalf("expected useTLS=false")
	}
	if insecure {
		t.Fatalf("expected insecure=false")
	}
	if serverName != "www.apple.com" {
		t.Fatalf("serverName = %q", serverName)
	}
	if hostHeader != "www.apple.com:80" {
		t.Fatalf("hostHeader = %q", hostHeader)
	}
	if path != "/generate_204" {
		t.Fatalf("path = %q", path)
	}
	if dst != M.ParseSocksaddrHostPort("www.apple.com", 80) {
		t.Fatalf("dst = %v", dst)
	}
}

func TestParseProbeTarget_HostOnly_DefaultPortAndPath(t *testing.T) {
	dst, useTLS, hostHeader, serverName, path, insecure, ok := parseProbeTarget("example.com", true)
	if !ok {
		t.Fatalf("expected ok")
	}
	if useTLS {
		t.Fatalf("expected useTLS=false")
	}
	if !insecure {
		t.Fatalf("expected insecure=true")
	}
	if serverName != "example.com" {
		t.Fatalf("serverName = %q", serverName)
	}
	if hostHeader != "example.com" {
		t.Fatalf("hostHeader = %q", hostHeader)
	}
	if path != "/generate_204" {
		t.Fatalf("path = %q", path)
	}
	if dst != M.ParseSocksaddrHostPort("example.com", 80) {
		t.Fatalf("dst = %v", dst)
	}
}