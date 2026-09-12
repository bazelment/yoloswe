package state

import (
	"reflect"
	"strings"
)

// structJSONKeys returns the JSON key for each exported field of v that carries
// one. Deriving the known-key set from struct tags keeps it from drifting away
// from the fields it is supposed to describe.
func structJSONKeys(v any) []string {
	t := reflect.TypeOf(v)
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			keys = append(keys, name)
		}
	}
	return keys
}
