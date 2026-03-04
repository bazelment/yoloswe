package session

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// GenerateTmuxWindowName creates a tmux window name in the format "repo/worktree:N"
// where N is the lowest available per-worktree index. If repo or worktree info is
// unavailable, falls back to random two-word names.
func GenerateTmuxWindowName(repoName, worktreeName string) string {
	if repoName == "" || worktreeName == "" {
		return generateRandomTmuxWindowName()
	}

	prefix := repoName + "/" + worktreeName + ":"

	// Scan existing tmux windows for ones matching our prefix.
	// Ignore errors: if listing fails we fall back to the TmuxWindowExists check below.
	windows, _ := ListTmuxWindows()
	used := make(map[int]bool)
	for _, w := range windows {
		if strings.HasPrefix(w, prefix) {
			suffix := w[len(prefix):]
			var idx int
			if _, err := fmt.Sscanf(suffix, "%d", &idx); err == nil {
				used[idx] = true
			}
		}
	}

	// Find the lowest unused index
	for i := 0; ; i++ {
		if !used[i] {
			name := fmt.Sprintf("%s%d", prefix, i)
			// Double-check it doesn't collide with an existing window
			if !TmuxWindowExists(name) {
				return name
			}
			// Mark as used and keep searching
			used[i] = true
		}
	}
}

// generateRandomTmuxWindowName creates a random two-word window name as fallback.
// It checks for uniqueness against existing tmux windows and retries up to maxAttempts times.
// Format: "{adjective}-{noun}" (e.g., "happy-tiger")
func generateRandomTmuxWindowName() string {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		name := randomTwoWordName()
		if !TmuxWindowExists(name) {
			return name
		}
	}

	// If all attempts fail, append a random suffix
	name := randomTwoWordName()
	suffix := randomHex(4)
	return fmt.Sprintf("%s-%s", name, suffix)
}

// Word lists for generating two-word session names.
var (
	adjectives = []string{
		"happy", "wise", "calm", "brave", "bright", "clever", "gentle", "kind",
		"swift", "quiet", "bold", "eager", "fair", "proud", "loyal", "noble",
		"warm", "cool", "steady", "lively", "sharp", "smooth", "strong", "light",
		"quick", "agile", "grand", "pure", "free", "fresh", "keen", "wild",
		"mild", "soft", "hard", "clear", "deep", "high", "dark", "pale",
		"rich", "rare", "fine", "good", "safe", "true", "vast", "vivid",
		"young", "old", "new", "great",
	}

	nouns = []string{
		"tiger", "ocean", "river", "mountain", "forest", "desert", "valley", "canyon",
		"eagle", "wolf", "bear", "hawk", "fox", "lion", "dragon", "falcon",
		"stone", "cloud", "storm", "wind", "rain", "snow", "fire", "ice",
		"star", "moon", "sun", "sky", "dawn", "dusk", "night", "day",
		"tree", "leaf", "branch", "root", "seed", "bloom", "pine", "oak",
		"wave", "tide", "reef", "shore", "peak", "cliff", "cave", "lake",
		"trail", "path", "bridge", "gate",
	}
)

// randomTwoWordName generates a random two-word name from the word lists.
func randomTwoWordName() string {
	adj := adjectives[randomInt(len(adjectives))]
	noun := nouns[randomInt(len(nouns))]
	return fmt.Sprintf("%s-%s", adj, noun)
}

// randomInt returns a random integer in [0, n).
func randomInt(n int) int {
	nBig, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// Fallback to a simple deterministic value if crypto/rand fails
		return 0
	}
	return int(nBig.Int64())
}

// randomHex generates a random hex string of the specified length.
func randomHex(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "0000"
	}
	return fmt.Sprintf("%x", bytes)[:length]
}
