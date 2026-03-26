# readline-voice-spike

Manual spike binary to verify that ANSI escape sequences for partial voice text
coexist with `ergochat/readline` terminal control without garbling output.

This is the prerequisite for voice/stt Phase 2: integrating the STT session
into the delegator's `VoiceInputSource`.

## How to run

Must be run on a real terminal (not in a pipe or CI):

```
bazel run //bramble/cmd/readline-voice-spike
```

The program:
1. Opens a readline prompt (`>>> `), mirroring the delegator's config.
2. Starts a fake STT event stream: two utterances with realistic partial-text
   timing (~120ms between partials).
3. Renders partial text on the line below the prompt using DECSC/DECRC.
4. After each utterance, clears the partial line and calls `rl.Refresh()`.
5. Exits ~3 seconds after the second utterance ends.

While it runs, type freely — test arrow keys, Backspace, and multi-character
edits to exercise readline's cursor management alongside the voice writes.

## ANSI approach being tested

**DECSC/DECRC save/restore cursor** (VT100, xterm-compatible):

```
\0337  — DECSC: save cursor position
\n     — move down one line (below the prompt)
\033[K — erase to end of line
\033[2m<text>\033[0m  — dim partial text
\0338  — DECRC: restore cursor to where readline left it
```

A `sync.Mutex` serializes all stderr writes between readline and the voice
rendering goroutine to prevent interleaving.

## Success criteria

The spike **passes** if all of the following hold throughout the full session:

1. The readline prompt (`>>> `) remains visible and at its expected screen
   position during and between all partial text updates.
2. The user can type characters, use Backspace, and press arrow keys normally —
   no garbled characters appear in the input line.
3. Partial text appears on the line below the prompt and is replaced cleanly
   on each update (no residue from previous partials).
4. After each `EventFinalText`, the partial line disappears and the prompt
   redraws cleanly.
5. The second utterance cycle behaves identically to the first.

## Failure criteria

The spike **fails** if any of the following occur:

- Partial text overwrites or collides with the readline prompt line.
- The prompt drifts downward over time as partials accumulate.
- After a partial update, readline's redraw leaves duplicate or partial prompt
  artifacts above or below the expected position.
- Arrow keys or Backspace produce unexpected output in the input area.

## Fallback plan

If the spike fails, the fallback is to suppress the readline prompt during
active speech and restore it after the utterance ends:

1. On `EventSpeechStart`: call `SetPrompt("")` and `rl.Clean()` to quiet
   readline visually.
2. Render partials using the spinner's existing `\r\033[K` pattern (safe
   because readline is not painting the cursor).
3. On `EventFinalText` / `EventSpeechEnd`: call `SetPrompt(">>> ")` and
   `RefreshPrompt()` to restore.

This is slightly worse UX (prompt disappears during speech) but is safe and
proven by the spinner's existing approach in `spinner.go`.

## Alternative ANSI approaches

| Approach | Notes |
|----------|-------|
| `\033[s` / `\033[u` (ANSI SC/RC) | Same concept as DECSC/DECRC; some terminals support only one variant — try if DECSC causes issues |
| Terminal title bar (`\033]0;...\007`) | Sidesteps cursor positioning entirely; visually subtle |
| Absolute cursor positioning (`\033[row;colH`) | More robust on some terminals; requires tracking terminal size and resize events |
