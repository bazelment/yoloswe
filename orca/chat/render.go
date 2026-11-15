package chat

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

const (
	replyPrefix      = "orca> "
	replyIndent      = "      "
	userPrefix       = "you> "
	userIndent       = "     "
	defaultMaxTurns  = 3
	defaultTermWidth = 80
)

type spanStyle uint8

const (
	styleNone spanStyle = 0
	styleBold spanStyle = 1 << iota
	styleCode
	styleUnderline
	styleDim
)

type styledSpan struct {
	text  string
	style spanStyle
}

// FormatReply renders an assistant markdown reply into terminal lines with
// word-boundary wrapping and continuation indent under "orca> ".
// When tty is false, markdown syntax is stripped to plain text without ANSI escapes.
func FormatReply(text string, width int, tty bool) []string {
	text = strings.TrimPrefix(text, replyPrefix)
	text = expandTabs(text)
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return []string{replyPrefix}
	}

	bodyWidth := 0
	if width > 0 {
		total := effectiveWidth(width)
		bodyWidth = max(total-len(replyPrefix), 10)
		if total < len(replyPrefix)+4 {
			bodyWidth = max(total, 1)
		}
	}

	rawLines := strings.Split(text, "\n")
	var bodyLines []string
	inFence := false
	lastBlank := false

	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			code := strings.TrimRight(raw, " \t\r")
			codeWidth := 0
			if bodyWidth > 2 {
				codeWidth = bodyWidth - 2
			}
			wrapped := wrapSpans([]styledSpan{{text: code, style: styleCode}}, codeWidth, tty)
			for _, w := range wrapped {
				bodyLines = append(bodyLines, "  "+w)
			}
			lastBlank = false
			continue
		}

		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" {
			if !lastBlank && len(bodyLines) > 0 {
				bodyLines = append(bodyLines, "")
				lastBlank = true
			}
			continue
		}
		lastBlank = false

		if heading, ok := cutHeading(trimmed); ok {
			spans := parseInline(heading, styleBold)
			bodyLines = append(bodyLines, wrapSpans(spans, bodyWidth, tty)...)
			continue
		}

		if lead, rest, ok := cutBullet(line); ok {
			bullet := "• "
			if !tty {
				bullet = "- "
			}
			marker := lead + bullet
			markerWidth := runewidth.StringWidth(marker)
			itemWidth := 0
			if bodyWidth > 0 {
				itemWidth = max(bodyWidth-markerWidth, 8)
			}
			spans := parseInline(rest, styleNone)
			wrapped := wrapSpans(spans, itemWidth, tty)
			hanging := strings.Repeat(" ", markerWidth)
			for i, w := range wrapped {
				if i == 0 {
					bodyLines = append(bodyLines, marker+w)
				} else {
					bodyLines = append(bodyLines, hanging+w)
				}
			}
			continue
		}

		if marker, rest, ok := cutNumbered(line); ok {
			markerWidth := runewidth.StringWidth(marker)
			itemWidth := 0
			if bodyWidth > 0 {
				itemWidth = max(bodyWidth-markerWidth, 8)
			}
			spans := parseInline(rest, styleNone)
			wrapped := wrapSpans(spans, itemWidth, tty)
			hanging := strings.Repeat(" ", markerWidth)
			for i, w := range wrapped {
				if i == 0 {
					bodyLines = append(bodyLines, marker+w)
				} else {
					bodyLines = append(bodyLines, hanging+w)
				}
			}
			continue
		}

		spans := parseInline(trimmed, styleNone)
		bodyLines = append(bodyLines, wrapSpans(spans, bodyWidth, tty)...)
	}

	for len(bodyLines) > 0 && bodyLines[len(bodyLines)-1] == "" {
		bodyLines = bodyLines[:len(bodyLines)-1]
	}
	if len(bodyLines) == 0 {
		return []string{replyPrefix}
	}

	indentContinuation := tty || width > 0
	out := make([]string, len(bodyLines))
	for i, b := range bodyLines {
		switch {
		case i == 0:
			out[i] = replyPrefix + b
		case b == "":
			out[i] = ""
		case indentContinuation:
			out[i] = replyIndent + b
		default:
			out[i] = b
		}
	}
	return out
}

// FormatUserLine renders an owner prompt line ("you> ...") with word-boundary
// wrapping and bold styling on a TTY.
func FormatUserLine(text string, width int, tty bool) []string {
	text = strings.TrimPrefix(text, userPrefix)
	text = expandTabs(strings.TrimSpace(text))
	bodyWidth := 0
	if width > 0 {
		total := effectiveWidth(width)
		bodyWidth = max(total-len(userPrefix), 10)
	}
	wrapped := wrapSpans([]styledSpan{{text: text, style: styleNone}}, bodyWidth, false)
	out := make([]string, len(wrapped))
	for i, line := range wrapped {
		prefix := userPrefix
		if i > 0 {
			prefix = userIndent
		}
		row := prefix + line
		if tty {
			row = renderStyledLine(row, styleBold, 0, true)
		}
		out[i] = row
	}
	return out
}

// FormatRouteLine renders a routing echo line ("→ concierge: ...") as a dim
// line on a TTY or plain text when piped.
func FormatRouteLine(handler, text string, fallback bool, width int, tty bool) []string {
	echo := routeEcho(handler, text, fallback)
	return FormatDimLine(echo, width, tty)
}

func routeEcho(handler, text string, fallback bool) string {
	echo := "→ " + handler + ": " + text
	if fallback {
		echo += " (via fallback)"
	}
	return echo
}

// FormatDimLine renders a routing or tool-call line ("→ ...") as a single dim
// line on a TTY or plain text when piped.
func FormatDimLine(text string, width int, tty bool) []string {
	text = expandTabs(strings.TrimSpace(text))
	if !tty {
		if width > 0 {
			return []string{truncateLine(text, width)}
		}
		return []string{text}
	}
	line := "  " + text
	if width > 0 {
		line = truncateLine(line, width)
	}
	return []string{renderStyledLine(line, styleDim, 0, true)}
}

func renderStyledLine(text string, style spanStyle, width int, tty bool) string {
	if width > 0 {
		text = truncateLine(text, width)
	}
	return renderLineSpans([]styledSpan{{text: text, style: style}}, tty)
}

// FormatToolCall summarizes a tool call as a single "→ <name> <target>" line.
func FormatToolCall(name string, rawInput []byte, defaultRunID string) string {
	name = strings.TrimSpace(name)
	var fields map[string]any
	if len(rawInput) > 0 {
		_ = json.Unmarshal(rawInput, &fields)
	}
	strField := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := fields[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	runID := cmp.Or(strField("run_id"), strings.TrimSpace(defaultRunID))
	trace := strField("trace")
	target := ""
	switch {
	case trace != "" && runID != "":
		target = runID + " " + trace
	case trace != "":
		target = trace
	case strField("lane_id", "lane") != "":
		target = strField("lane_id", "lane")
	case strField("ask_id", "id") != "":
		target = strField("ask_id", "id")
	case strField("goal") != "":
		target = strField("goal")
	case strField("action") != "":
		if runID != "" {
			target = runID + " " + strField("action")
		} else {
			target = strField("action")
		}
	case strField("kind") != "":
		target = strField("kind")
	case strField("summary", "text", "answer", "path") != "":
		target = strField("summary", "text", "answer", "path")
	case runID != "":
		target = runID
	}
	if idx := strings.IndexByte(target, '\n'); idx >= 0 {
		target = strings.TrimSpace(target[:idx])
	}
	if target == "" {
		return "→ " + name
	}
	return "→ " + name + " " + target
}

// FormatRestartStatus builds the startup lines after /restart:
// "orca restarted (version a -> b)" and held runs only when there are any.
func FormatRestartStatus(oldVer, newVer string, heldLines []string) []string {
	oldVer = strings.TrimSpace(oldVer)
	newVer = strings.TrimSpace(newVer)
	line := "orca restarted"
	if oldVer != "" || newVer != "" {
		line = fmt.Sprintf("orca restarted (version %s -> %s)", cmp.Or(oldVer, "unknown"), cmp.Or(newVer, "unknown"))
	}
	out := []string{line}
	for _, h := range heldLines {
		if strings.TrimSpace(h) != "" {
			out = append(out, h)
		}
	}
	return out
}

// FormatTranscript renders stored transcript lines for startup display, keeping
// the last maxTurns turns and summarizing older ones as
// "N earlier turns; /log to see them".
func FormatTranscript(lines []string, maxTurns, width int, tty bool) []string {
	if len(lines) == 0 {
		return nil
	}
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	var (
		headerLines []string
		turns       [][]string
		curTurn     []string
		tailLines   []string
	)
	flushTurn := func() {
		if len(curTurn) > 0 {
			turns = append(turns, curTurn)
			curTurn = nil
		}
	}

	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "you> "):
			flushTurn()
			curTurn = append(curTurn, raw)
		case strings.HasPrefix(trimmed, "→ "):
			curTurn = append(curTurn, raw)
		case strings.HasPrefix(trimmed, "orca> "):
			curTurn = append(curTurn, raw)
		case strings.Contains(trimmed, "earlier turns; /log to see them") || strings.HasPrefix(trimmed, "(resumed conversation:"):
			headerLines = append(headerLines, trimmed)
		default:
			if len(curTurn) > 0 && len(turns) == 0 && !strings.HasPrefix(trimmed, "orca restarted") && !strings.HasPrefix(trimmed, "reattached ") {
				curTurn = append(curTurn, raw)
			} else {
				flushTurn()
				tailLines = append(tailLines, raw)
			}
		}
	}
	flushTurn()

	var out []string
	earlier := 0
	if len(turns) > maxTurns {
		earlier = len(turns) - maxTurns
		turns = turns[len(turns)-maxTurns:]
	}
	for _, h := range headerLines {
		if strings.HasPrefix(h, "(resumed conversation:") {
			continue
		}
		if tty {
			out = append(out, renderStyledLine(h, styleDim, 0, true))
		} else {
			out = append(out, h)
		}
	}
	if earlier > 0 && len(headerLines) == 0 {
		summary := fmt.Sprintf("%d earlier turns; /log to see them", earlier)
		if tty {
			summary = renderStyledLine(summary, styleDim, 0, true)
		}
		out = append(out, summary)
	}

	for i, turn := range turns {
		if i > 0 || len(out) > 0 {
			out = append(out, "")
		}
		for _, entry := range turn {
			switch {
			case strings.HasPrefix(entry, "you> "):
				out = append(out, FormatUserLine(entry, width, tty)...)
			case strings.HasPrefix(entry, "→ "):
				out = append(out, FormatDimLine(entry, width, tty)...)
			case strings.HasPrefix(entry, "orca> "):
				out = append(out, FormatReply(entry, width, tty)...)
			default:
				out = append(out, wrapPhysicalLines(entry, width)...)
			}
		}
	}

	if len(tailLines) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		for _, t := range tailLines {
			if tty && (strings.HasPrefix(t, "orca restarted") || strings.HasPrefix(t, "reattached ")) {
				out = append(out, renderStyledLine(t, styleDim, 0, true))
			} else {
				out = append(out, wrapPhysicalLines(t, width)...)
			}
		}
	}
	return out
}

func cutHeading(s string) (string, bool) {
	if !strings.HasPrefix(s, "#") {
		return "", false
	}
	i := 0
	for i < len(s) && i < 6 && s[i] == '#' {
		i++
	}
	if i < len(s) && s[i] == ' ' {
		rest := strings.TrimSpace(s[i+1:])
		if rest != "" {
			return rest, true
		}
	}
	return "", false
}

func cutBullet(line string) (lead, rest string, ok bool) {
	trimmedLeft := strings.TrimLeft(line, " ")
	indent := min(len(line)-len(trimmedLeft), 4)
	for _, prefix := range []string{"- ", "* ", "+ ", "• "} {
		if after, found := strings.CutPrefix(trimmedLeft, prefix); found {
			return strings.Repeat(" ", indent), strings.TrimSpace(after), true
		}
	}
	return "", "", false
}

func cutNumbered(line string) (marker, rest string, ok bool) {
	trimmedLeft := strings.TrimLeft(line, " ")
	indent := min(len(line)-len(trimmedLeft), 4)
	i := 0
	for i < len(trimmedLeft) && trimmedLeft[i] >= '0' && trimmedLeft[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(trimmedLeft) {
		return "", "", false
	}
	if (trimmedLeft[i] == '.' || trimmedLeft[i] == ')') && trimmedLeft[i+1] == ' ' {
		return strings.Repeat(" ", indent) + trimmedLeft[:i+1] + " ", strings.TrimSpace(trimmedLeft[i+2:]), true
	}
	return "", "", false
}

func parseInline(s string, baseStyle spanStyle) []styledSpan {
	var spans []styledSpan
	var plain strings.Builder
	flushPlain := func() {
		if plain.Len() > 0 {
			spans = append(spans, styledSpan{text: plain.String(), style: baseStyle})
			plain.Reset()
		}
	}

	for i := 0; i < len(s); {
		// Inline code: `...`
		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end > 0 {
				flushPlain()
				spans = append(spans, styledSpan{text: s[i+1 : i+1+end], style: baseStyle | styleCode})
				i += end + 2
				continue
			}
		}

		// Markdown link: [text](url)
		if s[i] == '[' {
			if closeBracket := strings.IndexByte(s[i+1:], ']'); closeBracket >= 0 {
				afterBracket := i + 1 + closeBracket + 1
				if afterBracket < len(s) && s[afterBracket] == '(' {
					if closeParen := strings.IndexByte(s[afterBracket+1:], ')'); closeParen > 0 {
						label := strings.TrimSpace(s[i+1 : i+1+closeBracket])
						url := strings.TrimSpace(s[afterBracket+1 : afterBracket+1+closeParen])
						if url != "" && !strings.ContainsAny(url, " \t\n") {
							flushPlain()
							if label != "" && label != url {
								spans = append(spans, parseInline(label, baseStyle)...)
								spans = append(spans,
									styledSpan{text: " (", style: baseStyle},
									styledSpan{text: url, style: baseStyle | styleUnderline},
									styledSpan{text: ")", style: baseStyle},
								)
							} else {
								spans = append(spans, styledSpan{text: url, style: baseStyle | styleUnderline})
							}
							i = afterBracket + 1 + closeParen + 1
							continue
						}
					}
				}
			}
		}

		// Bold: **...**
		if strings.HasPrefix(s[i:], "**") {
			if end := strings.Index(s[i+2:], "**"); end > 0 {
				flushPlain()
				spans = append(spans, parseInline(s[i+2:i+2+end], baseStyle|styleBold)...)
				i += end + 4
				continue
			}
		}

		plain.WriteByte(s[i])
		i++
	}
	flushPlain()
	return spans
}

type styledWord struct {
	parts []styledSpan
	width int
}

func splitWords(spans []styledSpan) []styledWord {
	var words []styledWord
	var cur []styledSpan
	curWidth := 0
	flush := func() {
		if len(cur) > 0 {
			words = append(words, styledWord{parts: cur, width: curWidth})
			cur = nil
			curWidth = 0
		}
	}

	for _, sp := range spans {
		var token strings.Builder
		flushToken := func() {
			if token.Len() > 0 {
				txt := token.String()
				cur = append(cur, styledSpan{text: txt, style: sp.style})
				curWidth += runewidth.StringWidth(txt)
				token.Reset()
			}
		}
		for _, r := range sp.text {
			if r == ' ' || r == '\t' {
				flushToken()
				flush()
			} else {
				token.WriteRune(r)
			}
		}
		flushToken()
	}
	flush()
	return words
}

func breakLongWord(w styledWord, maxWidth int) []styledWord {
	if maxWidth <= 0 || w.width <= maxWidth {
		return []styledWord{w}
	}
	var out []styledWord
	var curParts []styledSpan
	curWidth := 0
	for _, p := range w.parts {
		var buf strings.Builder
		for _, r := range p.text {
			rw := runewidth.RuneWidth(r)
			if curWidth+rw > maxWidth && curWidth > 0 {
				if buf.Len() > 0 {
					curParts = append(curParts, styledSpan{text: buf.String(), style: p.style})
					buf.Reset()
				}
				out = append(out, styledWord{parts: curParts, width: curWidth})
				curParts = nil
				curWidth = 0
			}
			buf.WriteRune(r)
			curWidth += rw
		}
		if buf.Len() > 0 {
			curParts = append(curParts, styledSpan{text: buf.String(), style: p.style})
		}
	}
	if len(curParts) > 0 {
		out = append(out, styledWord{parts: curParts, width: curWidth})
	}
	return out
}

func wrapSpans(spans []styledSpan, maxWidth int, tty bool) []string {
	words := splitWords(spans)
	if len(words) == 0 {
		return []string{""}
	}
	if maxWidth <= 0 {
		var line []styledSpan
		for i, w := range words {
			if i > 0 {
				line = append(line, styledSpan{text: " ", style: line[len(line)-1].style &^ styleUnderline})
			}
			line = append(line, w.parts...)
		}
		return []string{renderLineSpans(line, tty)}
	}

	var lines []string
	var cur []styledSpan
	curWidth := 0
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, renderLineSpans(cur, tty))
			cur = nil
			curWidth = 0
		}
	}

	for _, rawWord := range words {
		for _, w := range breakLongWord(rawWord, maxWidth) {
			if curWidth == 0 {
				cur = append(cur, w.parts...)
				curWidth = w.width
				continue
			}
			if curWidth+1+w.width <= maxWidth {
				spaceStyle := styleNone
				if len(cur) > 0 && len(w.parts) > 0 && cur[len(cur)-1].style == w.parts[0].style {
					spaceStyle = cur[len(cur)-1].style &^ styleUnderline
				}
				cur = append(cur, styledSpan{text: " ", style: spaceStyle})
				cur = append(cur, w.parts...)
				curWidth += 1 + w.width
			} else {
				flush()
				cur = append(cur, w.parts...)
				curWidth = w.width
			}
		}
	}
	flush()
	return lines
}

func renderLineSpans(spans []styledSpan, tty bool) string {
	if !tty {
		var b strings.Builder
		for _, sp := range spans {
			b.WriteString(sp.text)
		}
		return b.String()
	}

	// Merge adjacent spans with identical style so ANSI codes wrap whole phrases on a line.
	var merged []styledSpan
	for _, sp := range spans {
		if sp.text == "" {
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].style == sp.style {
			merged[len(merged)-1].text += sp.text
		} else {
			merged = append(merged, sp)
		}
	}

	var b strings.Builder
	for _, sp := range merged {
		open, closeSeq := styleSGR(sp.style)
		b.WriteString(open)
		b.WriteString(sp.text)
		b.WriteString(closeSeq)
	}
	return b.String()
}

func styleSGR(st spanStyle) (open, closeSeq string) {
	if st == styleNone {
		return "", ""
	}
	var openCodes, closeCodes []string
	if st&styleBold != 0 {
		openCodes = append(openCodes, "1")
		closeCodes = append(closeCodes, "22")
	}
	if st&styleDim != 0 {
		openCodes = append(openCodes, "2")
		closeCodes = append(closeCodes, "22")
	}
	if st&styleUnderline != 0 {
		openCodes = append(openCodes, "4")
		closeCodes = append(closeCodes, "24")
	}
	if st&styleCode != 0 {
		openCodes = append(openCodes, "36")
		closeCodes = append(closeCodes, "39")
	}
	return "\x1b[" + strings.Join(openCodes, ";") + "m", "\x1b[" + strings.Join(closeCodes, ";") + "m"
}
