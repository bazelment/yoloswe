package app

import "testing"

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
