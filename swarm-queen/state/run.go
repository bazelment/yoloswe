package state

import (
	"encoding/json"
	"fmt"
)

// Phase is one step in a lane's lifecycle, with the model that runs it.
type Phase struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// Config is the run-level contract. Shared with ledger.py's "config" object.
//
// Like Lane, it retains unmodelled keys so a round-trip through swarm-queen
// never deletes a field another tool wrote.
type Config struct {
	extra  map[string]json.RawMessage `json:"-"`
	Goal   string                     `json:"goal"`
	Base   string                     `json:"base"`
	Target string                     `json:"target"`
	Phases []Phase                    `json:"phases"`
}

type configAlias Config

// UnmarshalJSON decodes the config and retains any unmodelled keys.
func (c *Config) UnmarshalJSON(b []byte) error {
	var alias configAlias
	if err := json.Unmarshal(b, &alias); err != nil {
		return err
	}
	*c = Config(alias)

	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, known := range structJSONKeys(Config{}) {
		delete(all, known)
	}
	if len(all) > 0 {
		c.extra = all
	}
	return nil
}

// MarshalJSON re-emits retained unknown keys alongside the modelled ones.
func (c Config) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(configAlias(c))
	if err != nil {
		return nil, err
	}
	if len(c.extra) == 0 {
		return b, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range c.extra {
		if _, clash := merged[k]; clash {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// State is the whole ledger: <run>/state.json.
type State struct {
	Config Config  `json:"config"`
	Lanes  []*Lane `json:"tasks"` // "tasks" is ledger.py's key; do not rename
}

// PhaseNames returns the ordered phase names.
func (c *Config) PhaseNames() []string {
	names := make([]string, len(c.Phases))
	for i, p := range c.Phases {
		names[i] = p.Name
	}
	return names
}

// Lane returns the lane with the given id.
func (s *State) Lane(id string) (*Lane, bool) {
	for _, l := range s.Lanes {
		if l.ID == id {
			return l, true
		}
	}
	return nil, false
}

// NextPhase returns the phase after the given one, and whether one exists.
// An empty current phase yields the first phase.
func (c *Config) NextPhase(current string) (string, bool) {
	names := c.PhaseNames()
	if len(names) == 0 {
		return "", false
	}
	if current == "" {
		return names[0], true
	}
	for i, n := range names {
		if n == current && i+1 < len(names) {
			return names[i+1], true
		}
	}
	return "", false
}

// Ready returns dependency-ready planned lanes, highest priority first. This is
// the dispatch queue.
func (s *State) Ready() []*Lane {
	done := make(map[string]bool, len(s.Lanes))
	for _, l := range s.Lanes {
		if l.Status == StatusDone {
			done[l.ID] = true
		}
	}
	var ready []*Lane
	for _, l := range s.Lanes {
		if l.Status != StatusPlanned {
			continue
		}
		blocked := false
		for _, dep := range l.DependsOn {
			if !done[dep] {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, l)
		}
	}
	stableSortByPriority(ready)
	return ready
}

// NonTerminal returns lanes still needing work. A run is not finished while this
// is non-empty — one real run was shut down with 6 of 21 lanes non-terminal,
// three of them p1 lanes that were never staffed at all.
func (s *State) NonTerminal() []*Lane {
	var out []*Lane
	for _, l := range s.Lanes {
		if !l.Status.Terminal() {
			out = append(out, l)
		}
	}
	return out
}

// Validate checks the whole ledger for structural problems.
func (s *State) Validate() error {
	seen := make(map[string]bool, len(s.Lanes))
	for _, l := range s.Lanes {
		if err := l.Validate(); err != nil {
			return err
		}
		if seen[l.ID] {
			return fmt.Errorf("duplicate lane id %q", l.ID)
		}
		seen[l.ID] = true
	}
	return nil
}
