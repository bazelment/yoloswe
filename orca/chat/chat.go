// Package chat provides the owner-facing Bubble Tea conversation.
package chat

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/orca/router"
)

type toolEmitterKey struct{}

// WithToolEmitter attaches a callback to ctx that receives live tool-call lines
// while a turn is running.
func WithToolEmitter(ctx context.Context, emit func(string)) context.Context {
	return context.WithValue(ctx, toolEmitterKey{}, emit)
}

// EmitToolCall formats and emits a live tool-call line if ctx carries a tool emitter.
func EmitToolCall(ctx context.Context, name string, rawInput []byte, defaultRunID string) {
	if emit, ok := ctx.Value(toolEmitterKey{}).(func(string)); ok && emit != nil {
		emit(FormatToolCall(name, rawInput, defaultRunID))
	}
}

type AskFunc func(context.Context, string) (string, error)
type DispatchFunc func(context.Context, router.Decision, string, func(string)) error

// CostGuardState classifies run spend relative to the cost-guard limit.
type CostGuardState uint8

const (
	CostStateNormal CostGuardState = iota
	CostStateNearLimit
	CostStateOverLimit
)

//nolint:govet
type Snapshot struct {
	Status           string
	BackgroundRole   string
	Questions        []string
	Outcomes         []string
	CostLimitUSD     float64
	BackgroundActive bool
	CostState        CostGuardState
}
type Backend interface {
	Snapshot(context.Context) (Snapshot, error)
	Command(context.Context, string) (string, error)
}

// RunLister supplies run IDs for argument completion.
type RunLister interface {
	RunIDs(context.Context) ([]string, error)
}

// Command describes a slash command and its argument hint.
type Command struct{ Name, Args string }

// Commands lists slash commands in help order.
var Commands = []Command{
	{"/runs", "[run-id]"}, {"/status", ""}, {"/lanes", ""}, {"/asks", ""}, {"/memory", ""}, {"/plan", ""},
	{"/log", "<trace>"}, {"/pause", ""}, {"/resume", ""}, {"/stop", ""}, {"/undo", ""},
	{"/adopt", "[run-id]"}, {"/release", "[run-id]"}, {"/restart", "[path]"}, {"/exit", ""},
}

func (c Command) usage() string { return strings.TrimSpace(c.Name + " " + c.Args) }

func commandUsage() string {
	out := make([]string, len(Commands))
	for i, c := range Commands {
		out[i] = c.usage()
	}
	return strings.Join(out, " ")
}

//nolint:govet
type Model struct {
	poll                 bool
	NoColor              bool
	initPrinted          bool
	refreshing           bool
	restarting           bool
	spinnerTicking       bool
	bgActive             bool
	bgRole               string
	costState            CostGuardState
	costLimitUSD         float64
	restartMessage       string
	PollInterval         time.Duration
	SpinnerInterval      time.Duration
	Now                  func() time.Time
	turnStartedAt        time.Time
	turnHandler          string
	turnStep             string
	spinnerFrame         int
	questions            []string
	ctx                  context.Context
	ask                  AskFunc
	router               router.Router
	Dispatch             DispatchFunc
	Backend              Backend
	runID, input, status string
	streamBuf            string
	initLines            []string
	pendingPrint         []string
	lines                []string
	queued               []string
	busy, streaming      bool
	width                int
	streamLine           int
	turnCount            int
	// cursor is the input cursor's byte offset.
	cursor int
	// hints are rendered below the prompt.
	hints []string
}

// NoColor reports whether terminal color, emoji, and animation are disabled via
// the NO_COLOR environment variable.
func NoColor() bool {
	return os.Getenv("NO_COLOR") != ""
}

func (m Model) styled() bool {
	return m.poll && !m.NoColor && !NoColor()
}

func (m Model) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	if b, ok := m.Backend.(*StoreBackend); ok && b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (m Model) turnElapsed() time.Duration {
	if m.turnStartedAt.IsZero() {
		return 0
	}
	return max(m.now().Sub(m.turnStartedAt), 0)
}

func New(ctx context.Context, runID string, ask AskFunc, r router.Router) Model {
	return Model{ctx: ctx, runID: runID, ask: ask, router: r, poll: true}
}

// WithTranscript adds earlier conversation lines above new input.
func (m Model) WithTranscript(lines []string) Model {
	m.initLines = append(m.initLines, lines...)
	m.lines = append(m.lines, lines...)
	if len(lines) > 0 {
		m.turnCount = 1
	}
	return m
}

// IsNewGoal reports whether text talks about starting work, so chat routes it
// to the concierge. Talking about it is not asking for it: run.start acts only
// on IsStartRequest.
func IsNewGoal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "start a run") || strings.Contains(lower, "start work") || strings.HasPrefix(lower, "new goal")
}

// startPhrases open an explicit request to start a run.
var startPhrases = []string{
	"start a run", "start a new run", "start the run", "start run", "start work", "start working",
	"kick off a run", "launch a run", "begin a run", "new goal", "new run",
}

// politeLeads may precede a start phrase without changing its meaning.
var politeLeads = []string{"please", "ok", "okay", "alright", "now", "go ahead and", "let's", "lets", "kindly", "orca"}

// doubtWords make a request negated, conditional or tentative.
var doubtWords = map[string]bool{
	"not": true, "no": true, "never": true, "don't": true, "dont": true, "doesn't": true, "won't": true,
	"can't": true, "cannot": true, "shouldn't": true, "without": true, "hold": true, "wait": true,
	"yet": true, "later": true, "if": true, "unless": true, "until": true, "maybe": true, "whether": true,
}

// questionLeads open a question rather than a request.
var questionLeads = map[string]bool{
	"how": true, "what": true, "why": true, "when": true, "where": true, "which": true, "who": true,
	"can": true, "could": true, "would": true, "should": true, "shall": true, "will": true, "may": true,
	"do": true, "does": true, "did": true, "is": true, "are": true,
}

func words(text string) []string {
	lower := strings.ToLower(strings.ReplaceAll(text, "’", "'"))
	return strings.FieldsFunc(lower, func(r rune) bool {
		return r != '\'' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// IsStartRequest reports whether text explicitly and affirmatively asks to
// start a run: a clause opens with a start phrase ("start a run to ...",
// "new goal: ..."), and nothing negates, conditions or questions it. When it
// returns false the concierge asks instead of starting.
func IsStartRequest(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.Contains(text, "?") {
		return false
	}
	all := words(text)
	if len(all) == 0 || questionLeads[all[0]] {
		return false
	}
	for _, w := range all {
		if doubtWords[w] {
			return false
		}
	}
	for _, clause := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return strings.ContainsRune(".!;:\n", r) }) {
		c := strings.Join(words(clause), " ")
		for trimmed := true; trimmed; {
			trimmed = false
			for _, lead := range politeLeads {
				if rest, ok := strings.CutPrefix(c, lead+" "); ok {
					c, trimmed = rest, true
				}
			}
		}
		for _, p := range startPhrases {
			if c == p || strings.HasPrefix(c, p+" ") {
				return true
			}
		}
	}
	return false
}

// confirmations are plain answers of yes.
var confirmations = map[string]bool{
	"yes": true, "y": true, "yep": true, "yeah": true, "sure": true, "confirm": true, "confirmed": true,
	"go ahead": true, "do it": true, "start it": true, "yes please": true, "please do": true,
	"yes start it": true, "yes go ahead": true, "yes do it": true, "ok start it": true, "okay start it": true,
}

// IsConfirmation reports whether text is a plain yes, answering a question
// such as "Shall I start a run for ...?".
func IsConfirmation(text string) bool {
	return confirmations[strings.Join(words(text), " ")]
}

//nolint:govet // message payload grouping
type pollMsg struct{}

//nolint:govet // message payload grouping
type spinnerTickMsg struct{}

//nolint:govet // message payload grouping
type initPrintMsg struct{}

//nolint:govet // message payload grouping
type snapshotMsg struct {
	snapshot Snapshot
	err      error
}

//nolint:govet // message payload grouping
type routeMsg struct {
	text     string
	decision router.Decision
	err      error
}

//nolint:govet // message payload grouping
type commandMsg struct {
	text string
	err  error
	quit bool
}

//nolint:govet // message payload grouping
type runIDsMsg struct {
	input string
	ids   []string
	err   error
}

//nolint:govet // message payload grouping
type streamMsg struct {
	text string
	tool string
	err  error
	done bool
	ch   <-chan streamMsg
}

func (m Model) refresh() tea.Cmd {
	if m.Backend == nil || !m.poll {
		return nil
	}
	return func() tea.Msg { s, e := m.Backend.Snapshot(m.ctx); return snapshotMsg{s, e} }
}
func (m *Model) requestRefresh() tea.Cmd {
	if m.refreshing {
		return nil
	}
	cmd := m.refresh()
	m.refreshing = cmd != nil
	return cmd
}

// pollInterval returns the periodic status refresh cadence (default 15s);
// store.Store has no push notifications and controllers may run out-of-process,
// so store mutations are observed within one poll interval or on turn end.
func (m Model) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return 15 * time.Second
}
func (m Model) nextPoll() tea.Cmd {
	return tea.Tick(m.pollInterval(), func(time.Time) tea.Msg { return pollMsg{} })
}
func (m Model) spinnerInterval() time.Duration {
	if m.SpinnerInterval > 0 {
		return m.SpinnerInterval
	}
	return 110 * time.Millisecond
}
func (m Model) needsSpinner() bool {
	return m.styled() && m.SpinnerInterval >= 0 && (m.busy || m.bgActive)
}
func (m *Model) ensureSpinner() tea.Cmd {
	if !m.needsSpinner() || m.spinnerTicking {
		return nil
	}
	m.spinnerTicking = true
	return tea.Tick(m.spinnerInterval(), func(time.Time) tea.Msg { return spinnerTickMsg{} })
}
func (m Model) currentRunID() string {
	runID := m.runID
	if backend, ok := m.Backend.(*StoreBackend); ok {
		runID = cmp.Or(backend.RunID(), runID)
	}
	return runID
}
func (m Model) headerLine() string {
	return "orca chat — run " + m.currentRunID()
}
func (m Model) Init() tea.Cmd {
	if m.poll {
		// Delay initial scrollback emission by one 60fps render frame (18ms) so
		// Bubble Tea processes the initial WindowSizeMsg and snapshotMsg before
		// tea.Println writes the header and resumed transcript above the prompt.
		// Any lines printed before initPrintMsg arrives are queued in pendingPrint
		// to preserve strict output ordering.
		initTick := tea.Tick(18*time.Millisecond, func(time.Time) tea.Msg { return initPrintMsg{} })
		return tea.Batch(initTick, m.refresh(), m.nextPoll())
	}
	return m.refresh()
}
func (m *Model) flushInitPrint() tea.Cmd {
	if m.initPrinted || !m.poll {
		return nil
	}
	m.initPrinted = true
	rows := []string{m.headerLine()}
	if len(m.initLines) > 0 {
		rows = append(rows, FormatTranscript(m.initLines, DefaultMaxTurns, m.width, m.styled())...)
	}
	rows = append(rows, m.pendingPrint...)
	m.pendingPrint = nil
	return tea.Println(strings.Join(rows, "\n"))
}
func (m *Model) printRawLines(rows ...string) tea.Cmd {
	if !m.poll || len(rows) == 0 {
		return nil
	}
	if !m.initPrinted {
		m.pendingPrint = append(m.pendingPrint, rows...)
		return nil
	}
	return tea.Println(strings.Join(rows, "\n"))
}

func (m *Model) flushStream(done bool) []string {
	if !done {
		return nil
	}
	m.streaming = false
	raw := strings.TrimPrefix(m.streamBuf, replyPrefix)
	m.streamBuf = ""
	if m.streamLine >= 0 && m.streamLine < len(m.lines) {
		m.lines[m.streamLine] = strings.Join(FormatReply(raw, m.width, false), "\n")
	}
	if raw == "" {
		return nil
	}
	return FormatReply(raw, m.width, m.styled())
}
func (m Model) submit(text string) (Model, tea.Cmd) {
	var rawRows []string
	if m.turnCount > 0 {
		m.lines = append(m.lines, "")
		rawRows = append(rawRows, "")
	}
	m.turnCount++
	userLine := "you> " + text
	m.lines = append(m.lines, userLine)
	rawRows = append(rawRows, FormatUserLine(text, m.width, m.styled())...)
	m.busy = true
	m.turnStartedAt = m.now()
	m.turnHandler = ""
	m.turnStep = "thinking"
	var runCmd tea.Cmd
	if strings.HasPrefix(text, "/") {
		m.turnStep = text
		fields := strings.Fields(text)
		isRestart := len(fields) > 0 && strings.EqualFold(fields[0], "/restart")
		ctx, backend := m.ctx, m.Backend
		runCmd = func() tea.Msg {
			if backend == nil {
				return commandMsg{err: fmt.Errorf("no run attached")}
			}
			s, e := backend.Command(ctx, text)
			return commandMsg{text: s, err: e, quit: isRestart && e == nil}
		}
	} else {
		runCmd = m.route(text)
	}
	printCmd := m.printRawLines(rawRows...)
	spinCmd := m.ensureSpinner()
	return m, tea.Batch(tea.Sequence(printCmd, runCmd), spinCmd)
}
func (m Model) finishTurn(printed ...string) (tea.Model, tea.Cmd) {
	m.busy = false
	m.turnHandler = ""
	m.turnStep = ""
	m.turnStartedAt = time.Time{}
	printCmd := m.printRawLines(printed...)
	var nextCmd tea.Cmd
	if len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		m, nextCmd = m.submit(next)
	}
	refresh := m.requestRefresh()
	return m, tea.Sequence(printCmd, nextCmd, refresh)
}
func receive(ctx context.Context, ch <-chan streamMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case x := <-ch:
			x.ch = ch
			return x
		case <-ctx.Done():
			return streamMsg{done: true, err: context.Canceled, ch: ch}
		}
	}
}
func (m Model) route(text string) tea.Cmd {
	return func() tea.Msg {
		if IsNewGoal(text) {
			return routeMsg{text: text, decision: router.Decision{Handler: router.Concierge, Confidence: 1, Backend: "goal-detection"}}
		}
		if m.router == nil {
			return routeMsg{text: text, decision: router.Decision{Handler: router.Concierge, Confidence: 1}}
		}
		input := text
		if m.Backend != nil {
			var questions []string
			var err error
			if q, ok := m.Backend.(interface {
				Questions(context.Context) ([]string, error)
			}); ok {
				questions, err = q.Questions(m.ctx)
			} else {
				var s Snapshot
				s, err = m.Backend.Snapshot(m.ctx)
				questions = s.Questions
			}
			if err != nil {
				return routeMsg{text: text, err: err}
			}
			if len(questions) > 0 {
				input = router.FormatInput(input, strings.Join(questions, "\n"))
			}
		}
		d, e := m.router.Route(m.ctx, input)
		return routeMsg{text, d, e}
	}
}
func (m Model) dispatch(d router.Decision, text string) tea.Cmd {
	ch := make(chan streamMsg)
	go func() {
		emit := func(s string) {
			select {
			case ch <- streamMsg{text: s}:
			case <-m.ctx.Done():
			}
		}
		turnCtx := WithToolEmitter(m.ctx, func(line string) {
			select {
			case ch <- streamMsg{tool: line}:
			case <-m.ctx.Done():
			}
		})
		var err error
		if m.Dispatch != nil {
			err = m.Dispatch(turnCtx, d, text, emit)
		} else if d.Handler == router.Concierge && m.ask != nil {
			var s string
			s, err = m.ask(turnCtx, text)
			emit(s)
		} else {
			err = fmt.Errorf("no handler for %s", d.Handler)
		}
		select {
		case ch <- streamMsg{done: true, err: err}:
		case <-m.ctx.Done():
		}
	}()
	return receive(m.ctx, ch)
}
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case initPrintMsg:
		cmd := m.flushInitPrint()
		return m, cmd
	case spinnerTickMsg:
		m.spinnerTicking = false
		if !m.styled() || (!m.busy && !m.bgActive) {
			return m, nil
		}
		m.spinnerFrame++
		spinCmd := m.ensureSpinner()
		return m, spinCmd
	case pollMsg:
		if !m.poll {
			return m, nil
		}
		refresh := m.requestRefresh()
		return m, tea.Batch(refresh, m.nextPoll())
	case tea.WindowSizeMsg:
		m.width = x.Width
	case snapshotMsg:
		m.refreshing = false
		if x.err != nil {
			m.status = "status error: " + x.err.Error()
			return m, nil
		}
		m.status = x.snapshot.Status
		m.questions = x.snapshot.Questions
		m.bgActive = x.snapshot.BackgroundActive
		m.bgRole = x.snapshot.BackgroundRole
		m.costState = x.snapshot.CostState
		m.costLimitUSD = x.snapshot.CostLimitUSD
		spinCmd := m.ensureSpinner()
		if len(x.snapshot.Outcomes) > 0 {
			m.lines = append(m.lines, x.snapshot.Outcomes...)
			var rawRows []string
			for _, o := range x.snapshot.Outcomes {
				rawRows = append(rawRows, FormatBackgroundEvent(o, m.width, m.styled())...)
			}
			printCmd := m.printRawLines(rawRows...)
			return m, tea.Batch(printCmd, spinCmd)
		}
		return m, spinCmd
	case commandMsg:
		out := x.text
		if x.err != nil {
			out = "error: " + x.err.Error()
		}
		m.lines = append(m.lines, out)
		if x.quit {
			m.busy = false
			m.restarting = true
			m.restartMessage = x.text
			return m, tea.Quit
		}
		return m.finishTurn(wrapPhysicalLines(out, m.width)...)
	case routeMsg:
		if x.err != nil {
			errLine := "error: " + x.err.Error()
			m.lines = append(m.lines, errLine)
			return m.finishTurn(wrapPhysicalLines(errLine, m.width)...)
		}
		echo := RouteEcho(string(x.decision.Handler), x.text, x.decision.Fallback)
		m.lines = append(m.lines, echo, "orca> ")
		m.streamLine = len(m.lines) - 1
		m.streaming = true
		m.streamBuf = ""
		m.turnHandler = string(x.decision.Handler)
		m.turnStep = "thinking"
		routeRows := FormatRouteLine(string(x.decision.Handler), x.text, x.decision.Fallback, m.width, m.styled())
		printCmd := m.printRawLines(routeRows...)
		return m, tea.Sequence(printCmd, m.dispatch(x.decision, x.text))
	case streamMsg:
		if x.tool != "" {
			m.lines = slices.Insert(m.lines, m.streamLine, x.tool)
			m.streamLine++
			m.turnStep = FormatToolStep(x.tool)
			toolRows := FormatDimLine(x.tool, m.width, m.styled())
			printCmd := m.printRawLines(toolRows...)
			return m, tea.Sequence(printCmd, receive(m.ctx, x.ch))
		}
		if x.text != "" {
			m.streamBuf += x.text
		}
		if x.done {
			printed := m.flushStream(true)
			if x.err != nil {
				errLine := "error: " + x.err.Error()
				m.lines = append(m.lines, errLine)
				printed = append(printed, wrapPhysicalLines(errLine, m.width)...)
			}
			return m.finishTurn(printed...)
		}
		return m, receive(m.ctx, x.ch)
	case runIDsMsg:
		if x.input == m.input {
			m.completeArg(x.ids, x.err)
		}
		return m, nil
	case tea.KeyPressMsg:
		if (x.Code == 'c' && x.Mod == tea.ModCtrl) || x.Code == tea.KeyEscape {
			return m, tea.Quit
		}
		if x.Code == tea.KeyTab {
			return m.complete()
		}
		if x.Code != tea.KeyEnter {
			m.edit(x)
			m.hints = commandHints(m.input)
			return m, nil
		}
		m.hints = nil
		text := strings.TrimSpace(m.input)
		m.setInput("")
		if text == "" {
			return m, nil
		}
		switch strings.ToLower(text) {
		case "exit", "quit", "/exit", "/quit":
			return m, tea.Quit
		}
		if m.busy {
			m.queued = append(m.queued, text)
			return m, nil
		}
		return m.submit(text)
	case tea.PasteMsg:
		m.insert(x.Content)
		m.hints = commandHints(m.input)
		return m, nil
	}
	return m, nil
}

// commandHints lists commands matching the current prefix.
func commandHints(input string) []string {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t") {
		return nil
	}
	var out []string
	for _, c := range Commands {
		if strings.HasPrefix(c.Name, strings.ToLower(input)) {
			out = append(out, c.usage())
		}
	}
	return out
}

// complete handles command and run-ID completion.
func (m Model) complete() (tea.Model, tea.Cmd) {
	name, arg, hasArg := strings.Cut(m.input, " ")
	name = strings.ToLower(name)
	if !strings.HasPrefix(name, "/") {
		return m, nil
	}
	if !hasArg {
		var names []string
		for _, c := range Commands {
			if strings.HasPrefix(c.Name, name) {
				names = append(names, c.Name)
			}
		}
		switch len(names) {
		case 0:
			m.hints = []string{"no command matches " + name}
		case 1:
			m.setInput(names[0] + " ")
			m.hints = nil
		default:
			m.setInput(commonPrefix(names))
			m.hints = commandHints(m.input)
		}
		return m, nil
	}
	lister, ok := m.Backend.(RunLister)
	if strings.Contains(arg, " ") || (name != "/adopt" && name != "/release") || !ok {
		return m, nil
	}
	input, ctx := m.input, m.ctx
	return m, func() tea.Msg {
		ids, err := lister.RunIDs(ctx)
		return runIDsMsg{input: input, ids: ids, err: err}
	}
}

func (m *Model) completeArg(ids []string, err error) {
	name, arg, _ := strings.Cut(m.input, " ")
	var match []string
	for _, id := range ids {
		if strings.HasPrefix(id, arg) {
			match = append(match, id)
		}
	}
	switch {
	case len(match) == 1:
		m.setInput(name + " " + match[0])
		m.hints = nil
	case len(match) > 1:
		m.setInput(name + " " + commonPrefix(match))
		m.hints = match
	case err != nil:
		m.hints = []string{"run ids unavailable: " + err.Error()}
	default:
		m.hints = []string{"no host swarm run matches " + strconv.Quote(arg)}
	}
}

func commonPrefix(s []string) string {
	p := s[0]
	for _, x := range s[1:] {
		for !strings.HasPrefix(x, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

// streamPreview renders a single-line live preview of the latest non-empty
// streaming line while a turn runs. Full markdown rendering is deferred until
// turn completion so live tool-call lines ("  → <name> <target>") appear
// above "orca> " in scrollback and multi-line markdown blocks wrap intact.
func (m Model) streamPreview() string {
	s := strings.TrimPrefix(m.streamBuf, replyPrefix)
	s = strings.TrimSpace(s)
	if idx := strings.LastIndexByte(s, '\n'); idx >= 0 {
		if tail := strings.TrimSpace(s[idx+1:]); tail != "" {
			s = tail
		} else {
			s = strings.ReplaceAll(s, "\n", " ")
		}
	}
	return replyPrefix + s
}

func (m Model) View() tea.View {
	var top strings.Builder
	switch {
	case m.streaming && strings.TrimSpace(strings.TrimPrefix(m.streamBuf, replyPrefix)) != "":
		top.WriteString(truncateLine(m.streamPreview(), m.width))
		top.WriteByte('\n')
	case m.busy && m.styled():
		top.WriteString(FormatSpinnerLine(m.spinnerFrame, m.turnHandler, m.turnStep, m.turnElapsed(), m.width, true))
		top.WriteByte('\n')
	case m.streaming:
		top.WriteString(truncateLine(m.streamPreview(), m.width))
		top.WriteByte('\n')
	}
	for _, q := range m.questions {
		top.WriteString(truncateLine("owner question> "+q, m.width))
		top.WriteByte('\n')
	}
	hints := ""
	if len(m.hints) > 0 {
		hints = "\n" + truncateLine("  "+strings.Join(m.hints, "  "), m.width)
	}
	return tea.NewView(top.String() + "\n> " + m.inputView() + hints + m.statusFooter())
}
func (m Model) statusFooter() string {
	if m.status == "" {
		return ""
	}
	out := FormatStatusFooter(m.status, FooterOptions{
		SpinnerFrame:     m.spinnerFrame,
		BackgroundActive: m.bgActive,
		BackgroundRole:   m.bgRole,
		CostState:        m.costState,
		CostLimitUSD:     m.costLimitUSD,
	}, m.width, m.styled())
	if len(out) == 0 {
		return ""
	}
	return "\n" + strings.Join(out, "\n")
}
func effectiveWidth(width int) int {
	if width > 1 {
		return width - 1
	}
	return width
}
func wrapPhysicalLines(s string, width int) []string {
	s = SanitizeText(s)
	if width <= 1 {
		return strings.Split(s, "\n")
	}
	return strings.Split(runewidth.Wrap(s, effectiveWidth(width)), "\n")
}
func truncateLine(s string, width int) string {
	s = SanitizeText(s)
	if width <= 0 {
		return s
	}
	return runewidth.Truncate(s, effectiveWidth(width), "…")
}
func isTerminal(in io.Reader) bool { f, ok := in.(*os.File); return ok && term.IsTerminal(int(f.Fd())) }
func Run(ctx context.Context, in io.Reader, out io.Writer, m Model) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.ctx = ctx
	if isTerminal(in) {
		m.poll = true
		if m.width <= 0 {
			for _, rw := range []any{out, in} {
				if f, ok := rw.(*os.File); ok {
					if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
						m.width = w
						break
					}
				}
			}
		}
		final, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx)).Run()
		if err == nil {
			if fm, ok := final.(Model); ok && fm.restarting {
				_, _ = fmt.Fprintln(out, fm.restartMessage)
			}
		}
		return err
	}
	m.poll = false
	// Scripts use the same update/command path as the terminal.
	if cmd := m.Init(); cmd != nil {
		next, _ := m.Update(cmd())
		m = next.(Model)
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		text := strings.TrimSpace(scanner.Text())
		switch strings.ToLower(text) {
		case "exit", "quit", "/exit", "/quit":
			return nil
		}
		before := len(m.lines)
		m.setInput(text)
		next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = next.(Model)
		for cmd != nil {
			msg := cmd()
			next, cmd = m.Update(msg)
			m = next.(Model)
		}
		for _, s := range m.lines[before:] {
			if _, err := fmt.Fprintln(out, s); err != nil {
				return err
			}
		}
		if m.restarting {
			return nil
		}
	}
	return scanner.Err()
}
