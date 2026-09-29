package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxCommentLength keeps rendered output under GitHub's 65536 character limit
// for issue and pull request comments, with room for a marker line.
const maxCommentLength = 60000

// kindInfo describes how one kind of change is rendered. Sections are listed
// destructive first. The diff prefix sets the colour in GitHub's diff syntax
// highlighting: "-" red, "+" green, "!" orange.
type kindInfo struct {
	kind    string
	verb    string // "to delete"
	heading string // "Resources to delete"
	prefix  string // diff line prefix
}

var kinds = []kindInfo{
	{"delete", "to delete", "Resources to delete", "-"},
	{"replace", "to replace", "Resources to replace", "-/+"},
	{"create", "to create", "Resources to create", "+"},
	{"update", "to update", "Resources to update", "!"},
	{"forget", "to forget", "Resources to forget (removed from state, not destroyed)", "#"},
}

// planView is one plan to render, labelled with its summary name and path.
type planView struct {
	label   string
	jobURL  string
	changes []ResourceChange
}

// detailLevel controls how much is rendered, reduced until the comment fits.
type detailLevel int

const (
	levelFull       detailLevel = iota // resource lists and attribute details
	levelResources                     // resource lists only
	levelCountsOnly                    // per-plan counts only
)

// render builds a single markdown comment from any number of summaries.
// Each plan with changes is a collapsible section with one diff block per
// kind of change; attribute details for updates and replacements sit in a
// further collapsible under their block. Detail is dropped as needed to stay
// under GitHub's comment size limit.
func render(summaries []*Summary, title string, now time.Time) string {
	var out string
	for _, level := range []detailLevel{levelFull, levelResources, levelCountsOnly} {
		out = renderMarkdown(summaries, title, now, level)
		if len(out) <= maxCommentLength {
			break
		}
	}
	return out
}

func renderMarkdown(summaries []*Summary, title string, now time.Time, level detailLevel) string {
	var withChanges, withoutChanges []planView
	versions := map[string]struct{}{}

	for _, s := range summaries {
		for _, p := range s.Plans {
			if p.TerraformVersion != "" {
				versions[p.TerraformVersion] = struct{}{}
			}
			v := planView{label: planLabel(s.Name, p.Path), jobURL: s.JobURL, changes: p.ResourceChanges}
			if len(p.ResourceChanges) == 0 {
				withoutChanges = append(withoutChanges, v)
			} else {
				withChanges = append(withChanges, v)
			}
		}
	}
	sort.Slice(withChanges, func(i, j int) bool { return withChanges[i].label < withChanges[j].label })
	sort.Slice(withoutChanges, func(i, j int) bool { return withoutChanges[i].label < withoutChanges[j].label })

	var md strings.Builder
	md.WriteString("## " + title + "\n\n")

	if len(withChanges) == 0 {
		md.WriteString("✅ **No changes.** Infrastructure matches the configuration.\n\n")
	} else {
		var all []ResourceChange
		for _, v := range withChanges {
			all = append(all, v.changes...)
		}
		md.WriteString(fmt.Sprintf("**%d of %d plans have changes:** %s.\n\n",
			len(withChanges), len(withChanges)+len(withoutChanges), countLine(all)))

		// A single plan is shown expanded; several start collapsed.
		open := len(withChanges) == 1
		for _, v := range withChanges {
			writePlan(&md, v, open, level)
		}
		switch level {
		case levelResources:
			md.WriteString("_Attribute details omitted to fit the comment size limit. See the linked jobs for full plans._\n\n")
		case levelCountsOnly:
			md.WriteString("_Resource lists omitted to fit the comment size limit. See the linked jobs for full plans._\n\n")
		}
	}

	// Plans without changes use the same line as plans with changes.
	for _, v := range withoutChanges {
		writeUnchangedPlan(&md, v)
	}

	md.WriteString(footer(versions, now))
	return md.String()
}

func planLabel(name, path string) string {
	switch {
	case name == "" && path == ".":
		return "root"
	case name == "":
		return path
	case path == ".":
		return name
	default:
		return name + "/" + path
	}
}

// planHeader is the summary line of a plan's collapsible section.
func planHeader(icon, label, status string) string {
	return fmt.Sprintf("<summary>%s <b><code>%s</code></b> · %s</summary>", icon, escapeHTML(label), status)
}

func writeUnchangedPlan(md *strings.Builder, v planView) {
	md.WriteString("<details>\n" + planHeader("✅", v.label, "no changes") + "\n\n")
	if v.jobURL != "" {
		md.WriteString(fmt.Sprintf("[View plan job](%s)\n\n", v.jobURL))
	} else {
		md.WriteString("Infrastructure matches the configuration.\n\n")
	}
	md.WriteString("</details>\n\n")
}

// groupByKind groups changes by kind, each group sorted by address, and
// returns the kinds in display order: known kinds first, then any others.
func groupByKind(changes []ResourceChange) ([]kindInfo, map[string][]ResourceChange) {
	groups := map[string][]ResourceChange{}
	for _, rc := range changes {
		groups[rc.Kind()] = append(groups[rc.Kind()], rc)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].Address < g[j].Address })
	}

	var order []kindInfo
	known := map[string]bool{}
	for _, k := range kinds {
		known[k.kind] = true
		if len(groups[k.kind]) > 0 {
			order = append(order, k)
		}
	}
	var others []string
	for k := range groups {
		if !known[k] {
			others = append(others, k)
		}
	}
	sort.Strings(others)
	for _, k := range others {
		order = append(order, kindInfo{kind: k, verb: "with actions " + k, heading: "Resources with actions " + k, prefix: " "})
	}
	return order, groups
}

// countLine renders "1 to delete, 2 to update", skipping zero counts.
func countLine(changes []ResourceChange) string {
	order, groups := groupByKind(changes)
	parts := make([]string, len(order))
	for i, k := range order {
		parts[i] = fmt.Sprintf("%d %s", len(groups[k.kind]), k.verb)
	}
	return strings.Join(parts, ", ")
}

func writePlan(md *strings.Builder, v planView, open bool, level detailLevel) {
	openAttr := ""
	if open {
		openAttr = " open"
	}
	// Markdown inside <details> needs the blank lines around it.
	md.WriteString(fmt.Sprintf("<details%s>\n%s\n\n", openAttr, planHeader("📁", v.label, countLine(v.changes))))
	if v.jobURL != "" {
		md.WriteString(fmt.Sprintf("[View plan job](%s)\n\n", v.jobURL))
	}

	if level == levelCountsOnly {
		md.WriteString("</details>\n\n")
		return
	}

	order, groups := groupByKind(v.changes)
	for _, k := range order {
		group := groups[k.kind]
		md.WriteString("**" + k.heading + ":**\n\n```diff\n")
		for _, rc := range group {
			md.WriteString(k.prefix + " " + rc.Address + replaceOrder(rc) + "\n")
		}
		md.WriteString("```\n\n")

		if level == levelFull {
			writeAttributeDetails(md, group)
		}
	}
	md.WriteString("</details>\n\n")
}

// writeAttributeDetails adds a collapsible list of which attributes change
// for each resource in a group that has any. Values are never shown.
func writeAttributeDetails(md *strings.Builder, group []ResourceChange) {
	var lines []string
	for _, rc := range group {
		if d := detailLines(rc); len(d) > 0 {
			lines = append(lines, "- `"+rc.Address+"`: "+strings.Join(d, "; "))
		}
	}
	if len(lines) == 0 {
		return
	}
	md.WriteString("<details>\n<summary>Attribute changes</summary>\n\n")
	md.WriteString(strings.Join(lines, "\n") + "\n\n</details>\n\n")
}

// replaceOrder marks replacements that create the new object before
// destroying the old one.
func replaceOrder(rc ResourceChange) string {
	if strings.Join(rc.Actions, ",") == "create,delete" {
		return " (create before destroy)"
	}
	return ""
}

// detailLines lists which attributes change, never their values. Attributes
// already named as forcing replacement are not repeated as changed.
func detailLines(rc ResourceChange) []string {
	var lines []string
	if len(rc.ReplacePaths) > 0 {
		lines = append(lines, "replaced because of "+codeList(rc.ReplacePaths))
	}
	if changed := withoutReplacePaths(rc.ChangedAttributes, rc.ReplacePaths); len(changed) > 0 {
		lines = append(lines, "changed "+codeList(changed))
	}
	if len(rc.UnknownAttributes) > 0 {
		lines = append(lines, "computed during apply "+codeList(rc.UnknownAttributes))
	}
	return lines
}

// withoutReplacePaths drops attributes that are, or contain, a replace path:
// "rules" is dropped for the path "rules[0].from".
func withoutReplacePaths(attrs, paths []string) []string {
	var out []string
	for _, a := range attrs {
		covered := false
		for _, p := range paths {
			if p == a || strings.HasPrefix(p, a+".") || strings.HasPrefix(p, a+"[") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, a)
		}
	}
	return out
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = "`" + it + "`"
	}
	return strings.Join(quoted, ", ")
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}

func footer(versions map[string]struct{}, now time.Time) string {
	var vs []string
	for v := range versions {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	ts := now.UTC().Format("2006-01-02 15:04:05 UTC")
	if len(vs) == 0 {
		return fmt.Sprintf("<sub>Generated %s</sub>\n", ts)
	}
	return fmt.Sprintf("<sub>Terraform %s · Generated %s</sub>\n", strings.Join(vs, ", "), ts)
}
