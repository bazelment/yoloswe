package model

import (
	"fmt"
	"strings"
	"unicode"
)

// toolNameCodec maps canonical names to the restricted model wire names.
type toolNameCodec struct {
	wireToOriginal map[string]string
}

func newToolNameCodec(req Request, maxNameLength int) (*toolNameCodec, error) {
	c := &toolNameCodec{wireToOriginal: make(map[string]string)}
	for _, tool := range req.Tools {
		if err := c.add(tool.Name, maxNameLength); err != nil {
			return nil, err
		}
	}
	for _, message := range req.Messages {
		for i := range message.Content {
			block := message.Content[i]
			if block.Type == BlockTypeToolUse {
				if err := c.add(block.Name, maxNameLength); err != nil {
					return nil, err
				}
			}
		}
	}
	return c, nil
}

func (c *toolNameCodec) add(original string, maxNameLength int) error {
	wire := encodeToolName(original)
	if len(wire) < 1 || len(wire) > maxNameLength {
		return fmt.Errorf("orca/model: tool name length for %q must be 1-%d encoded characters (got %d)", original, maxNameLength, len(wire))
	}
	if previous, ok := c.wireToOriginal[wire]; ok && previous != original {
		return fmt.Errorf("orca/model: tool name collision: %q and %q both encode to %q", previous, original, wire)
	}
	c.wireToOriginal[wire] = original
	return nil
}

func encodeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r <= unicode.MaxASCII && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (c *toolNameCodec) wire(name string) string {
	return encodeToolName(name)
}

func (c *toolNameCodec) original(name string) string {
	if original, ok := c.wireToOriginal[name]; ok {
		return original
	}
	return name
}
