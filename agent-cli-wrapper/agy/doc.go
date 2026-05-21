// Package agy wraps the Antigravity CLI's non-interactive print mode.
//
// The agy CLI currently exposes no model-selection flag. Higher-level routing
// treats gemini-family model IDs as compatibility aliases for agy's default
// model rather than passing those IDs to the subprocess.
package agy
