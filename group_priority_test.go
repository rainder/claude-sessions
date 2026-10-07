package main

import "testing"

func TestGroupPriority(t *testing.T) {
	if !validGroup(groupPriority) || validGroup(0) || validGroup(groupPriority+1) {
		t.Error("validGroup must accept 1..9 and groupPriority only")
	}
	if groupBadgeGlyph(groupPriority) != "⓪" {
		t.Errorf("groupBadgeGlyph(groupPriority) = %q, want ⓪", groupBadgeGlyph(groupPriority))
	}
	if groupSGR[groupPriority] == "" {
		t.Error("groupPriority has no badge color")
	}
	for g := 1; g <= 9; g++ {
		if groupSortRank(groupPriority) >= groupSortRank(g) {
			t.Errorf("groupPriority must rank above group %d", g)
		}
	}
	if groupSortRank(0) <= groupSortRank(9) {
		t.Error("ungrouped must rank below every named group")
	}
	a, b := Session{Group: groupPriority}, Session{Group: 1}
	if !sessionLessGrouped(a, b, "updated", true) || sessionLessGrouped(b, a, "updated", true) {
		t.Error("group-first sort must put the priority group above group 1")
	}
}
