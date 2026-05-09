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
		{
			name:    "partial: only provider name",
			ep:      Endpoint{ProviderName: "baseten"},
			wantErr: "partially configured",
		},
		{
			name:    "partial: only wire",
			ep:      Endpoint{Wire: WireAPIResponses},
			wantErr: "partially configured",
		},
		{
			name:    "partial: only headers",
			ep:      Endpoint{Headers: map[string]string{"X-Org": "acme"}},
			wantErr: "partially configured",
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

func TestEndpoint_Clone_HeadersAreIndependent(t *testing.T) {
	t.Parallel()
	in := Endpoint{
		BaseURL:   "https://x",
		APIKeyEnv: "K",
		Headers:   map[string]string{"X-Org": "acme"},
	}
	out := in.Clone()
	out.Headers["X-Org"] = "evil"
	out.Headers["X-Extra"] = "added"
	if in.Headers["X-Org"] != "acme" {
		t.Errorf("Clone aliased Headers; original mutated to %q", in.Headers["X-Org"])
	}
	if _, ok := in.Headers["X-Extra"]; ok {
		t.Error("Clone aliased Headers; original gained an entry")
	}
}

func TestEndpoint_Clone_NilHeadersStayNil(t *testing.T) {
	t.Parallel()
	in := Endpoint{BaseURL: "https://x", APIKeyEnv: "K"}
	out := in.Clone()
	if out.Headers != nil {
		t.Errorf("nil Headers should clone to nil, got %v", out.Headers)
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

func TestEndpoint_String_HeadersListed(t *testing.T) {
	t.Parallel()
	// String() prints header KEYS in sorted plaintext (so operators can see
	// what shape the endpoint carries) and a SHA-256 fingerprint of the
	// canonical sorted "key=value\n" rows (so divergence diagnostics can
	// distinguish endpoints that differ only on header VALUES). Raw values
	// are deliberately omitted from the plaintext so operators don't leak
	// secrets they may have stuffed into auth-style headers.
	ep := Endpoint{
		BaseURL:   "https://x",
		APIKeyEnv: "K",
		Headers: map[string]string{
			"X-B": "secretvalue",
			"X-A": "alpha",
		},
	}
	got := ep.String()
	// Sorted, joined with comma, followed by a short value-fingerprint —
	// guarantees deterministic output for diff'ing.
	if !strings.Contains(got, "headers=[X-A,X-B]/") {
		t.Errorf("expected sorted headers + fingerprint in output, got %q", got)
	}
	if strings.Contains(got, "secretvalue") || strings.Contains(got, "alpha") {
		t.Errorf("header values must not appear in String(): %q", got)
	}

	// Two endpoints that differ only on header values must produce different
	// String() outputs (otherwise divergence error messages can't distinguish
	// value-only changes — the regression cursor flagged in r6).
	ep2 := ep
	ep2.Headers = map[string]string{"X-B": "different", "X-A": "alpha"}
	if ep.String() == ep2.String() {
		t.Errorf("value-only divergence must change String(): %q", ep.String())
	}

	noHeaders := Endpoint{BaseURL: "https://x", APIKeyEnv: "K"}
	if strings.Contains(noHeaders.String(), "headers=") {
		t.Errorf("empty Headers should not produce a headers= section, got %q", noHeaders.String())
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
