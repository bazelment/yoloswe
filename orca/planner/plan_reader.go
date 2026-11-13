package planner

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/orca/store"
)

// Source kinds for a resolved PlanDocument.
const (
	PlanSourceFile       = "file"
	PlanSourceNebulaPost = "nebula_post"
	DefaultMaxPlanBytes  = 1 << 20
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
	MaxBytes    int
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
		data, err := r.readPlanFile(target)
		if err != nil {
			return PlanDocument{}, err
		}
		return buildPlanDocument(trimmed, PlanSourceFile, target, string(data)), nil
	}

	if strings.HasPrefix(target, "-") {
		return PlanDocument{}, fmt.Errorf("%w: invalid nebula post target %q", store.ErrInvalidArgument, target)
	}

	if r != nil && r.FetchPost != nil {
		content, err := r.FetchPost(ctx, target)
		if err != nil {
			return PlanDocument{}, fmt.Errorf("orca/planner: fetch nebula post %q: %w", target, err)
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
	cmd := exec.CommandContext(ctx, bin, "post", "get", "--", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return PlanDocument{}, fmt.Errorf("orca/planner: fetch nebula post %q: %w (%s)", target, err, strings.TrimSpace(string(out)))
	}
	return buildPlanDocument(trimmed, PlanSourceNebulaPost, target, string(out)), nil
}

func (r *DefaultPlanReader) readPlanFile(target string) ([]byte, error) {
	resolved := filepath.Clean(target)
	if r != nil && strings.TrimSpace(r.BaseDir) != "" {
		baseAbs, err := filepath.Abs(r.BaseDir)
		if err != nil {
			return nil, fmt.Errorf("orca/planner: resolve base dir %q: %w", r.BaseDir, err)
		}
		candidate := resolved
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(baseAbs, candidate)
		}
		candAbs, err := filepath.Abs(candidate)
		if err != nil {
			return nil, fmt.Errorf("orca/planner: resolve plan file %q: %w", target, err)
		}
		rel, err := filepath.Rel(baseAbs, candAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: plan path %q escapes base directory %q", store.ErrInvalidArgument, target, r.BaseDir)
		}
		resolved = candAbs
	}

	maxBytes := DefaultMaxPlanBytes
	if r != nil && r.MaxBytes > 0 {
		maxBytes = r.MaxBytes
	}

	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("orca/planner: read plan file %q: %w", target, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("orca/planner: read plan file %q: %w", target, err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("%w: plan file %q exceeds max size (%d bytes)", store.ErrInvalidArgument, target, maxBytes)
	}
	return data, nil
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
