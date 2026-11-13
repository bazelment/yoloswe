package store_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestMemoryStoreContract(t *testing.T) {
	t.Parallel()

	store.RunContractTests(t, func(_ *testing.T, clk clock.Clock) store.Store {
		return store.NewMemoryStore(clk)
	})
}

func TestMemoryStoreConcurrency(t *testing.T) {
	t.Parallel()

	st := store.NewMemoryStore(nil)
	defer st.Close()
	ctx := t.Context()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-conc", Objective: "concurrency test"}); err != nil {
		t.Fatalf("Create run error: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := st.Events().Append(ctx, store.Event{
				RunID:   "run-conc",
				Kind:    store.EventTimer,
				Summary: fmt.Sprintf("tick-%d", idx),
			})
			if err != nil {
				t.Errorf("Append event %d: %v", idx, err)
			}
			_, err = st.Decisions().Append(ctx, store.Decision{
				RunID: "run-conc",
				Actor: "controller",
				Tool:  "noop",
			})
			if err != nil {
				t.Errorf("Append decision %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-conc"})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	if len(evs) != workers {
		t.Fatalf("len(evs) = %d, want %d", len(evs), workers)
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}
