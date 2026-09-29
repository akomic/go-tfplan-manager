package main

import (
	"fmt"
	"sort"
	"strings"
)

// change identifies what apply will do to one resource.
type change struct {
	plan    string
	address string
	actions string
}

func (c change) String() string {
	prefix := ""
	if c.plan != "." {
		prefix = c.plan + ": "
	}
	return fmt.Sprintf("%s%s (%s)", prefix, c.address, c.actions)
}

// compareSummaries compares two summaries by plan path, resource address and
// actions. Attribute names are ignored: they depend on unknown values and
// refreshed state, while addresses and actions define what apply will do.
// It returns one line per difference; no lines means the summaries match.
// Lines start with "-" (only in expected) or "+" (only in actual).
func compareSummaries(expected, actual *Summary) []string {
	type entry struct {
		sign string
		c    change
	}
	var entries []entry

	exp, act := changeSet(expected), changeSet(actual)
	for c := range exp {
		if !act[c] {
			entries = append(entries, entry{"-", c})
		}
	}
	for c := range act {
		if !exp[c] {
			entries = append(entries, entry{"+", c})
		}
	}

	// Group by resource so both sides of a changed resource sit together,
	// expected before actual.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.c.plan != b.c.plan {
			return a.c.plan < b.c.plan
		}
		if a.c.address != b.c.address {
			return a.c.address < b.c.address
		}
		return a.sign == "-" && b.sign == "+"
	})

	diff := sortedDiff(planPaths(expected), planPaths(actual))
	for _, e := range entries {
		diff = append(diff, e.sign+" "+e.c.String())
	}
	return diff
}

// hasChanges reports whether any plan in the summary has resource changes.
func hasChanges(s *Summary) bool {
	for _, p := range s.Plans {
		if len(p.ResourceChanges) > 0 {
			return true
		}
	}
	return false
}

// samePlans reports whether both summaries contain the same plan paths.
func samePlans(expected, actual *Summary) bool {
	return len(sortedDiff(planPaths(expected), planPaths(actual))) == 0
}

func changeSet(s *Summary) map[change]bool {
	set := map[change]bool{}
	for _, p := range s.Plans {
		for _, rc := range p.ResourceChanges {
			set[change{plan: p.Path, address: rc.Address, actions: strings.Join(rc.Actions, ",")}] = true
		}
	}
	return set
}

func planPaths(s *Summary) map[string]bool {
	set := map[string]bool{}
	for _, p := range s.Plans {
		set[p.Path] = true
	}
	return set
}

// sortedDiff reports plan paths present on only one side.
func sortedDiff(expected, actual map[string]bool) []string {
	var out []string
	for p := range expected {
		if !actual[p] {
			out = append(out, "- plan "+p)
		}
	}
	for p := range actual {
		if !expected[p] {
			out = append(out, "+ plan "+p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][2:] < out[j][2:] })
	return out
}
