package llmendpoint

import (
	"strings"
	"testing"
)

func TestEndpoint_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ep      Endpoint
		wantErr string
	}{
		{name: "zero is valid", ep: Endpoint{}},
		{
			name: "ok env-key",
			ep: Endpoint{
				BaseURL:   "https://inference.baseten.co/v1",
				APIKeyEnv: "BASETEN_API_KEY",
			},
		},
		{
			name: "ok inline-key",
			ep: Endpoint{
				BaseURL: "https://example.com",
				APIKey:  "sk-xxx",
				Wire:    WireAPIChat,
			},
		},
		{
			name:    "missing base url",
			ep:      Endpoint{APIKeyEnv: "X"},
			wantErr: "BaseURL is required",
		},
		{
			name:    "non-http scheme",
			ep:      Endpoint{BaseURL: "ftp://example.com", APIKeyEnv: "X"},
			wantErr: "must be http(s)",
		},
		{
			name:    "missing key",
			ep:      Endpoint{BaseURL: "https://example.com"},
			wantErr: "APIKey or APIKeyEnv",
		},
		{
			name:    "bad wire api",
			ep:      Endpoint{BaseURL: "https://example.com", APIKeyEnv: "X", Wire: "grpc"},
			wantErr: "unknown wire api",
		},
		{
			name:    "bad provider name (space)",
			ep:      Endpoint{BaseURL: "https://example.com", APIKeyEnv: "X", ProviderName: "has space"},
			wantErr: "invalid ProviderName",
		},
		{
			name:    "bad provider name (dot would unquote into nested config segment)",
			ep:      Endpoint{BaseURL: "https://example.com", APIKeyEnv: "X", ProviderName: "a.b"},
			wantErr: "invalid ProviderName",
		},
		{
			name:    "hostless URL",
			ep:      Endpoint{BaseURL: "https:///v1", APIKeyEnv: "X"},
			wantErr: "missing a host",
		},
		{
			name: "ok with simple headers",
			ep: Endpoint{
				BaseURL:   "https://example.com",
				APIKeyEnv: "X",
				Headers:   map[string]string{"X-Org": "acme"},
			},
		},
		{
			name: "bad header name (space)",
			ep: Endpoint{
				BaseURL:   "https://example.com",
				APIKeyEnv: "X",
				Headers:   map[string]string{"X Org": "acme"},
			},
			wantErr: "invalid header name",
		},
		{
			name: "bad header name (CRLF injection)",
			ep: Endpoint{
				BaseURL:   "https://example.com",
				APIKeyEnv: "X",
				Headers:   map[string]string{"X-Org\r\nInjected": "acme"},
			},
			wantErr: "invalid header name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.ep.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEndpoint_Redacted(t *testing.T) {
	t.Parallel()
	in := Endpoint{
		BaseURL:   "https://inference.baseten.co/v1",
		APIKey:    "sk-secret",
		APIKeyEnv: "BASETEN_API_KEY",
	}
	out := in.Redacted()
	if out.APIKey != "" {
		t.Errorf("APIKey not cleared: %q", out.APIKey)
	}
	if out.APIKeyEnv != "BASETEN_API_KEY" {
		t.Errorf("APIKeyEnv lost: %q", out.APIKeyEnv)
	}
	if in.APIKey != "sk-secret" {
		t.Error("Redacted mutated receiver")
	}
}

func TestEndpoint_ResolvedKey(t *testing.T) {
	// Cannot t.Parallel here: child uses t.Setenv.
	t.Run("inline wins", func(t *testing.T) {
		ep := Endpoint{APIKey: "inline", APIKeyEnv: "PATH"}
		if got := ep.ResolvedKey(); got != "inline" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("LLMENDPOINT_TEST_KEY", "from-env")
		ep := Endpoint{APIKeyEnv: "LLMENDPOINT_TEST_KEY"}
		if got := ep.ResolvedKey(); got != "from-env" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("empty when unset", func(t *testing.T) {
		ep := Endpoint{APIKeyEnv: "DEFINITELY_NOT_SET_LLMENDPOINT_TEST"}
		if got := ep.ResolvedKey(); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestEndpoint_String_NoLeak(t *testing.T) {
	t.Parallel()
	ep := Endpoint{
		BaseURL:   "https://inference.baseten.co/v1",
		APIKey:    "super-secret-key",
		APIKeyEnv: "BASETEN_API_KEY",
	}
	got := ep.String()
	if strings.Contains(got, "super-secret-key") {
		t.Fatalf("API key leaked: %s", got)
	}
	if !strings.Contains(got, "$BASETEN_API_KEY") {
		t.Fatalf("expected key source label, got %s", got)
	}
}

func TestEndpoint_Defaults(t *testing.T) {
	t.Parallel()
	ep := Endpoint{}
	if ep.Provider() != DefaultProviderName {
		t.Errorf("Provider default %q", ep.Provider())
	}
	if ep.WireAPI() != WireAPIChat {
		t.Errorf("WireAPI default %q", ep.WireAPI())
	}
}

func TestEndpoint_IsZero(t *testing.T) {
	t.Parallel()
	if !(Endpoint{}).IsZero() {
		t.Error("zero endpoint should be IsZero")
	}
	if (Endpoint{BaseURL: "https://x"}).IsZero() {
		t.Error("non-zero endpoint should not be IsZero")
	}
}
