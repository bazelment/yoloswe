package reviewer

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Each registry is a JSON file of prompt fragments grouped by two organizing
// keys — type then topic — so a review prompt can be maintained as a matrix of
// small, independently editable blocks instead of one monolithic string. Edit
// the JSON to change the guidance; reviewer.go assembles it at init via
// assembleGuidance.
//
//go:embed prompts/code_base.json
var codeBaseJSON []byte

//go:embed prompts/design_doc.json
var designDocJSON []byte

// promptRegistry maps type -> topic -> ordered prompt fragments.
type promptRegistry map[string]map[string][]string

// promptTypeOrder is the canonical type ordering, shared across registries.
// Keys not listed render after the known ones, alphabetically, so an
// unrecognized tag stays visible rather than being silently dropped.
var promptTypeOrder = []string{"principle", "expertise"}

// promptTypeTitle renders a type key as a section heading.
var promptTypeTitle = map[string]string{
	"principle": "Principles",
	"expertise": "Expertise",
}

// guidanceLayout configures how one registry's topics are ordered and titled.
// The type axis is shared (promptTypeOrder/promptTypeTitle); only the topic
// axis differs between review modes.
type guidanceLayout struct {
	topicOrder []string
	topicTitle map[string]string
}

var codeBaseLayout = guidanceLayout{
	topicOrder: []string{"design", "functionality", "test", "efficiency"},
	topicTitle: map[string]string{
		"readability": "Readability",
		"design":      "Design",
		"function":    "Function",
		"test":        "Test",
		"efficiency":  "Efficiency",
	},
}

var designDocLayout = guidanceLayout{
	topicOrder: []string{"substance", "citation", "scope"},
	topicTitle: map[string]string{
		"substance": "Substance",
		"citation":  "Citation",
		"scope":     "Scope",
	},
}

// reviewGuidance and designDocGuidance are the assembled guidance sections,
// built once from their embedded registries.
var (
	reviewGuidance    = assembleGuidance(codeBaseJSON, codeBaseLayout)
	designDocGuidance = assembleGuidance(designDocJSON, designDocLayout)
)

// reviewTopics and designDocTopics list the topics that have guidance, in
// canonical order. They drive each mode's "grade on the following topics" line
// so that sentence stays in sync with the registry.
var (
	reviewTopics    = registryTopics(codeBaseJSON, codeBaseLayout)
	designDocTopics = registryTopics(designDocJSON, designDocLayout)
)

// registryTopics returns the topic keys present in the registry, ordered by
// the layout (unknown topics follow, alphabetically).
func registryTopics(raw []byte, layout guidanceLayout) []string {
	var reg promptRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		panic(fmt.Sprintf("reviewer: parse prompt registry: %v", err))
	}
	present := make(map[string]bool)
	for _, topics := range reg {
		for topic := range topics {
			present[topic] = true
		}
	}
	return orderedKeys(present, layout.topicOrder)
}

// assembleGuidance parses the prompt registry and renders its fragments grouped
// by type, then topic, in canonical order, with a markdown heading per group.
//
// It panics on malformed JSON: the registry is embedded at build time, so a
// parse failure is an authoring bug that should fail the build, not a runtime
// condition to be tolerated.
func assembleGuidance(raw []byte, layout guidanceLayout) string {
	var reg promptRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		panic(fmt.Sprintf("reviewer: parse prompt registry: %v", err))
	}

	var b strings.Builder
	for _, typ := range orderedKeys(reg, promptTypeOrder) {
		topics := reg[typ]
		if len(topics) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", titleOr(promptTypeTitle, typ))
		for _, topic := range orderedKeys(topics, layout.topicOrder) {
			fragments := topics[topic]
			if len(fragments) == 0 {
				continue
			}
			fmt.Fprintf(&b, "### %s\n\n", titleOr(layout.topicTitle, topic))
			for _, fragment := range fragments {
				b.WriteString(strings.TrimSpace(fragment))
				b.WriteString("\n\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// orderedKeys returns the keys of m sorted by their position in order; keys not
// in order follow, alphabetically.
func orderedKeys[V any](m map[string]V, order []string) []string {
	seen := make(map[string]bool, len(m))
	out := make([]string, 0, len(m))
	for _, k := range order {
		if _, ok := m[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	extra := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// titleOr returns the heading for key k, falling back to k itself.
func titleOr(titles map[string]string, k string) string {
	if t := titles[k]; t != "" {
		return t
	}
	return k
}
