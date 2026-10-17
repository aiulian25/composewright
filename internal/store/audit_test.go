package store

import (
	"context"
	"slices"
	"testing"
)

// TestAuditPage: entries come newest first, filtered by action and project,
// and a page that starts before the last one seen goes on where it ended.
func TestAuditPage(t *testing.T) {
	store, _, _ := openTestStore(t)
	ctx := context.Background()
	for _, event := range []struct{ action, project string }{
		{ActionLogin, ""}, {ActionReveal, "first"}, {ActionSettings, ""}, {ActionReveal, "second"}, {ActionReveal, "first"},
	} {
		if err := store.Audit(ctx, "web:admin", event.action, event.project, map[string]any{"key": "TOKEN"}); err != nil {
			t.Fatal(err)
		}
	}
	sequences := func(filter AuditFilter) []int64 {
		t.Helper()
		entries, err := store.AuditPage(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		var numbers []int64
		for _, entry := range entries {
			numbers = append(numbers, entry.Sequence)
		}
		return numbers
	}
	for _, check := range []struct {
		name   string
		filter AuditFilter
		want   []int64
	}{
		{"everything", AuditFilter{}, []int64{5, 4, 3, 2, 1}},
		{"one action", AuditFilter{Action: ActionReveal}, []int64{5, 4, 2}},
		{"one project", AuditFilter{ProjectID: "first"}, []int64{5, 2}},
		{"first page", AuditFilter{Limit: 2}, []int64{5, 4}},
		{"next page", AuditFilter{Before: 4, Limit: 2}, []int64{3, 2}},
		{"next page of one action", AuditFilter{Action: ActionReveal, Before: 4}, []int64{2}},
	} {
		if got := sequences(check.filter); !slices.Equal(got, check.want) {
			t.Errorf("%s: %v, want %v", check.name, got, check.want)
		}
	}
	actions, err := store.AuditActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{ActionLogin, ActionReveal, ActionSettings}; !slices.Equal(actions, want) {
		t.Fatalf("actions %v, want %v", actions, want)
	}
	projects, err := store.AuditProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"first", "second"}; !slices.Equal(projects, want) {
		t.Fatalf("projects %v, want %v", projects, want)
	}
}
