package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testItems() []DropdownItem {
	return []DropdownItem{
		{ID: "alpha", Label: "Alpha Feature"},
		{ID: "beta", Label: "Beta Testing"},
		{ID: "gamma", Label: "Gamma Ray"},
		{ID: "delta", Label: "Delta Force"},
		{ID: "epsilon", Label: "Epsilon Eridani"},
	}
}

func TestDropdownFilter_ReducesList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	// Filter for 'al' which should match "Alpha" only
	d.AppendFilter('a')
	d.AppendFilter('l')
	eff := d.effectiveItems()

	// Should only show items containing "al"
	assert.Equal(t, 1, len(eff), "Should filter to only 'Alpha Feature'")
	assert.Contains(t, strings.ToLower(eff[0].Label), "al")
}

func TestDropdownFilter_CaseInsensitive(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('A')
	effUpper := d.effectiveItems()

	d.ClearFilter()
	d.AppendFilter('a')
	effLower := d.effectiveItems()

	assert.Equal(t, len(effUpper), len(effLower))
	for i := range effUpper {
		assert.Equal(t, effUpper[i].ID, effLower[i].ID)
	}
}

func TestDropdownFilter_BackspaceExtendsList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('a')
	d.AppendFilter('l')
	narrowCount := d.effectiveLen()

	d.BackspaceFilter()
	widerCount := d.effectiveLen()

	assert.GreaterOrEqual(t, widerCount, narrowCount)
}

func TestDropdownFilter_EmptyShowsAll(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('x')
	d.BackspaceFilter()

	assert.Equal(t, "", d.FilterText())
	assert.Equal(t, 5, d.effectiveLen())
}

func TestDropdownFilter_SelectionFromFilteredListReturnsCorrectID(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	// Filter to only items containing "eta" -> "Beta Testing", "Delta Force"
	d.AppendFilter('e')
	d.AppendFilter('t')
	d.AppendFilter('a')

	require.Greater(t, d.effectiveLen(), 0)

	// The first filtered item should be selected (index 0 in filtered list)
	item := d.SelectedItem()
	require.NotNil(t, item)
	// The returned item should be from the original items list
	found := false
	for _, orig := range testItems() {
		if orig.ID == item.ID {
			found = true
			assert.Contains(t, strings.ToLower(orig.Label), "eta")
			break
		}
	}
	assert.True(t, found, "Selected item ID should exist in original items")
}

func TestDropdownFilter_OpenResetsFilter(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()
	d.AppendFilter('z')
	assert.Equal(t, "z", d.FilterText())

	d.Close()
	d.Open()
	assert.Equal(t, "", d.FilterText())
	assert.Equal(t, 5, d.effectiveLen())
}

func TestDropdownFilter_SeparatorsExcluded(t *testing.T) {
	items := []DropdownItem{
		{ID: "live1", Label: "Live Session"},
		{ID: "---separator---", Label: "--- History ---"},
		{ID: "hist1", Label: "History Session"},
	}
	d := NewDropdown(items)
	d.Open()

	d.AppendFilter('s') // matches "Live Session" and "History Session"
	eff := d.effectiveItems()
	for _, item := range eff {
		assert.NotEqual(t, "---separator---", item.ID)
	}
}

func TestDropdownFilter_NoMatchesShowsEmptyMessage(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()
	// Use 'x' which doesn't appear in any item label
	d.AppendFilter('x')

	assert.Equal(t, 0, d.effectiveLen(), "Should have no matches for 'x'")
	view := d.ViewList(NewStyles(DefaultDark))
	assert.Contains(t, view, "No matches")
	assert.Contains(t, view, "x")
}

func TestDropdownFilter_ViewShowsFilterIndicator(t *testing.T) {
	d := NewDropdown(testItems())
	d.SetWidth(60)
	d.Open()

	d.AppendFilter('a')
	view := d.ViewList(NewStyles(DefaultDark))
	assert.Contains(t, view, "Filter:")
	assert.Contains(t, view, "a")
}

func TestDropdownFilter_NavigationOnFilteredList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	// Filter to 2 items
	d.AppendFilter('a')
	d.AppendFilter('l')
	effLen := d.effectiveLen()
	require.Greater(t, effLen, 0)

	// Navigate down
	d.MoveSelection(1)
	idx := d.SelectedIndex()
	assert.LessOrEqual(t, idx, effLen-1)

	// Navigate up
	d.MoveSelection(-1)
	assert.GreaterOrEqual(t, d.SelectedIndex(), 0)
}

func TestDropdownFilter_ClearFilterPreservesSelection(t *testing.T) {
	// Items: alpha(0), beta(1), gamma(2), delta(3), epsilon(4)
	d := NewDropdown(testItems())
	d.Open()

	// Filter to items containing "lt" -> delta(orig 3) only matches
	// But let's use "ta" which matches: "Beta Testing"(1), "Delta Force"(3)
	d.AppendFilter('t')
	d.AppendFilter('a')
	require.Equal(t, 2, d.effectiveLen())

	// Move to second filtered item: delta (original index 3)
	d.MoveSelection(1) // index 0->1 in filtered list
	item := d.SelectedItem()
	require.NotNil(t, item)
	assert.Equal(t, "delta", item.ID)

	// Clear filter -- selected item should still be delta
	d.ClearFilter()
	item = d.SelectedItem()
	require.NotNil(t, item)
	assert.Equal(t, "delta", item.ID, "ClearFilter should preserve the selected item from the filtered list")
}
