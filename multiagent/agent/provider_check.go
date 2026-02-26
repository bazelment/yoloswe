package agent

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Provider name constants for agent backends.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
	ProviderGemini = "gemini"
	ProviderCursor = "cursor"
)

// AllProviders is the ordered list of known provider names.
var AllProviders = []string{ProviderClaude, ProviderCodex, ProviderGemini, ProviderCursor}

// providerBinaries maps provider names to their CLI binary names.
var providerBinaries = map[string]string{
	ProviderClaude: "claude",
	ProviderCodex:  "codex",
	ProviderGemini: "gemini",
	ProviderCursor: "agent",
}

// ProviderStatus describes the availability of a single provider CLI.
type ProviderStatus struct {
	Provider  string
	Version   string
	Error     string
	Installed bool
}

// ProviderAvailability holds cached CLI availability information.
type ProviderAvailability struct {
	statuses map[string]ProviderStatus
}

// NewProviderAvailability probes all known provider CLIs in parallel
// and returns their availability status.
func NewProviderAvailability() *ProviderAvailability {
	statuses := make(map[string]ProviderStatus, len(AllProviders))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, provider := range AllProviders {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			status := checkProvider(name)
			mu.Lock()
			statuses[name] = status
			mu.Unlock()
		}(provider)
	}

	wg.Wait()
	return &ProviderAvailability{statuses: statuses}
}

// IsInstalled returns true if the provider's CLI binary is in PATH.
func (pa *ProviderAvailability) IsInstalled(provider string) bool {
	if s, ok := pa.statuses[provider]; ok {
		return s.Installed
	}
	return false
}

// Status returns the full status for a provider. Returns a zero-value status
// with Installed=false if the provider is unknown.
func (pa *ProviderAvailability) Status(provider string) ProviderStatus {
	if s, ok := pa.statuses[provider]; ok {
		return s
	}
	return ProviderStatus{Provider: provider}
}

// AllStatuses returns statuses for all known providers in order.
func (pa *ProviderAvailability) AllStatuses() []ProviderStatus {
	result := make([]ProviderStatus, 0, len(AllProviders))
	for _, name := range AllProviders {
		result = append(result, pa.Status(name))
	}
	return result
}

// InstalledProviders returns the names of providers whose CLI is installed.
func (pa *ProviderAvailability) InstalledProviders() []string {
	var result []string
	for _, name := range AllProviders {
		if pa.IsInstalled(name) {
			result = append(result, name)
		}
	}
	return result
}

// checkProvider probes a single provider binary.
func checkProvider(provider string) ProviderStatus {
	binary, ok := providerBinaries[provider]
	if !ok {
		return ProviderStatus{Provider: provider, Error: "unknown provider"}
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return ProviderStatus{Provider: provider, Error: "not found in PATH"}
	}

	// Try to get version; non-fatal if it fails.
	version := getVersion(path)

	return ProviderStatus{
		Provider:  provider,
		Installed: true,
		Version:   version,
	}
}

// versionTimeout is the maximum time to wait for a `--version` probe.
const versionTimeout = 5 * time.Second

// getVersion runs `<binary> --version` and returns the first line of stdout.
// Stderr is discarded to avoid capturing noise like Node.js deprecation warnings.
// Returns empty string if it fails or times out.
func getVersion(binaryPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	// Discard stderr — tools like gemini emit Node.js deprecation warnings there.
	if err := cmd.Run(); err != nil {
		return ""
	}
	firstLine := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	return strings.TrimSpace(firstLine)
}
