# TODOS

## voice/stt — Phase 2

### Readline + ANSI escape coexistence spike
**What:** Verify that writing ANSI escape sequences to stderr for partial voice text works alongside readline's terminal control without garbling output.
**Why:** The delegator uses readline for input editing. Voice partial text renders below the prompt via stderr ANSI escapes. If these conflict, the fallback is to suppress readline during active voice input — which changes the UX significantly.
**Context:** The delegator's readline instance writes prompts and line-editing to stderr (`Stdout: os.Stderr` in `input.go:43`). Voice partials also go to stderr. The risk is cursor position conflicts. Spike this early in Phase 2 before building the full VoiceInputSource integration. If the spike fails, switch to the fallback approach (temporarily hide readline prompt during voice input).
**Depends on:** Phase 1 complete (voice/stt package exists with streaming events).
