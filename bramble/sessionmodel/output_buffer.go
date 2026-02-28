package sessionmodel

import (
	"strings"
	"sync"
	"time"
)

// OutputBuffer is a thread-safe, capped ring buffer of OutputLine values.
// It extracts the output storage logic previously spread across Manager
// methods (addOutput, appendOrAddOutput, updateToolOutput).
type OutputBuffer struct {
	lines []OutputLine
	max   int
	mu    sync.RWMutex
}

// NewOutputBuffer creates a buffer that keeps at most max lines.
func NewOutputBuffer(max int) *OutputBuffer {
	return &OutputBuffer{
		max: max,
	}
}

// Append adds a new output line, evicting the oldest if at capacity.
func (b *OutputBuffer) Append(line OutputLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) >= b.max {
		// Zero the slot before reslicing so the GC can reclaim pointer-bearing
		// fields (ToolResult, ToolInput) in the evicted element.
		b.lines[0] = OutputLine{}
		b.lines = append(b.lines[1:], line)
	} else {
		b.lines = append(b.lines, line)
	}
}

// AppendStreamingText appends a text delta to the last text line, or creates
// a new text line if the last line is not text. Plain concatenation is used
// because live streaming deltas are non-overlapping token chunks.
func (b *OutputBuffer) AppendStreamingText(delta string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) > 0 && b.lines[len(b.lines)-1].Type == OutputTypeText {
		b.lines[len(b.lines)-1].Content += delta
		return
	}
	b.appendLocked(OutputLine{
		Timestamp: time.Now(),
		Type:      OutputTypeText,
		Content:   delta,
	})
}

// AppendStreamingThinking appends a thinking delta to the last thinking line,
// or creates a new thinking line.
func (b *OutputBuffer) AppendStreamingThinking(delta string) {
	if strings.TrimSpace(delta) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) > 0 && b.lines[len(b.lines)-1].Type == OutputTypeThinking {
		b.lines[len(b.lines)-1].Content += delta
		return
	}
	b.appendLocked(OutputLine{
		Timestamp: time.Now(),
		Type:      OutputTypeThinking,
		Content:   delta,
	})
}

// UpdateToolByID finds the most recent tool_start line with the given toolID,
// applies fn via copy-on-write, and returns true if found.
func (b *OutputBuffer) UpdateToolByID(toolID string, fn func(*OutputLine)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.lines) - 1; i >= 0; i-- {
		if b.lines[i].ToolID == toolID && b.lines[i].Type == OutputTypeToolStart {
			lineCopy := b.lines[i]
			// Deep-copy mutable map fields before mutation.
			if lineCopy.ToolInput != nil {
				newInput := make(map[string]interface{}, len(lineCopy.ToolInput))
				for k, v := range lineCopy.ToolInput {
					newInput[k] = v
				}
				lineCopy.ToolInput = newInput
			}
			fn(&lineCopy)
			b.lines[i] = lineCopy
			return true
		}
	}
	return false
}

// Snapshot returns a deep-copied slice of all lines.
func (b *OutputBuffer) Snapshot() []OutputLine {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]OutputLine, len(b.lines))
	for i := range b.lines {
		result[i] = DeepCopyOutputLine(b.lines[i])
	}
	return result
}

// Len returns the current number of lines.
func (b *OutputBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.lines)
}

// appendLocked appends a line while already holding b.mu.
func (b *OutputBuffer) appendLocked(line OutputLine) {
	if len(b.lines) >= b.max {
		// Zero the slot before reslicing so the GC can reclaim pointer-bearing
		// fields (ToolResult, ToolInput) in the evicted element.
		b.lines[0] = OutputLine{}
		b.lines = append(b.lines[1:], line)
	} else {
		b.lines = append(b.lines, line)
	}
}
