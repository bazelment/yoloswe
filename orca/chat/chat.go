// Package chat provides the owner-facing Bubble Tea conversation.
package chat

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/orca/router"
)

type AskFunc func(context.Context, string) (string, error)
type DispatchFunc func(context.Context, router.Decision, string, func(string)) error
type Snapshot struct {
	Status    string
	Questions []string
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
	restarting           bool
	restartMessage       string
	PollInterval         time.Duration
	initAt               time.Time
	questions            []string
	ctx                  context.Context
	ask                  AskFunc
	router               router.Router
	Dispatch             DispatchFunc
	Backend              Backend
	runID, input, status string
	streamBuf            string
	lines                []string
	queued               []string
	busy, streaming      bool
	width, height        int
	streamLine           int
	// cursor is the input cursor's byte offset.
	cursor int
	// hints are rendered below the prompt.
	hints []string
}

func New(ctx context.Context, runID string, ask AskFunc, r router.Router) Model {
	return Model{ctx: ctx, runID: runID, ask: ask, router: r, poll: true, initAt: time.Now()}
}

// WithTranscript adds earlier conversation lines above new input.
func (m Model) WithTranscript(lines []string) Model {
	m.lines = append(m.lines, lines...)
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
func (m Model) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return time.Second
}
func (m Model) nextPoll() tea.Cmd {
	return tea.Tick(m.pollInterval(), func(time.Time) tea.Msg { return pollMsg{} })
}
func (m Model) Init() tea.Cmd {
	if m.poll {
		return tea.Batch(m.printLines(m.lines...), m.refresh(), m.nextPoll())
	}
	return m.refresh()
}
func (m Model) printLines(blocks ...string) tea.Cmd {
	if !m.poll || len(blocks) == 0 {
		return nil
	}
	var rows []string
	for _, b := range blocks {
		rows = append(rows, wrapPhysicalLines(b, m.width)...)
	}
	if len(rows) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(rows))
	initAt := m.initAt
	first := tea.Println(rows[0])
	cmds = append(cmds, func() tea.Msg {
		if !initAt.IsZero() {
			if wait := 20*time.Millisecond - time.Since(initAt); wait > 0 {
				time.Sleep(wait)
			}
		}
		return first()
	})
	for _, row := range rows[1:] {
		cmds = append(cmds, tea.Println(row))
	}
	return tea.Sequence(cmds...)
}
func (m *Model) flushStream(done bool) []string {
	if done {
		m.streaming = false
		if m.streamBuf == "" {
			return nil
		}
		out := wrapPhysicalLines(m.streamBuf, m.width)
		m.streamBuf = ""
		return out
	}
	var out []string
	if idx := strings.LastIndexByte(m.streamBuf, '\n'); idx >= 0 {
		out = append(out, wrapPhysicalLines(m.streamBuf[:idx], m.width)...)
		m.streamBuf = m.streamBuf[idx+1:]
	}
	if m.width > 1 && displayWidth(m.streamBuf) >= m.width {
		wrapped := wrapPhysicalLines(m.streamBuf, m.width)
		if len(wrapped) > 1 {
			out = append(out, wrapped[:len(wrapped)-1]...)
			m.streamBuf = wrapped[len(wrapped)-1]
		}
	}
	return out
}
func (m Model) submit(text string) (tea.Model, tea.Cmd) {
	userLine := "you> " + text
	m.lines = append(m.lines, userLine)
	m.busy = true
	var runCmd tea.Cmd
	if strings.HasPrefix(text, "/") {
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
	return m, tea.Sequence(m.printLines(userLine), runCmd)
}
func (m Model) finishTurn(printed ...string) (tea.Model, tea.Cmd) {
	m.busy = false
	var nextCmd tea.Cmd
	if len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		var nextModel tea.Model
		nextModel, nextCmd = m.submit(next)
		m = nextModel.(Model)
	}
	return m, tea.Sequence(m.printLines(printed...), nextCmd, m.refresh())
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
		var err error
		if m.Dispatch != nil {
			err = m.Dispatch(m.ctx, d, text, emit)
		} else if d.Handler == router.Concierge && m.ask != nil {
			var s string
			s, err = m.ask(m.ctx, text)
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
	case pollMsg:
		if !m.poll {
			return m, nil
		}
		return m, tea.Batch(m.refresh(), m.nextPoll())
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
	case snapshotMsg:
		if x.err != nil {
			errLine := "error: " + x.err.Error()
			m.lines = append(m.lines, errLine)
			return m, m.printLines(errLine)
		}
		m.status = x.snapshot.Status
		m.questions = x.snapshot.Questions
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
		return m.finishTurn(out)
	case routeMsg:
		if x.err != nil {
			errLine := "error: " + x.err.Error()
			m.lines = append(m.lines, errLine)
			return m.finishTurn(errLine)
		}
		echo := "→ " + string(x.decision.Handler) + ": " + x.text
		if x.decision.Fallback {
			echo += " (via fallback)"
		}
		m.lines = append(m.lines, echo, "orca> ")
		m.streamLine = len(m.lines) - 1
		m.streaming = true
		m.streamBuf = "orca> "
		return m, tea.Sequence(m.printLines(echo), m.dispatch(x.decision, x.text))
	case streamMsg:
		if x.text != "" {
			m.lines[m.streamLine] += x.text
			m.streamBuf += x.text
		}
		if x.done {
			printed := m.flushStream(true)
			if x.err != nil {
				errLine := "error: " + x.err.Error()
				m.lines = append(m.lines, errLine)
				printed = append(printed, errLine)
			}
			return m.finishTurn(printed...)
		}
		printed := m.flushStream(false)
		return m, tea.Sequence(m.printLines(printed...), receive(m.ctx, x.ch))
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

func (m Model) View() tea.View {
	runID := m.runID
	if backend, ok := m.Backend.(*StoreBackend); ok {
		runID = cmp.Or(backend.RunID(), runID)
	}
	stream := ""
	if m.streaming && m.streamBuf != "" {
		stream = "\n" + truncateLine(m.streamBuf, m.width)
	}
	questions := ""
	for _, q := range m.questions {
		questions += "\n" + truncateLine("owner question> "+q, m.width)
	}
	hints := ""
	if len(m.hints) > 0 {
		hints = "\n" + truncateLine("  "+strings.Join(m.hints, "  "), m.width)
	}
	return tea.NewView("orca chat — run " + runID + stream + questions + "\n\n> " + m.inputView() + hints + m.statusFooter())
}
func (m Model) statusFooter() string {
	if m.status == "" {
		return ""
	}
	raw := strings.Split(m.status, "\n")
	if len(raw) > 2 {
		raw = raw[:2]
	}
	out := make([]string, len(raw))
	for i, line := range raw {
		out[i] = "\x1b[2m" + truncateLine(line, m.width) + "\x1b[22m"
	}
	return "\n" + strings.Join(out, "\n")
}
func runeDisplayWidth(r rune) int {
	if r < 0x80 {
		return 1
	}
	return 2
}
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeDisplayWidth(r)
	}
	return w
}
func wrapPhysicalLines(s string, width int) []string {
	raw := strings.Split(s, "\n")
	if width <= 1 {
		return raw
	}
	limit := width - 1
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" || displayWidth(line) <= limit {
			out = append(out, line)
			continue
		}
		var b strings.Builder
		w := 0
		for _, r := range line {
			rw := runeDisplayWidth(r)
			if w > 0 && w+rw > limit {
				out = append(out, b.String())
				b.Reset()
				w = 0
			}
			b.WriteRune(r)
			w += rw
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return out
}
func truncateLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}
func isTerminal(in io.Reader) bool { f, ok := in.(*os.File); return ok && term.IsTerminal(int(f.Fd())) }
func Run(ctx context.Context, in io.Reader, out io.Writer, m Model) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.ctx = ctx
	if isTerminal(in) {
		m.poll = true
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
