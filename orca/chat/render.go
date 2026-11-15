package chat

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

const (
	replyPrefix     = "orca> "
	replyIndent     = "      "
	userPrefix      = "you> "
	userIndent      = "     "
	DefaultMaxTurns = 3
)

type spanStyle uint16

const (
	styleNone spanStyle = 0
	styleBold spanStyle = 1 << iota
	styleCode
	styleUnderline
	styleDim
	styleRed
	styleGreen
	styleYellow
	styleBlue
	styleMagenta
	styleCyan
)

// SpinnerFrames are the braille animation glyphs used for in-progress turns and
// background activity indicators.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type styledSpan struct {
	text  string
	style spanStyle
}

// FormatReply renders an assistant markdown reply into terminal lines with
// word-boundary wrapping and continuation indent under "orca> ".
// When tty is false, markdown syntax is stripped to plain text without ANSI escapes.
func FormatReply(text string, width int, tty bool) []string {
	text = strings.TrimPrefix(text, replyPrefix)
	text = SanitizeText(text)
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
			if code == "" {
				bodyLines = append(bodyLines, "")
				lastBlank = true
				continue
			}
			codeWidth := 0
			if bodyWidth > 2 {
				codeWidth = bodyWidth - 2
			}
			for _, w := range wrapCodeLine(code, codeWidth, tty) {
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
			bodyLines = append(bodyLines, renderListItem(lead+bullet, rest, bodyWidth, tty)...)
			continue
		}

		if marker, rest, ok := cutNumbered(line); ok {
			bodyLines = append(bodyLines, renderListItem(marker, rest, bodyWidth, tty)...)
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
	text = SanitizeText(strings.TrimSpace(text))
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

// FooterOptions configures dynamic styling and animation in the status footer.
//
//nolint:govet
type FooterOptions struct {
	BackgroundRole   string
	SpinnerFrame     int
	BackgroundActive bool
	CostState        CostGuardState
}

// phaseEmoji returns the phase icon and color style for a live lane phase.
func phaseEmoji(phase string) (emoji string, style spanStyle) {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "swe":
		return "🔨", styleCyan
	case "clean":
		return "🧹", styleBlue
	case "review":
		return "🔍", styleYellow
	case "integrate":
		return "🚀", styleMagenta
	default:
		return "▸", styleCyan
	}
}

func classifyBackgroundEvent(clean string) (emoji string, style spanStyle) {
	lower := strings.ToLower(clean)
	switch {
	case strings.HasPrefix(lower, "cost guard"):
		return "🛑", styleRed | styleBold
	case strings.HasPrefix(lower, "pr merged"):
		return "🎉", styleMagenta | styleBold
	case strings.HasPrefix(lower, "lane question"):
		return "❓", styleYellow | styleBold
	case isLaneLifecycleNotice(lower):
		return "🔄", styleCyan
	case strings.HasPrefix(lower, "dispatcher failed:"):
		return "✗", styleRed
	case strings.HasPrefix(lower, "dispatcher:"):
		return "⚡", styleYellow
	default:
		return "•", styleCyan
	}
}

func isLaneLifecycleNotice(lower string) bool {
	rest, ok := strings.CutPrefix(lower, "lane ")
	if !ok {
		return false
	}
	if strings.HasPrefix(rest, "advanced:") || strings.HasPrefix(rest, "completed:") {
		return true
	}
	laneID, tail, ok := strings.Cut(rest, " ")
	return ok && laneID != "" && (strings.HasPrefix(tail, "advanced:") || strings.HasPrefix(tail, "completed:"))
}

// FormatBackgroundEvent renders a notable background event (lane advanced, PR
// merged, lane question, cost guard pause) as colored line(s) on a TTY or
// plain text when piped/NO_COLOR, wrapping across physical lines at narrow widths.
func FormatBackgroundEvent(line string, width int, tty bool) []string {
	clean := strings.TrimSpace(sanitizeInline(line))
	if clean == "" {
		return nil
	}
	if !tty {
		return wrapPhysicalLines(clean, width)
	}
	emoji, style := classifyBackgroundEvent(clean)
	prefix := "  " + emoji + " "
	prefixWidth := runewidth.StringWidth(prefix)
	bodyWidth := 0
	if width > 0 {
		total := effectiveWidth(width)
		bodyWidth = max(total-prefixWidth, 10)
		if total < prefixWidth+4 {
			bodyWidth = max(total, 1)
		}
	}
	wrapped := wrapSpans([]styledSpan{{text: clean, style: styleNone}}, bodyWidth, false)
	indent := strings.Repeat(" ", prefixWidth)
	out := make([]string, len(wrapped))
	for i, w := range wrapped {
		row := prefix + w
		if i > 0 {
			row = indent + w
		}
		out[i] = renderStyledLine(row, style, 0, true)
	}
	return out
}

// FormatStatusFooter renders the 1- or 2-line pinned status footer with colored
// segments, phase emojis, PR check colors, dimmed elapsed time, highlighted open
// questions, cost-guard coloring, and an optional background activity spinner.
// When tty is false, it returns plain text without ANSI escapes or emojis.
func FormatStatusFooter(status string, opts FooterOptions, width int, tty bool) []string {
	status = strings.TrimSpace(SanitizeText(status))
	if status == "" {
		return nil
	}
	raw := strings.Split(status, "\n")
	if len(raw) > 2 {
		raw = raw[:2]
	}
	if !tty {
		out := make([]string, len(raw))
		for i, line := range raw {
			out[i] = truncateLine(line, width)
		}
		return out
	}
	out := make([]string, len(raw))
	for i, line := range raw {
		var spans []styledSpan
		if i == 0 && opts.BackgroundActive {
			glyph := SpinnerFrames[max(opts.SpinnerFrame, 0)%len(SpinnerFrames)]
			role := cmp.Or(strings.TrimSpace(sanitizeInline(opts.BackgroundRole)), "controller")
			emoji, rStyle := "🔄", styleCyan
			if role == "dispatcher" || role == "planner" || role == "concierge" {
				emoji, rStyle = handlerBadge(role)
			}
			spans = append(spans,
				styledSpan{text: glyph + " " + emoji + " " + role, style: rStyle | styleBold},
				styledSpan{text: " | ", style: styleDim},
			)
		}
		segments := strings.Split(line, " | ")
		for si, seg := range segments {
			if si > 0 {
				spans = append(spans, styledSpan{text: " | ", style: styleDim})
			}
			spans = append(spans, formatFooterSegmentSpans(seg, opts)...)
		}
		out[i] = renderLineSpans(truncateSpans(spans, width), true)
	}
	return out
}

func formatFooterSegmentSpans(seg string, opts FooterOptions) []styledSpan {
	seg = strings.TrimSpace(seg)
	switch {
	case strings.HasPrefix(seg, "status error:"):
		return []styledSpan{{text: seg, style: styleRed | styleBold}}
	case strings.HasPrefix(seg, "front desk:"):
		return []styledSpan{{text: seg, style: styleDim}}
	case strings.HasPrefix(seg, "adopted: "):
		return []styledSpan{{text: "🐋 " + seg, style: styleCyan | styleBold}}
	case strings.HasPrefix(seg, "goal: "):
		rest := strings.TrimPrefix(seg, "goal: ")
		return []styledSpan{
			{text: "🎯 goal: ", style: styleCyan},
			{text: rest, style: styleBold},
		}
	case strings.HasPrefix(seg, "lanes: "):
		return formatLanesSummarySpans(seg)
	case strings.HasPrefix(seg, "open questions: "):
		rest := strings.TrimSpace(strings.TrimPrefix(seg, "open questions: "))
		if n, err := strconv.Atoi(rest); err == nil && n > 0 {
			return []styledSpan{{text: "❓ open questions: " + rest, style: styleYellow | styleBold}}
		}
		return []styledSpan{{text: seg, style: styleDim}}
	case strings.HasPrefix(seg, "cost today: "):
		return formatCostSegmentSpans(seg, opts)
	default:
		return formatLiveLanesGroupSpans(seg)
	}
}

func formatLanesSummarySpans(seg string) []styledSpan {
	rest := strings.TrimPrefix(seg, "lanes: ")
	countPart, parenPart, hasParen := strings.Cut(rest, " (")
	spans := []styledSpan{
		{text: "lanes: ", style: styleDim},
		{text: countPart, style: styleBold},
	}
	if !hasParen || !strings.HasSuffix(parenPart, ")") {
		return spans
	}
	inner := strings.TrimSuffix(parenPart, ")")
	statusPart, phasePart, hasSemi := strings.Cut(inner, "; ")
	spans = append(spans, styledSpan{text: " (", style: styleDim})
	for i, item := range strings.Split(statusPart, ", ") {
		if i > 0 {
			spans = append(spans, styledSpan{text: ", ", style: styleDim})
		}
		st := styleDim
		switch {
		case strings.HasPrefix(item, "done:"):
			st = styleGreen
		case strings.HasPrefix(item, "failed:") || strings.HasPrefix(item, "blocked:"):
			st = styleRed
		case strings.HasPrefix(item, "running:"):
			st = styleCyan
		}
		spans = append(spans, styledSpan{text: item, style: st})
	}
	if hasSemi {
		spans = append(spans, styledSpan{text: "; " + phasePart, style: styleDim})
	}
	spans = append(spans, styledSpan{text: ")", style: styleDim})
	return spans
}

func formatCostSegmentSpans(seg string, opts FooterOptions) []styledSpan {
	switch opts.CostState {
	case CostStateOverLimit:
		return []styledSpan{{text: "🛑 " + seg, style: styleRed | styleBold}}
	case CostStateNearLimit:
		return []styledSpan{{text: "🔶 " + seg, style: styleYellow | styleBold}}
	default:
		return []styledSpan{{text: seg, style: styleGreen}}
	}
}

func splitTopLevelComma(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 && i+1 < len(s) && s[i+1] == ' ' {
				out = append(out, s[start:i])
				start = i + 2
				i++
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func formatLiveLanesGroupSpans(seg string) []styledSpan {
	items := splitTopLevelComma(seg)
	var spans []styledSpan
	for i, item := range items {
		if i > 0 {
			spans = append(spans, styledSpan{text: ", ", style: styleDim})
		}
		spans = append(spans, formatSingleLiveLaneSpans(strings.TrimSpace(item))...)
	}
	return spans
}

func isElapsedToken(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	last := tok[len(tok)-1]
	if last != 's' && last != 'm' && last != 'h' {
		return false
	}
	for i := 0; i < len(tok)-1; i++ {
		c := tok[i]
		if (c < '0' || c > '9') && c != 'h' {
			return false
		}
	}
	return tok[0] >= '0' && tok[0] <= '9'
}

func formatSingleLiveLaneSpans(item string) []styledSpan {
	var spans []styledSpan
	if colonIdx := strings.Index(item, ": "); colonIdx > 0 {
		if parenIdx := strings.IndexByte(item, '('); parenIdx < 0 || colonIdx < parenIdx {
			prefix := item[:colonIdx]
			if !strings.Contains(prefix, " ") {
				spans = append(spans, styledSpan{text: prefix + ": ", style: styleCyan | styleBold})
				item = strings.TrimSpace(item[colonIdx+2:])
			}
		}
	}
	var elapsedTok string
	if lastSpace := strings.LastIndexByte(item, ' '); lastSpace > 0 {
		cand := item[lastSpace+1:]
		if isElapsedToken(cand) {
			elapsedTok = cand
			item = strings.TrimSpace(item[:lastSpace])
		}
	}
	head, prRest, hasPR := strings.Cut(item, " PR #")
	fields := strings.Fields(head)
	if len(fields) >= 2 {
		laneID := fields[0]
		phase := fields[1]
		emoji, pStyle := phaseEmoji(phase)
		tail := phase
		if len(fields) > 2 {
			tail += " " + strings.Join(fields[2:], " ")
		}
		spans = append(spans,
			styledSpan{text: emoji + " ", style: pStyle},
			styledSpan{text: laneID, style: styleBold},
			styledSpan{text: " " + tail, style: pStyle},
		)
	} else {
		spans = append(spans, styledSpan{text: head, style: styleDim})
	}
	if hasPR {
		prNum, parenPart, hasParen := strings.Cut(prRest, " (")
		spans = append(spans, styledSpan{text: " PR #" + prNum, style: styleBold})
		if hasParen && strings.HasSuffix(parenPart, ")") {
			inner := strings.TrimSuffix(parenPart, ")")
			spans = append(spans, styledSpan{text: " (", style: styleDim})
			spans = append(spans, formatPRParenSpans(inner)...)
			spans = append(spans, styledSpan{text: ")", style: styleDim})
		} else if hasParen {
			spans = append(spans, styledSpan{text: " (" + parenPart, style: styleDim})
		}
	}
	if elapsedTok != "" {
		spans = append(spans, styledSpan{text: " " + elapsedTok, style: styleDim})
	}
	return spans
}

func formatPRParenSpans(inner string) []styledSpan {
	if statePart, checksPart, hasChecks := strings.Cut(inner, ", checks: "); hasChecks {
		spans := []styledSpan{
			formatPRStateSpan(statePart),
			{text: ", checks: ", style: styleDim},
		}
		return append(spans, formatChecksListSpans(checksPart)...)
	}
	if afterChecks, ok := strings.CutPrefix(inner, "checks: "); ok {
		spans := []styledSpan{{text: "checks: ", style: styleDim}}
		return append(spans, formatChecksListSpans(afterChecks)...)
	}
	return []styledSpan{formatPRStateSpan(inner)}
}

func formatPRStateSpan(state string) styledSpan {
	lower := strings.ToLower(strings.TrimSpace(state))
	switch {
	case lower == "open":
		return styledSpan{text: state, style: styleGreen}
	case lower == "merged":
		return styledSpan{text: state, style: styleMagenta | styleBold}
	case lower == "closed":
		return styledSpan{text: state, style: styleRed}
	case strings.Contains(lower, "fail"):
		return styledSpan{text: state, style: styleRed | styleBold}
	case strings.Contains(lower, "pend"):
		return styledSpan{text: state, style: styleYellow}
	case strings.Contains(lower, "pass"):
		return styledSpan{text: state, style: styleGreen}
	default:
		return styledSpan{text: state, style: styleCyan}
	}
}

func formatChecksListSpans(checks string) []styledSpan {
	parts := strings.Split(checks, ", ")
	var spans []styledSpan
	for i, ck := range parts {
		if i > 0 {
			spans = append(spans, styledSpan{text: ", ", style: styleDim})
		}
		lower := strings.ToLower(ck)
		st := styleDim
		switch {
		case strings.Contains(lower, "fail"):
			st = styleRed | styleBold
		case strings.Contains(lower, "pass") || strings.Contains(lower, "green"):
			st = styleGreen
		case strings.Contains(lower, "pend") || strings.Contains(lower, "wait") || strings.Contains(lower, "run"):
			st = styleYellow
		}
		spans = append(spans, styledSpan{text: ck, style: st})
	}
	return spans
}
func handlerBadge(handler string) (emoji string, style spanStyle) {
	switch strings.ToLower(strings.TrimSpace(handler)) {
	case "concierge":
		return "💬", styleCyan
	case "dispatcher":
		return "⚡", styleYellow
	case "planner":
		return "🧭", styleMagenta
	default:
		return "⏳", styleCyan
	}
}

// FormatRouteLine renders a routing echo line ("→ concierge: ...") with a
// handler-specific emoji and color on a TTY, or plain text when piped/NO_COLOR.
func FormatRouteLine(handler, text string, fallback bool, width int, tty bool) []string {
	echo := RouteEcho(handler, text, fallback)
	if !tty {
		return FormatDimLine(echo, width, false)
	}
	emoji, style := handlerBadge(handler)
	line := "  " + emoji + " " + echo
	if width > 0 {
		line = truncateLine(line, width)
	}
	return []string{renderStyledLine(line, style, 0, true)}
}

// FormatToolStep formats a tool call line into a concise spinner step
// (e.g. "→ status 20261114-trial-m1", "reading OBJECTIVE.md").
func FormatToolStep(toolLine string) string {
	clean := strings.TrimSpace(sanitizeInline(toolLine))
	if after, ok := strings.CutPrefix(clean, "→ read.file "); ok && strings.TrimSpace(after) != "" {
		return "reading " + strings.TrimSpace(after)
	}
	return clean
}

// FormatSpinnerLine renders an animated single-line progress indicator under the
// owner's message while a turn runs.
func FormatSpinnerLine(frame int, handler, step string, elapsed time.Duration, width int, tty bool) string {
	step = cmp.Or(strings.TrimSpace(sanitizeInline(step)), "thinking")
	secs := max(int(elapsed.Round(time.Second)/time.Second), 0)
	elapsedStr := fmt.Sprintf("(%ds)", secs)
	handlerClean := strings.TrimSpace(sanitizeInline(handler))
	if !tty {
		line := "  * " + step + " " + elapsedStr
		if handlerClean != "" {
			line = "  * " + handlerClean + " · " + step + " " + elapsedStr
		}
		return truncateLine(line, width)
	}
	glyph := SpinnerFrames[max(frame, 0)%len(SpinnerFrames)]
	emoji, hStyle := handlerBadge(handlerClean)
	var spans []styledSpan
	if handlerClean != "" {
		spans = []styledSpan{
			{text: "  " + glyph + " " + emoji + " " + handlerClean, style: hStyle | styleBold},
			{text: " · ", style: styleDim},
			{text: step, style: hStyle},
			{text: " " + elapsedStr, style: styleDim},
		}
	} else {
		spans = []styledSpan{
			{text: "  " + glyph + " ", style: hStyle | styleBold},
			{text: step, style: hStyle},
			{text: " " + elapsedStr, style: styleDim},
		}
	}
	return renderLineSpans(truncateSpans(spans, width), true)
}

// RouteEcho formats the plain-text routing echo ("→ <handler>: <text>").
func RouteEcho(handler, text string, fallback bool) string {
	echo := "→ " + sanitizeInline(handler) + ": " + sanitizeInline(text)
	if fallback {
		echo += " (via fallback)"
	}
	return echo
}

// FormatDimLine renders a routing or tool-call line ("→ ...") as a single dim
// line on a TTY or plain text when piped.
func FormatDimLine(text string, width int, tty bool) []string {
	text = SanitizeText(strings.TrimSpace(text))
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
	} else {
		text = SanitizeText(text)
	}
	return renderLineSpans([]styledSpan{{text: text, style: style}}, tty)
}

// FormatToolCall summarizes a tool call as a single "→ <name> <target>" line.
func FormatToolCall(name string, rawInput []byte, defaultRunID string) string {
	name = strings.TrimSpace(sanitizeInline(name))
	var fields map[string]any
	if len(rawInput) > 0 {
		_ = json.Unmarshal(rawInput, &fields)
	}
	strField := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := fields[k]; ok {
				if s, ok := v.(string); ok {
					if clean := strings.TrimSpace(SanitizeText(s)); clean != "" {
						return clean
					}
				}
			}
		}
		return ""
	}
	runID := cmp.Or(strField("run_id"), strings.TrimSpace(sanitizeInline(defaultRunID)))
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
		maxTurns = DefaultMaxTurns
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
		raw = SanitizeText(raw)
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
		case strings.Contains(trimmed, "earlier turns; /log to see them"):
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

func renderListItem(marker, rest string, bodyWidth int, tty bool) []string {
	markerWidth := runewidth.StringWidth(marker)
	itemWidth := 0
	if bodyWidth > 0 {
		itemWidth = max(bodyWidth-markerWidth, 8)
	}
	spans := parseInline(rest, styleNone)
	wrapped := wrapSpans(spans, itemWidth, tty)
	hanging := strings.Repeat(" ", markerWidth)
	out := make([]string, len(wrapped))
	for i, w := range wrapped {
		if i == 0 {
			out[i] = marker + w
		} else {
			out[i] = hanging + w
		}
	}
	return out
}

func wrapCodeLine(code string, maxWidth int, tty bool) []string {
	if code == "" {
		return []string{""}
	}
	if maxWidth <= 0 || runewidth.StringWidth(code) <= maxWidth {
		return []string{renderStyledLine(code, styleCode, 0, tty)}
	}
	var out []string
	var buf strings.Builder
	curWidth := 0
	for _, r := range code {
		rw := max(runewidth.RuneWidth(r), 1)
		if curWidth+rw > maxWidth && curWidth > 0 {
			out = append(out, renderStyledLine(buf.String(), styleCode, 0, tty))
			buf.Reset()
			curWidth = 0
		}
		buf.WriteRune(r)
		curWidth += rw
	}
	if buf.Len() > 0 {
		out = append(out, renderStyledLine(buf.String(), styleCode, 0, tty))
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

	for _, w := range words {
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
	flush()
	return lines
}

// SanitizeText strips terminal escape sequences (CSI, OSC, 2-byte ESC) and
// C0/C1 control characters (other than '\n', with '\t' expanded to 4 spaces)
// so untrusted text cannot reposition the cursor or inject ANSI sequences.
func SanitizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		if r == '\x1b' || r == 0x9b || r == 0x9d {
			if r == 0x9b || (r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[') {
				if r == '\x1b' {
					i += 2
				} else {
					i++
				}
				for i < len(runes) && runes[i] >= 0x20 && runes[i] <= 0x3f {
					i++
				}
				if i < len(runes) && runes[i] >= 0x40 && runes[i] <= 0x7e {
					i++
				}
				continue
			}
			if r == 0x9d || (r == '\x1b' && i+1 < len(runes) && runes[i+1] == ']') {
				if r == '\x1b' {
					i += 2
				} else {
					i++
				}
				for i < len(runes) {
					if runes[i] == '\x07' || runes[i] == 0x9c {
						i++
						break
					}
					if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
				continue
			}
			if i+1 < len(runes) {
				i += 2
			} else {
				i++
			}
			continue
		}
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n':
			b.WriteByte('\n')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// Strip remaining C0/DEL/C1 control characters.
		default:
			b.WriteRune(r)
		}
		i++
	}
	return b.String()
}

func sanitizeInline(s string) string {
	return strings.ReplaceAll(SanitizeText(s), "\n", " ")
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

func truncateSpans(spans []styledSpan, width int) []styledSpan {
	if width <= 0 {
		out := make([]styledSpan, 0, len(spans))
		for _, sp := range spans {
			if txt := sanitizeInline(sp.text); txt != "" {
				out = append(out, styledSpan{text: txt, style: sp.style})
			}
		}
		return out
	}
	limit := effectiveWidth(width)
	totalWidth := 0
	clean := make([]styledSpan, 0, len(spans))
	for _, sp := range spans {
		if txt := sanitizeInline(sp.text); txt != "" {
			clean = append(clean, styledSpan{text: txt, style: sp.style})
			totalWidth += runewidth.StringWidth(txt)
		}
	}
	if totalWidth <= limit {
		return clean
	}
	if limit <= 1 {
		if limit == 1 && len(clean) > 0 {
			return []styledSpan{{text: "…", style: clean[0].style}}
		}
		return nil
	}
	var out []styledSpan
	curWidth := 0
	for _, sp := range clean {
		var buf strings.Builder
		for _, r := range sp.text {
			rw := max(runewidth.RuneWidth(r), 1)
			if curWidth+rw > limit-1 {
				if buf.Len() > 0 {
					out = append(out, styledSpan{text: buf.String(), style: sp.style})
				}
				out = append(out, styledSpan{text: "…", style: styleDim})
				return out
			}
			buf.WriteRune(r)
			curWidth += rw
		}
		if buf.Len() > 0 {
			out = append(out, styledSpan{text: buf.String(), style: sp.style})
		}
	}
	return out
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
		if st&styleBold == 0 {
			closeCodes = append(closeCodes, "22")
		}
	}
	if st&styleUnderline != 0 {
		openCodes = append(openCodes, "4")
		closeCodes = append(closeCodes, "24")
	}
	switch {
	case st&styleRed != 0:
		openCodes = append(openCodes, "31")
		closeCodes = append(closeCodes, "39")
	case st&styleGreen != 0:
		openCodes = append(openCodes, "32")
		closeCodes = append(closeCodes, "39")
	case st&styleYellow != 0:
		openCodes = append(openCodes, "33")
		closeCodes = append(closeCodes, "39")
	case st&styleBlue != 0:
		openCodes = append(openCodes, "34")
		closeCodes = append(closeCodes, "39")
	case st&styleMagenta != 0:
		openCodes = append(openCodes, "35")
		closeCodes = append(closeCodes, "39")
	case st&(styleCyan|styleCode) != 0:
		openCodes = append(openCodes, "36")
		closeCodes = append(closeCodes, "39")
	}
	return "\x1b[" + strings.Join(openCodes, ";") + "m", "\x1b[" + strings.Join(closeCodes, ";") + "m"
}
