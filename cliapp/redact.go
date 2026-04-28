package cliapp

import "strings"

// DefaultSensitiveFlags lists flag prefixes whose values are redacted from
// the invocation banner by default. Callers can extend this list via
// Options.SensitiveFlags.
var DefaultSensitiveFlags = []string{"--api-key", "--token", "--secret", "--password"}

// RedactArgs returns a copy of args with values of sensitive flags replaced
// by "***". Both --flag=value and --flag value forms are handled.
func RedactArgs(args []string, sensitive []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range out {
		for _, prefix := range sensitive {
			if strings.HasPrefix(arg, prefix+"=") {
				out[i] = prefix + "=***"
				break
			}
			if arg == prefix && i+1 < len(out) {
				out[i+1] = "***"
				break
			}
		}
	}
	return out
}
