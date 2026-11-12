package planner

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/orch/store"
)

// Source kinds for a resolved PlanDocument.
const (
	PlanSourceFile       = "file"
	PlanSourceNebulaPost = "nebula_post"
)

// PlanDocument is the resolved text of a plan referenced by file path or Nebula post ID.
type PlanDocument struct {
	Ref        string `json:"ref"`
	SourceKind string `json:"source_kind"`
	Target     string `json:"target"`
	Title      string `json:"title,omitempty"`
	Content    string `json:"content"`
}

// PlanReader resolves a plan reference (file path or Nebula post ID) into a PlanDocument.
type PlanReader interface {
	ReadPlan(ctx context.Context, ref string) (PlanDocument, error)
}

// PlanReaderFunc adapts a function to PlanReader.
type PlanReaderFunc func(ctx context.Context, ref string) (PlanDocument, error)

func (f PlanReaderFunc) ReadPlan(ctx context.Context, ref string) (PlanDocument, error) {
	return f(ctx, ref)
}

// PostFetcher fetches the Markdown body of a Nebula post by its post ID.
type PostFetcher func(ctx context.Context, postID string) (string, error)

// DefaultPlanReader reads plans from local files, in-memory post/plan maps, or Nebula posts.
type DefaultPlanReader struct {
	Posts       map[string]string
	FetchPost   PostFetcher
	NebulaBin   string
	BaseDir     string
	DisableExec bool
}

// NewPlanReader constructs a DefaultPlanReader with optional pre-seeded post contents.
func NewPlanReader(posts map[string]string) *DefaultPlanReader {
	return &DefaultPlanReader{
		Posts: maps.Clone(posts),
	}
}

// ParsePlanRef classifies ref as either a Nebula post ID (PlanSourceNebulaPost)
// or a file path (PlanSourceFile) and returns the normalized target identifier.
func ParsePlanRef(ref string) (sourceKind, target string) {
	trimmed := strings.TrimSpace(ref)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"post:", "nebula://post/", "nebula:"} {
		if strings.HasPrefix(lower, prefix) {
			return PlanSourceNebulaPost, strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	if strings.HasPrefix(lower, "file://") {
		return PlanSourceFile, strings.TrimSpace(trimmed[len("file://"):])
	}
	if looksLikeUUID(trimmed) {
		return PlanSourceNebulaPost, trimmed
	}
	return PlanSourceFile, trimmed
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// ReadPlan resolves ref from a file path or Nebula post ID.
func (r *DefaultPlanReader) ReadPlan(ctx context.Context, ref string) (PlanDocument, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return PlanDocument{}, fmt.Errorf("%w: plan reference is required", store.ErrInvalidArgument)
	}

	sourceKind, target := ParsePlanRef(trimmed)
	if target == "" {
		return PlanDocument{}, fmt.Errorf("%w: empty target in plan reference %q", store.ErrInvalidArgument, ref)
	}

	if r != nil && len(r.Posts) > 0 {
		if content, ok := r.Posts[trimmed]; ok {
			return buildPlanDocument(trimmed, sourceKind, target, content), nil
		}
		if content, ok := r.Posts[target]; ok {
			return buildPlanDocument(trimmed, PlanSourceNebulaPost, target, content), nil
		}
	}

	if sourceKind == PlanSourceFile {
		data, err := os.ReadFile(target)
		if err != nil && r != nil && r.BaseDir != "" && !filepath.IsAbs(target) {
			if altData, altErr := os.ReadFile(filepath.Join(r.BaseDir, target)); altErr == nil {
				data, err = altData, nil
			}
		}
		if err != nil {
			return PlanDocument{}, fmt.Errorf("orch/planner: read plan file %q: %w", target, err)
		}
		return buildPlanDocument(trimmed, PlanSourceFile, target, string(data)), nil
	}

	if data, err := os.ReadFile(target); err == nil {
		return buildPlanDocument(trimmed, PlanSourceFile, target, string(data)), nil
	}

	if r != nil && r.FetchPost != nil {
		content, err := r.FetchPost(ctx, target)
		if err != nil {
			return PlanDocument{}, fmt.Errorf("orch/planner: fetch nebula post %q: %w", target, err)
		}
		return buildPlanDocument(trimmed, PlanSourceNebulaPost, target, content), nil
	}

	if r != nil && r.DisableExec {
		return PlanDocument{}, fmt.Errorf("%w: nebula post %q not found in configured posts", store.ErrNotFound, target)
	}

	bin := "nebula"
	if r != nil && strings.TrimSpace(r.NebulaBin) != "" {
		bin = strings.TrimSpace(r.NebulaBin)
	}
	cmd := exec.CommandContext(ctx, bin, "post", "get", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return PlanDocument{}, fmt.Errorf("orch/planner: fetch nebula post %q: %w (%s)", target, err, strings.TrimSpace(string(out)))
	}
	return buildPlanDocument(trimmed, PlanSourceNebulaPost, target, string(out)), nil
}

func buildPlanDocument(ref, sourceKind, target, content string) PlanDocument {
	trimmedContent := strings.TrimSpace(content)
	return PlanDocument{
		Ref:        ref,
		SourceKind: sourceKind,
		Target:     target,
		Title:      extractMarkdownTitle(trimmedContent),
		Content:    trimmedContent,
	}
}

func extractMarkdownTitle(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			title := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if title != "" {
				return title
			}
		}
	}
	return ""
}
