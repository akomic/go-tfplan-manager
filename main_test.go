package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func mustSummarize(t *testing.T, input, name, jobURL string) *Summary {
	t.Helper()
	s, err := summarize(input, name, jobURL, fixedTime)
	if err != nil {
		t.Fatalf("summarize(%s): %v", input, err)
	}
	return s
}

func TestSummarizeFile(t *testing.T) {
	s := mustSummarize(t, "testdata/plan.json", "stack/app", "https://ci.example/job/1")

	if s.SchemaVersion != SchemaVersion || s.Name != "stack/app" || s.JobURL != "https://ci.example/job/1" {
		t.Fatalf("unexpected metadata: %+v", s)
	}
	if !s.GeneratedAt.Equal(fixedTime) {
		t.Errorf("GeneratedAt = %v, want %v", s.GeneratedAt, fixedTime)
	}
	if len(s.Plans) != 1 || s.Plans[0].Path != "." || s.Plans[0].TerraformVersion != "1.15.9" {
		t.Fatalf("unexpected plans: %+v", s.Plans)
	}

	want := []ResourceChange{
		{Address: "example_bucket.logs", Actions: []string{"create"}},
		{Address: "example_instance.web", Actions: []string{"update"}, ChangedAttributes: []string{"size", "tags"}, UnknownAttributes: []string{"arn"}},
		{Address: "example_queue.old", Actions: []string{"delete"}},
		{Address: "module.net.example_rule.ingress[0]", Actions: []string{"delete", "create"}, ChangedAttributes: []string{"cidr"}, UnknownAttributes: []string{"id"}, ReplacePaths: []string{"cidr", "rules[0].from"}},
		{Address: "random_password.db", Actions: []string{"update"}, ChangedAttributes: []string{"length", "result"}},
	}
	if got := s.Plans[0].ResourceChanges; !reflect.DeepEqual(got, want) {
		t.Errorf("resource changes mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestSummaryContainsNoValues(t *testing.T) {
	s := mustSummarize(t, "testdata/plan.json", "stack/app", "")
	var buf bytes.Buffer
	if err := encodeJSON(&buf, s); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, secret := range []string{"SECRET", "10.0.0.0/8", "10.1.0.0/16", "small", "large", "platform", "arn:example"} {
		if strings.Contains(out, secret) {
			t.Errorf("summary leaks value %q:\n%s", secret, out)
		}
	}
}

func TestSummarizeDirectory(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/plan.json", filepath.Join(dir, "tfplan.json"))
	copyFile(t, "testdata/noop-plan.json", filepath.Join(dir, "child", "tfplan.json"))
	copyFile(t, "testdata/plan.json", filepath.Join(dir, "child", "ignored.json"))

	s := mustSummarize(t, dir, "stack", "")
	if len(s.Plans) != 2 || s.Plans[0].Path != "." || s.Plans[1].Path != "child" {
		t.Fatalf("unexpected plans: %+v", s.Plans)
	}
	if len(s.Plans[1].ResourceChanges) != 0 {
		t.Errorf("no-op plan should have no changes, got %+v", s.Plans[1].ResourceChanges)
	}
}

func TestSummarizeEmptyDirectory(t *testing.T) {
	if _, err := summarize(t.TempDir(), "x", "", fixedTime); err == nil {
		t.Fatal("expected an error for a directory without plans")
	}
}

func TestCompare(t *testing.T) {
	a := mustSummarize(t, "testdata/plan.json", "a", "")
	b := mustSummarize(t, "testdata/plan.json", "b", "https://ci.example/other")

	if diff := compareSummaries(a, b); len(diff) != 0 {
		t.Errorf("identical plans should match, got %v", diff)
	}

	// Attribute names are ignored.
	b.Plans[0].ResourceChanges[1].ChangedAttributes = []string{"size"}
	if diff := compareSummaries(a, b); len(diff) != 0 {
		t.Errorf("attribute differences should be ignored, got %v", diff)
	}

	// Actions and addresses are not.
	b.Plans[0].ResourceChanges[1].Actions = []string{"delete", "create"}
	b.Plans[0].ResourceChanges = append(b.Plans[0].ResourceChanges, ResourceChange{Address: "example_extra.x", Actions: []string{"create"}})
	want := []string{
		"+ example_extra.x (create)",
		"- example_instance.web (update)",
		"+ example_instance.web (delete,create)",
	}
	if diff := compareSummaries(a, b); !reflect.DeepEqual(diff, want) {
		t.Errorf("diff mismatch\n got: %q\nwant: %q", diff, want)
	}
}

func TestCompareMissingPlan(t *testing.T) {
	a := mustSummarize(t, "testdata/noop-plan.json", "a", "")
	b := &Summary{SchemaVersion: SchemaVersion, Name: "a", Plans: []PlanSummary{}}
	diff := compareSummaries(a, b)
	if !reflect.DeepEqual(diff, []string{"- plan ."}) {
		t.Errorf("expected missing plan to be reported, got %q", diff)
	}
}

func TestRender(t *testing.T) {
	changed := mustSummarize(t, "testdata/plan.json", "stack/app", "https://ci.example/job/1")
	unchanged := mustSummarize(t, "testdata/noop-plan.json", "stack/db", "https://ci.example/job/2")

	md := render([]*Summary{unchanged, changed}, "Plan", fixedTime)

	for _, want := range []string{
		"## Plan\n",
		"**1 of 2 plans have changes:** 1 to delete, 1 to replace, 1 to create, 2 to update.",
		"<details open>\n<summary>📁 <b><code>stack/app</code></b> · 1 to delete, 1 to replace, 1 to create, 2 to update</summary>",
		"[View plan job](https://ci.example/job/1)",
		"**Resources to delete:**\n\n```diff\n- example_queue.old\n```",
		"**Resources to replace:**\n\n```diff\n-/+ module.net.example_rule.ingress[0]\n```",
		"**Resources to create:**\n\n```diff\n+ example_bucket.logs\n```",
		"**Resources to update:**\n\n```diff\n! example_instance.web\n! random_password.db\n```",
		"- `example_instance.web`: changed `size`, `tags`; computed during apply `arn`",
		"- `module.net.example_rule.ingress[0]`: replaced because of `cidr`, `rules[0].from`; computed during apply `id`",
		"<details>\n<summary>✅ <b><code>stack/db</code></b> · no changes</summary>\n\n[View plan job](https://ci.example/job/2)",
		"<sub>Terraform 1.15.9 · Generated 2026-01-02 03:04:05 UTC</sub>",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "SECRET") {
		t.Errorf("rendered markdown leaks a value:\n%s", md)
	}
	// Creates and deletes have no attribute details, so only update and
	// replace get an "Attribute changes" collapsible.
	if n := strings.Count(md, "<summary>Attribute changes</summary>"); n != 2 {
		t.Errorf("expected 2 attribute collapsibles, got %d\n%s", n, md)
	}
}

func TestRenderSeveralPlansStartCollapsed(t *testing.T) {
	a := mustSummarize(t, "testdata/plan.json", "stack/a", "")
	b := mustSummarize(t, "testdata/plan.json", "stack/b", "")
	md := render([]*Summary{b, a}, "Plan", fixedTime)
	if strings.Contains(md, "<details open>") {
		t.Errorf("several changed plans should start collapsed:\n%s", md)
	}
	if !strings.Contains(md, "**2 of 2 plans have changes:** 2 to delete, 2 to replace, 2 to create, 4 to update.") {
		t.Errorf("totals should sum both plans:\n%s", md)
	}
	if strings.Index(md, "stack/a") > strings.Index(md, "stack/b") {
		t.Errorf("plans should be sorted by label:\n%s", md)
	}
}

func TestCreateBeforeDestroyAndForget(t *testing.T) {
	s := &Summary{SchemaVersion: SchemaVersion, Name: "x", Plans: []PlanSummary{{Path: ".", ResourceChanges: []ResourceChange{
		{Address: "a.cbd", Actions: []string{"create", "delete"}, ReplacePaths: []string{"name"}},
		{Address: "a.gone", Actions: []string{"forget"}},
		{Address: "a.odd", Actions: []string{"create", "update"}},
	}}}}
	md := render([]*Summary{s}, "Plan", fixedTime)
	for _, want := range []string{
		"-/+ a.cbd (create before destroy)",
		"**Resources to forget (removed from state, not destroyed):**\n\n```diff\n# a.gone\n```",
		"**Resources with actions create,update:**",
		"1 to replace, 1 to forget, 1 with actions create,update",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q\n%s", want, md)
		}
	}
}

func TestDetailLines(t *testing.T) {
	cases := []struct {
		rc   ResourceChange
		want string
	}{
		{ResourceChange{Actions: []string{"create"}}, ""},
		{ResourceChange{Actions: []string{"update"}, ChangedAttributes: []string{"tags"}}, "changed `tags`"},
		{ResourceChange{Actions: []string{"delete", "create"}, ChangedAttributes: []string{"name", "tags"}, ReplacePaths: []string{"name"}}, "replaced because of `name`; changed `tags`"},
		{ResourceChange{Actions: []string{"delete", "create"}, ChangedAttributes: []string{"rules", "rulesx"}, ReplacePaths: []string{"rules[0].from"}}, "replaced because of `rules[0].from`; changed `rulesx`"},
		{ResourceChange{Actions: []string{"delete", "create"}, ChangedAttributes: []string{"block"}, ReplacePaths: []string{"block.inner"}, UnknownAttributes: []string{"id"}}, "replaced because of `block.inner`; computed during apply `id`"},
	}
	for _, c := range cases {
		if got := strings.Join(detailLines(c.rc), "; "); got != c.want {
			t.Errorf("detailLines(%+v) = %q, want %q", c.rc, got, c.want)
		}
	}
}

func TestRenderEscapesLabels(t *testing.T) {
	s := mustSummarize(t, "testdata/noop-plan.json", "a<b>&\"c", "")
	md := render([]*Summary{s}, "Plan", fixedTime)
	if !strings.Contains(md, "<code>a&lt;b&gt;&amp;&quot;c</code>") {
		t.Errorf("label not escaped:\n%s", md)
	}
}

func TestRenderNoChanges(t *testing.T) {
	s := mustSummarize(t, "testdata/noop-plan.json", "stack/db", "")
	md := render([]*Summary{s}, "Plan", fixedTime)
	if !strings.Contains(md, "✅ **No changes.**") || !strings.Contains(md, "<summary>✅ <b><code>stack/db</code></b> · no changes</summary>") {
		t.Errorf("expected no-changes message and plan line:\n%s", md)
	}
}

func TestRenderShrinksToFit(t *testing.T) {
	withUpdates := func(n int) *Summary {
		s := mustSummarize(t, "testdata/plan.json", "stack/app", "")
		for i := 0; i < n; i++ {
			s.Plans[0].ResourceChanges = append(s.Plans[0].ResourceChanges, ResourceChange{
				Address:           fmt.Sprintf("example_bucket.generated_%05d", i),
				Actions:           []string{"update"},
				ChangedAttributes: []string{"tags", "versioning", "lifecycle_rule"},
			})
		}
		return s
	}

	// Attribute details dropped first.
	md := render([]*Summary{withUpdates(900)}, "Plan", fixedTime)
	if len(md) > maxCommentLength || !strings.Contains(md, "Attribute details omitted") || !strings.Contains(md, "! example_bucket.generated_00001") {
		t.Errorf("expected resource lists without attribute details (len %d)", len(md))
	}

	// Then resource lists.
	md = render([]*Summary{withUpdates(3000)}, "Plan", fixedTime)
	if len(md) > maxCommentLength || !strings.Contains(md, "Resource lists omitted") || strings.Contains(md, "generated_00001") {
		t.Errorf("expected counts only (len %d)", len(md))
	}
	if !strings.Contains(md, "3002 to update") {
		t.Errorf("counts should still be shown:\n%s", md)
	}
}

func TestCLI(t *testing.T) {
	now = func() time.Time { return fixedTime }
	defer func() { now = time.Now }()

	dir := t.TempDir()
	pr := filepath.Join(dir, "pr.json")
	main := filepath.Join(dir, "main.json")
	noop := filepath.Join(dir, "noop.json")
	comment := filepath.Join(dir, "comment.md")

	var stdout, stderr bytes.Buffer
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"summarize", "-name", "stack/app", "-job-url", "https://ci.example/job/1", "-o", pr, "testdata/plan.json"}, 0},
		{[]string{"summarize", "-name", "stack/app", "-o", main, "testdata/plan.json"}, 0},
		{[]string{"summarize", "-name", "stack/app", "-o", noop, "testdata/noop-plan.json"}, 0},
		{[]string{"compare", pr, main}, 0},
		{[]string{"compare", pr, noop}, 3},
		{[]string{"compare", noop, pr}, 1},
		{[]string{"compare", pr, filepath.Join(dir, "missing.json")}, 2},
		{[]string{"render", "-o", comment, pr}, 0},
		{[]string{"render", t.TempDir()}, 1},
		{[]string{"summarize"}, 2},
		{[]string{"bogus"}, 2},
		{[]string{"version"}, 0},
	}
	for _, c := range cases {
		stdout.Reset()
		stderr.Reset()
		if code := run(c.args, &stdout, &stderr); code != c.code {
			t.Errorf("run(%v) = %d, want %d\nstdout: %s\nstderr: %s", c.args, code, c.code, stdout.String(), stderr.String())
		}
	}

	var s Summary
	data, err := os.ReadFile(pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.JobURL != "https://ci.example/job/1" {
		t.Errorf("job URL not recorded: %+v", s)
	}
	if md, _ := os.ReadFile(comment); !strings.Contains(string(md), "[View plan job](https://ci.example/job/1)") {
		t.Errorf("comment missing job link:\n%s", md)
	}
}

func TestSummarizeDefaultOutputAndRenderDirectory(t *testing.T) {
	now = func() time.Time { return fixedTime }
	defer func() { now = time.Now }()

	root := t.TempDir()
	plan, err := filepath.Abs("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"env/network", "env/cluster"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		var stdout, stderr bytes.Buffer
		if code := run([]string{"summarize", "-name", name, plan}, &stdout, &stderr); code != 0 {
			t.Fatalf("summarize %s: exit %d: %s", name, code, stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, summaryFileName)); err != nil {
			t.Fatalf("default output not written: %v", err)
		}
	}
	// Unrelated JSON files in the tree are ignored.
	if err := os.WriteFile(filepath.Join(root, "env", "other.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	summaries, err := loadSummaries([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || summaries[0].Name != "env/cluster" || summaries[1].Name != "env/network" {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}
}

func TestResolveJobURL(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	full := map[string]string{
		"GITHUB_ACTIONS":     "true",
		"GITHUB_SERVER_URL":  "https://github.example/",
		"GITHUB_REPOSITORY":  "org/repo",
		"GITHUB_RUN_ID":      "10",
		"GITHUB_RUN_ATTEMPT": "3",
	}
	noAttempt := map[string]string{}
	for k, v := range full {
		noAttempt[k] = v
	}
	delete(noAttempt, "GITHUB_RUN_ATTEMPT")
	missingRepo := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "10"}

	cases := []struct {
		explicit, jobID string
		env             map[string]string
		want            string
	}{
		{"https://ci.example/x", "5", full, "https://ci.example/x"},
		{"", "5", full, "https://github.example/org/repo/actions/runs/10/job/5"},
		{"", "", full, "https://github.example/org/repo/actions/runs/10/attempts/3"},
		{"", "", noAttempt, "https://github.example/org/repo/actions/runs/10"},
		{"", "5", missingRepo, ""},
		{"", "5", map[string]string{}, ""},
	}
	for _, c := range cases {
		if got := resolveJobURL(c.explicit, c.jobID, env(c.env)); got != c.want {
			t.Errorf("resolveJobURL(%q, %q, %v) = %q, want %q", c.explicit, c.jobID, c.env, got, c.want)
		}
	}
}

func TestLoadSummaryRejectsUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 99, "plans": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSummary(path); err == nil {
		t.Fatal("expected an error for an unknown schema version")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
