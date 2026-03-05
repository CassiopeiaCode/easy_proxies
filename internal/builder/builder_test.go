package builder

import "testing"

func TestValidateNodeURI_HTTPRequiresPort(t *testing.T) {
	if err := ValidateNodeURI("http://example.com", false); err == nil {
		t.Fatalf("expected error for http uri without port")
	}
}

func TestValidateNodeURI_HTTPSRequiresPort(t *testing.T) {
	if err := ValidateNodeURI("https://example.com", false); err == nil {
		t.Fatalf("expected error for https uri without port")
	}
}

func TestValidateNodeURI_HTTPWithPort(t *testing.T) {
	if err := ValidateNodeURI("http://example.com:8080", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

