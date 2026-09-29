package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is bumped whenever the summary format changes incompatibly.
const SchemaVersion = 1

// planFileName is the file name searched for when summarizing a directory.
const planFileName = "tfplan.json"

// summaryFileName is the default summary output name, and the file name
// searched for when rendering a directory.
const summaryFileName = "tfplan-summary.json"

// Summary is the intermediate, value-free representation of one or more plans.
// It never contains attribute values, only addresses, actions and attribute names.
type Summary struct {
	SchemaVersion int           `json:"schema_version"`
	Name          string        `json:"name"`
	JobURL        string        `json:"job_url,omitempty"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Plans         []PlanSummary `json:"plans"`
}

// PlanSummary describes a single plan file. Path is relative to the summarized
// directory ("." when a single file was summarized).
type PlanSummary struct {
	Path             string           `json:"path"`
	TerraformVersion string           `json:"terraform_version"`
	ResourceChanges  []ResourceChange `json:"resource_changes"`
}

// ResourceChange is a single resource change without any values.
type ResourceChange struct {
	Address           string   `json:"address"`
	Actions           []string `json:"actions"`
	ChangedAttributes []string `json:"changed_attributes,omitempty"`
	UnknownAttributes []string `json:"unknown_attributes,omitempty"`
	ReplacePaths      []string `json:"replace_paths,omitempty"`
}

// tfPlan is the subset of `terraform show -json` output that is read.
type tfPlan struct {
	TerraformVersion string             `json:"terraform_version"`
	ResourceChanges  []tfResourceChange `json:"resource_changes"`
}

type tfResourceChange struct {
	Address string   `json:"address"`
	Change  tfChange `json:"change"`
}

type tfChange struct {
	Actions      []string        `json:"actions"`
	Before       interface{}     `json:"before"`
	After        interface{}     `json:"after"`
	AfterUnknown interface{}     `json:"after_unknown"`
	ReplacePaths [][]interface{} `json:"replace_paths"`
}

// Kind returns the display category of a change.
func (rc ResourceChange) Kind() string {
	return actionKind(rc.Actions)
}

func actionKind(actions []string) string {
	switch strings.Join(actions, ",") {
	case "create":
		return "create"
	case "update":
		return "update"
	case "delete":
		return "delete"
	case "delete,create", "create,delete":
		return "replace"
	case "forget":
		return "forget"
	default:
		return strings.Join(actions, ",")
	}
}

// summarize builds a Summary from a plan JSON file or a directory containing
// plan JSON files named tfplan.json.
func summarize(input, name, jobURL string, now time.Time) (*Summary, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, err
	}

	s := &Summary{
		SchemaVersion: SchemaVersion,
		Name:          name,
		JobURL:        jobURL,
		GeneratedAt:   now.UTC().Truncate(time.Second),
		Plans:         []PlanSummary{},
	}

	if !info.IsDir() {
		p, err := summarizeFile(input, ".")
		if err != nil {
			return nil, err
		}
		s.Plans = append(s.Plans, *p)
		return s, nil
	}

	err = filepath.WalkDir(input, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != planFileName {
			return nil
		}
		rel, err := filepath.Rel(input, filepath.Dir(path))
		if err != nil {
			return err
		}
		p, err := summarizeFile(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		s.Plans = append(s.Plans, *p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(s.Plans) == 0 {
		return nil, fmt.Errorf("no %s files found in %s", planFileName, input)
	}

	sort.Slice(s.Plans, func(i, j int) bool { return s.Plans[i].Path < s.Plans[j].Path })
	return s, nil
}

func summarizeFile(path, rel string) (*PlanSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan tfPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	p := &PlanSummary{
		Path:             rel,
		TerraformVersion: plan.TerraformVersion,
		ResourceChanges:  []ResourceChange{},
	}

	for _, rc := range plan.ResourceChanges {
		actions := rc.Change.Actions
		if isNoop(actions) {
			continue
		}
		change := ResourceChange{
			Address: rc.Address,
			Actions: actions,
		}
		kind := actionKind(actions)
		if kind == "update" || kind == "replace" {
			change.ChangedAttributes, change.UnknownAttributes = attributeChanges(rc.Change)
		}
		if kind == "replace" {
			change.ReplacePaths = formatPaths(rc.Change.ReplacePaths)
		}
		p.ResourceChanges = append(p.ResourceChanges, change)
	}

	sort.Slice(p.ResourceChanges, func(i, j int) bool {
		return p.ResourceChanges[i].Address < p.ResourceChanges[j].Address
	})
	return p, nil
}

// isNoop reports whether a change has no effect on infrastructure. Data source
// reads are excluded because they can legitimately differ between two plans of
// the same configuration.
func isNoop(actions []string) bool {
	if len(actions) != 1 {
		return false
	}
	return actions[0] == "no-op" || actions[0] == "read"
}

// attributeChanges returns the top-level attribute names whose values differ
// between before and after, and those whose values are only known after apply.
// Values are compared in memory and never returned.
func attributeChanges(c tfChange) (changed, unknown []string) {
	before, _ := c.Before.(map[string]interface{})
	after, _ := c.After.(map[string]interface{})
	afterUnknown, _ := c.AfterUnknown.(map[string]interface{})

	keys := map[string]struct{}{}
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	for k := range afterUnknown {
		keys[k] = struct{}{}
	}

	for k := range keys {
		if containsTrue(afterUnknown[k]) {
			unknown = append(unknown, k)
			continue
		}
		if !reflect.DeepEqual(before[k], after[k]) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	sort.Strings(unknown)
	return changed, unknown
}

// containsTrue reports whether an after_unknown value marks anything as unknown.
func containsTrue(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]interface{}:
		for _, x := range t {
			if containsTrue(x) {
				return true
			}
		}
	case []interface{}:
		for _, x := range t {
			if containsTrue(x) {
				return true
			}
		}
	}
	return false
}

// formatPaths renders replace_paths such as ["ingress", 0, "cidr"] as "ingress[0].cidr".
func formatPaths(paths [][]interface{}) []string {
	var out []string
	for _, path := range paths {
		var b strings.Builder
		for _, step := range path {
			switch s := step.(type) {
			case string:
				if b.Len() > 0 {
					b.WriteString(".")
				}
				b.WriteString(s)
			case float64:
				b.WriteString("[" + strconv.Itoa(int(s)) + "]")
			default:
				b.WriteString(fmt.Sprintf("[%v]", s))
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	sort.Strings(out)
	return out
}

func loadSummary(path string) (*Summary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema_version %d (want %d)", path, s.SchemaVersion, SchemaVersion)
	}
	return &s, nil
}

// loadSummaries reads summary files from the given files and directories.
// Directories are searched recursively for files named tfplan-summary.json.
func loadSummaries(inputs []string) ([]*Summary, error) {
	var files []string
	for _, in := range inputs {
		info, err := os.Stat(in)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, in)
			continue
		}
		err = filepath.WalkDir(in, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() == summaryFileName {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s files found", summaryFileName)
	}

	var out []*Summary
	for _, f := range files {
		s, err := loadSummary(f)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func encodeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
