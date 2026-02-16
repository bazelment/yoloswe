package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestToastManagerAdd(t *testing.T) {
	tm := NewToastManager()
	tm.Add("hello", ToastSuccess)
	assert.Equal(t, 1, tm.Count())
	assert.True(t, tm.HasToasts())
}

func TestToastManagerMaxStack(t *testing.T) {
	tm := NewToastManager()
	for i := 0; i < 5; i++ {
		tm.Add(fmt.Sprintf("toast %d", i), ToastInfo)
	}
	// Should only keep 3
	assert.Equal(t, maxToasts, tm.Count())
	// Oldest should be evicted: "toast 2", "toast 3", "toast 4" remain
	assert.Equal(t, "toast 2", tm.toasts[0].Message)
	assert.Equal(t, "toast 4", tm.toasts[2].Message)
}

func TestToastExpiry(t *testing.T) {
	tm := NewToastManager()
	// Manually set CreatedAt in the past
	tm.toasts = append(tm.toasts, Toast{
		Message:   "old",
		Level:     ToastSuccess,
		CreatedAt: time.Now().Add(-10 * time.Second),
		Duration:  3 * time.Second,
	})
	tm.toasts = append(tm.toasts, Toast{
		Message:   "new",
		Level:     ToastSuccess,
		CreatedAt: time.Now(),
		Duration:  3 * time.Second,
	})

	changed := tm.Tick(time.Now())
	assert.True(t, changed)
	assert.Equal(t, 1, tm.Count()) // "old" expired, "new" remains
	assert.Equal(t, "new", tm.toasts[0].Message)
}

func TestToastIsExpired(t *testing.T) {
	toast := Toast{
		CreatedAt: time.Now().Add(-5 * time.Second),
		Duration:  3 * time.Second,
	}
	assert.True(t, toast.IsExpired(time.Now()))

	toast2 := Toast{
		CreatedAt: time.Now(),
		Duration:  3 * time.Second,
	}
	assert.False(t, toast2.IsExpired(time.Now()))
}

func TestToastHeight(t *testing.T) {
	tm := NewToastManager()
	assert.Equal(t, 0, tm.Height())

	tm.Add("a", ToastSuccess)
	assert.Equal(t, 1, tm.Height())

	tm.Add("b", ToastError)
	assert.Equal(t, 2, tm.Height())
}

func TestToastRendering(t *testing.T) {
	tm := NewToastManager()
	tm.SetWidth(80)

	// No toasts -> empty string
	assert.Equal(t, "", tm.View(NewStyles(Dark)))

	// Success toast
	tm.Add("Worktree created", ToastSuccess)
	view := tm.View(NewStyles(Dark))
	assert.Contains(t, view, "Worktree created")
	assert.Contains(t, view, "✓") // success icon

	// Error toast
	tm.Add("Failed to start session", ToastError)
	view = tm.View(NewStyles(Dark))
	assert.Contains(t, view, "Failed to start session")
	assert.Contains(t, view, "!") // error icon
}

func TestToastDurationByLevel(t *testing.T) {
	tm := NewToastManager()

	tm.Add("success", ToastSuccess)
	assert.Equal(t, 3*time.Second, tm.toasts[0].Duration)

	tm.toasts = nil
	tm.Add("info", ToastInfo)
	assert.Equal(t, 4*time.Second, tm.toasts[0].Duration)

	tm.toasts = nil
	tm.Add("error", ToastError)
	assert.Equal(t, 5*time.Second, tm.toasts[0].Duration)
}

func TestToastSmallWidth(t *testing.T) {
	tm := NewToastManager()
	tm.SetWidth(5) // Very small width -- should not panic
	tm.Add("a long message that exceeds the width", ToastError)
	view := tm.View(NewStyles(Dark))
	assert.NotEmpty(t, view) // Should render without panic
}

func TestToastViaModelUpdate(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil)

	// Simulate an error message
	newModel, cmd := m.Update(errMsg{fmt.Errorf("test error")})
	m2 := newModel.(Model)

	// Should have a toast
	assert.True(t, m2.toasts.HasToasts())
	assert.Equal(t, 1, m2.toasts.Count())

	// cmd should be a tea.Tick for expiry
	assert.NotNil(t, cmd)

	// View should contain the error message
	view := m2.View()
	assert.Contains(t, view, "test error")
}
