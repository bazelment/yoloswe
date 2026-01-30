package codex

import (
	"context"
	"testing"
)

func TestDefaultClientConfig(t *testing.T) {
	cfg := defaultClientConfig()

	if cfg.ClientName == "" {
		t.Error("ClientName should have a default")
	}
	if cfg.ClientVersion == "" {
		t.Error("ClientVersion should have a default")
	}
	if cfg.EventBufferSize <= 0 {
		t.Error("EventBufferSize should be positive")
	}
}

func TestClientOption_WithCodexPath(t *testing.T) {
	cfg := defaultClientConfig()
	WithCodexPath("/usr/local/bin/codex")(&cfg)

	if cfg.CodexPath != "/usr/local/bin/codex" {
		t.Errorf("unexpected CodexPath: %q", cfg.CodexPath)
	}
}

func TestClientOption_WithClientName(t *testing.T) {
	cfg := defaultClientConfig()
	WithClientName("my-app")(&cfg)

	if cfg.ClientName != "my-app" {
		t.Errorf("unexpected ClientName: %q", cfg.ClientName)
	}
}

func TestClientOption_WithClientVersion(t *testing.T) {
	cfg := defaultClientConfig()
	WithClientVersion("2.0.0")(&cfg)

	if cfg.ClientVersion != "2.0.0" {
		t.Errorf("unexpected ClientVersion: %q", cfg.ClientVersion)
	}
}

func TestClientOption_WithEventBufferSize(t *testing.T) {
	cfg := defaultClientConfig()
	WithEventBufferSize(500)(&cfg)

	if cfg.EventBufferSize != 500 {
		t.Errorf("unexpected EventBufferSize: %d", cfg.EventBufferSize)
	}
}

func TestClientOption_WithStderrHandler(t *testing.T) {
	cfg := defaultClientConfig()
	called := false
	handler := func(data []byte) {
		called = true
	}
	WithStderrHandler(handler)(&cfg)

	if cfg.StderrHandler == nil {
		t.Error("StderrHandler should be set")
	}

	// Call the handler to verify it's the right one
	cfg.StderrHandler([]byte("test"))
	if !called {
		t.Error("StderrHandler was not called")
	}
}

func TestClientOption_WithApprovalHandler(t *testing.T) {
	cfg := defaultClientConfig()
	handler := AutoApproveHandler()
	WithApprovalHandler(handler)(&cfg)

	if cfg.ApprovalHandler == nil {
		t.Error("ApprovalHandler should be set")
	}
}

func TestClientOptions_Multiple(t *testing.T) {
	cfg := defaultClientConfig()
	opts := []ClientOption{
		WithCodexPath("/custom/codex"),
		WithClientName("test-client"),
		WithClientVersion("1.0.0"),
		WithEventBufferSize(200),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.CodexPath != "/custom/codex" {
		t.Errorf("CodexPath not set correctly")
	}
	if cfg.ClientName != "test-client" {
		t.Errorf("ClientName not set correctly")
	}
	if cfg.ClientVersion != "1.0.0" {
		t.Errorf("ClientVersion not set correctly")
	}
	if cfg.EventBufferSize != 200 {
		t.Errorf("EventBufferSize not set correctly")
	}
}

func TestDefaultThreadConfig(t *testing.T) {
	cfg := defaultThreadConfig()

	// Thread config starts empty, all fields are optional
	if cfg.Model != "" {
		t.Error("Model should be empty by default")
	}
	if cfg.WorkDir != "" {
		t.Error("WorkDir should be empty by default")
	}
}

func TestThreadOption_WithModel(t *testing.T) {
	cfg := defaultThreadConfig()
	WithModel("gpt-4o")(&cfg)

	if cfg.Model != "gpt-4o" {
		t.Errorf("unexpected Model: %q", cfg.Model)
	}
}

func TestThreadOption_WithModelProvider(t *testing.T) {
	cfg := defaultThreadConfig()
	WithModelProvider("openai")(&cfg)

	if cfg.ModelProvider != "openai" {
		t.Errorf("unexpected ModelProvider: %q", cfg.ModelProvider)
	}
}

func TestThreadOption_WithProfile(t *testing.T) {
	cfg := defaultThreadConfig()
	WithProfile("coding")(&cfg)

	if cfg.Profile != "coding" {
		t.Errorf("unexpected Profile: %q", cfg.Profile)
	}
}

func TestThreadOption_WithWorkDir(t *testing.T) {
	cfg := defaultThreadConfig()
	WithWorkDir("/home/user/project")(&cfg)

	if cfg.WorkDir != "/home/user/project" {
		t.Errorf("unexpected WorkDir: %q", cfg.WorkDir)
	}
}

func TestThreadOption_WithApprovalPolicy(t *testing.T) {
	cfg := defaultThreadConfig()
	WithApprovalPolicy(ApprovalPolicyFullAuto)(&cfg)

	if cfg.ApprovalPolicy != ApprovalPolicyFullAuto {
		t.Errorf("unexpected ApprovalPolicy: %v", cfg.ApprovalPolicy)
	}
}

func TestThreadOption_WithSandbox(t *testing.T) {
	cfg := defaultThreadConfig()
	sandbox := &SandboxConfig{
		Type:          "docker",
		WritableRoots: []string{"/tmp"},
		NetworkAccess: false,
	}
	WithSandbox(sandbox)(&cfg)

	if cfg.Sandbox != sandbox {
		t.Error("Sandbox not set correctly")
	}
	if cfg.Sandbox.Type != "docker" {
		t.Errorf("unexpected Sandbox.Type: %q", cfg.Sandbox.Type)
	}
}

func TestThreadOption_WithThreadConfig(t *testing.T) {
	cfg := defaultThreadConfig()
	extraConfig := map[string]interface{}{
		"key1": "value1",
		"key2": 42,
	}
	WithThreadConfig(extraConfig)(&cfg)

	if cfg.Config == nil {
		t.Error("Config should be set")
	}
	if cfg.Config["key1"] != "value1" {
		t.Errorf("unexpected Config[key1]: %v", cfg.Config["key1"])
	}
}

func TestDefaultTurnConfig(t *testing.T) {
	cfg := defaultTurnConfig()

	// Turn config starts empty
	if cfg.Model != "" {
		t.Error("Model should be empty by default")
	}
	if cfg.ApprovalPolicy != "" {
		t.Error("ApprovalPolicy should be empty by default")
	}
}

func TestTurnOption_WithTurnApprovalPolicy(t *testing.T) {
	cfg := defaultTurnConfig()
	WithTurnApprovalPolicy(ApprovalPolicyAutoEdit)(&cfg)

	if cfg.ApprovalPolicy != ApprovalPolicyAutoEdit {
		t.Errorf("unexpected ApprovalPolicy: %v", cfg.ApprovalPolicy)
	}
}

func TestTurnOption_WithTurnModel(t *testing.T) {
	cfg := defaultTurnConfig()
	WithTurnModel("o4-mini")(&cfg)

	if cfg.Model != "o4-mini" {
		t.Errorf("unexpected Model: %q", cfg.Model)
	}
}

func TestTurnOption_WithEffort(t *testing.T) {
	cfg := defaultTurnConfig()
	WithEffort("high")(&cfg)

	if cfg.Effort != "high" {
		t.Errorf("unexpected Effort: %q", cfg.Effort)
	}
}

func TestTurnOption_WithSummary(t *testing.T) {
	cfg := defaultTurnConfig()
	WithSummary("Context for this turn")(&cfg)

	if cfg.Summary != "Context for this turn" {
		t.Errorf("unexpected Summary: %q", cfg.Summary)
	}
}

func TestTurnOption_WithOutputSchema(t *testing.T) {
	cfg := defaultTurnConfig()
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"answer": map[string]interface{}{"type": "string"},
		},
	}
	WithOutputSchema(schema)(&cfg)

	if cfg.OutputSchema == nil {
		t.Error("OutputSchema should be set")
	}
}

func TestNewClient_WithOptions(t *testing.T) {
	handler := ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		return &ApprovalResponse{Approved: true}, nil
	})

	client := NewClient(
		WithClientName("test-client"),
		WithClientVersion("1.0.0"),
		WithEventBufferSize(50),
		WithApprovalHandler(handler),
	)

	if client == nil {
		t.Fatal("NewClient should return a client")
	}
	if client.config.ClientName != "test-client" {
		t.Errorf("unexpected ClientName: %q", client.config.ClientName)
	}
	if client.config.ClientVersion != "1.0.0" {
		t.Errorf("unexpected ClientVersion: %q", client.config.ClientVersion)
	}
	if client.config.EventBufferSize != 50 {
		t.Errorf("unexpected EventBufferSize: %d", client.config.EventBufferSize)
	}
}
