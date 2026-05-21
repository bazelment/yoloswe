package app

import (
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestSettingsEnabledProvidersNormalizesLegacyGemini(t *testing.T) {
	t.Parallel()

	raw := []string{agent.ProviderGemini, agent.ProviderAgy, "  Gemini  "}
	s := Settings{EnabledProviders: &raw}

	got := s.GetEnabledProviders()
	if len(got) != 1 || got[0] != agent.ProviderAgy {
		t.Fatalf("GetEnabledProviders() = %v, want [%s]", got, agent.ProviderAgy)
	}
	if !s.IsProviderEnabled(agent.ProviderAgy) {
		t.Fatalf("IsProviderEnabled(%q) = false, want true", agent.ProviderAgy)
	}
	if !s.IsProviderEnabled(agent.ProviderGemini) {
		t.Fatalf("IsProviderEnabled(%q) = false, want true", agent.ProviderGemini)
	}
}

func TestSettingsSetEnabledProvidersNormalizesLegacyGemini(t *testing.T) {
	t.Parallel()

	var s Settings
	s.SetEnabledProviders([]string{agent.ProviderClaude, agent.ProviderGemini, agent.ProviderAgy})

	if s.EnabledProviders == nil {
		t.Fatal("EnabledProviders = nil, want normalized explicit list")
	}
	want := []string{agent.ProviderClaude, agent.ProviderAgy}
	if got := *s.EnabledProviders; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("EnabledProviders = %v, want %v", got, want)
	}
}

func TestSettingsSetRepoSettingsNormalizesValues(t *testing.T) {
	var s Settings

	s.SetRepoSettings("my-repo", RepoSettings{
		OnWorktreeCreate: []string{"  npm ci  ", "", "go test ./..."},
		OnWorktreeDelete: []string{" ", "rm -rf .cache"},
	})

	got := s.RepoSettingsFor("my-repo")
	if len(got.OnWorktreeCreate) != 2 {
		t.Fatalf("len(OnWorktreeCreate) = %d, want 2", len(got.OnWorktreeCreate))
	}
	if got.OnWorktreeCreate[0] != "npm ci" || got.OnWorktreeCreate[1] != "go test ./..." {
		t.Fatalf("OnWorktreeCreate = %v, want [npm ci go test ./...]", got.OnWorktreeCreate)
	}
	if len(got.OnWorktreeDelete) != 1 || got.OnWorktreeDelete[0] != "rm -rf .cache" {
		t.Fatalf("OnWorktreeDelete = %v, want [rm -rf .cache]", got.OnWorktreeDelete)
	}
}

func TestSettingsSetRepoSettingsRemovesEmptyRepoConfig(t *testing.T) {
	s := Settings{
		Repos: map[string]RepoSettings{
			"my-repo": {
				OnWorktreeCreate: []string{"npm ci"},
			},
		},
	}

	s.SetRepoSettings("my-repo", RepoSettings{})

	if got := s.RepoSettingsFor("my-repo"); len(got.OnWorktreeCreate) != 0 || len(got.OnWorktreeDelete) != 0 {
		t.Fatalf("RepoSettingsFor(my-repo) = %+v, want empty", got)
	}
	if s.Repos != nil {
		t.Fatalf("Repos map should be nil after removing last repo, got %+v", s.Repos)
	}
}
