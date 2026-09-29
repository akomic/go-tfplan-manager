package main

// End-to-end tests: real Terraform plans run through the compiled binary.
//
// They use only the built-in terraform_data resource, so no providers are
// downloaded and no network access is needed. TF_BINARY selects the binary
// (default "terraform", e.g. "tofu" for OpenTofu). Tests are skipped when it is
// not on PATH, unless E2E_REQUIRED=1 (set in CI), in which case they fail.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var binaryPath string

// tfBinary returns the Terraform-compatible binary used by the e2e tests.
func tfBinary() string {
	if b := os.Getenv("TF_BINARY"); b != "" {
		return b
	}
	return "terraform"
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "go-tfplan-manager-e2e")
	if err != nil {
		panic(err)
	}
	binaryPath = filepath.Join(dir, "go-tfplan-manager")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	// Tests must not pick up the GitHub Actions environment of CI itself.
	getenv = func(k string) string {
		if strings.HasPrefix(k, "GITHUB_") {
			return ""
		}
		return os.Getenv(k)
	}

	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("building binary: " + err.Error())
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// workspace is a Terraform root module in a temporary directory.
type workspace struct {
	t   *testing.T
	dir string
}

func newWorkspace(t *testing.T, config string) *workspace {
	t.Helper()
	if _, err := exec.LookPath(tfBinary()); err != nil {
		if os.Getenv("E2E_REQUIRED") == "1" {
			t.Fatalf("%s not found on PATH and E2E_REQUIRED=1", tfBinary())
		}
		t.Skipf("%s not found on PATH", tfBinary())
	}
	w := &workspace{t: t, dir: t.TempDir()}
	w.write(config)
	w.tf("init", "-input=false", "-no-color")
	return w
}

func (w *workspace) write(config string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "main.tf"), []byte(config), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *workspace) tf(args ...string) string {
	w.t.Helper()
	cmd := exec.Command(tfBinary(), args...)
	cmd.Dir = w.dir
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		w.t.Fatalf("%s %s: %v\n%s%s", tfBinary(), strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String()
}

func (w *workspace) apply() {
	w.t.Helper()
	w.tf("apply", "-auto-approve", "-input=false", "-no-color")
}

// plan writes a saved plan and its JSON, returning the JSON path.
func (w *workspace) plan(name string) string {
	w.t.Helper()
	planFile := name + ".tfplan"
	w.tf("plan", "-input=false", "-no-color", "-out="+planFile)
	jsonPath := filepath.Join(w.dir, name+".json")
	if err := os.WriteFile(jsonPath, []byte(w.tf("show", "-json", planFile)), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return jsonPath
}

func (w *workspace) applyPlan(name string) {
	w.t.Helper()
	w.tf("apply", "-auto-approve", "-input=false", "-no-color", name+".tfplan")
}

// cli runs the compiled binary and returns stdout, stderr and the exit code.
func cli(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath, args...)
	cmd.Dir = dir
	cmd.Env = withoutGitHubEnv(os.Environ())
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return out.String(), errOut.String(), code
}

func mustCLI(t *testing.T, dir string, want int, args ...string) string {
	t.Helper()
	out, errOut, code := cli(t, dir, args...)
	if code != want {
		t.Fatalf("go-tfplan-manager %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errOut)
	}
	return out
}

func readSummary(t *testing.T, path string) *Summary {
	t.Helper()
	s, err := loadSummary(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func changesByAddress(s *Summary) map[string]ResourceChange {
	m := map[string]ResourceChange{}
	for _, p := range s.Plans {
		for _, rc := range p.ResourceChanges {
			m[rc.Address] = rc
		}
	}
	return m
}

const baseConfig = `
resource "terraform_data" "update" { input = "v1" }
resource "terraform_data" "replace" { triggers_replace = "r1" }
resource "terraform_data" "cbd" {
  triggers_replace = "c1"
  lifecycle { create_before_destroy = true }
}
resource "terraform_data" "delete" { input = "gone" }
resource "terraform_data" "forget" { input = "keep" }
resource "terraform_data" "secret" { input = sensitive("hunter2-before") }
`

const changedConfig = `
resource "terraform_data" "update" { input = "v2" }
resource "terraform_data" "replace" { triggers_replace = "r2" }
resource "terraform_data" "cbd" {
  triggers_replace = "c2"
  lifecycle { create_before_destroy = true }
}
removed {
  from = terraform_data.forget
  lifecycle { destroy = false }
}
resource "terraform_data" "secret" { input = sensitive("hunter2-after") }
resource "terraform_data" "create" { input = terraform_data.update.output }
`

// TestE2ESummarizeAndRenderAllActions covers every action Terraform can plan,
// and checks that no value, sensitive or not, reaches the summary or comment.
func TestE2ESummarizeAndRenderAllActions(t *testing.T) {
	w := newWorkspace(t, baseConfig)
	w.apply()
	w.write(changedConfig)
	planJSON := w.plan("pr")

	if data, _ := os.ReadFile(planJSON); !bytes.Contains(data, []byte("hunter2")) {
		t.Fatal("test setup: expected the raw plan JSON to contain the sensitive value")
	}

	mustCLI(t, w.dir, 0, "summarize", "-name", "env/app", "-job-url", "https://ci.example/runs/1/job/2", planJSON)
	summaryPath := filepath.Join(w.dir, summaryFileName)
	s := readSummary(t, summaryPath)

	if s.Name != "env/app" || s.JobURL != "https://ci.example/runs/1/job/2" || s.GeneratedAt.IsZero() {
		t.Errorf("unexpected metadata: %+v", s)
	}
	if !strings.HasPrefix(s.Plans[0].TerraformVersion, "1.") {
		t.Errorf("terraform version not recorded: %q", s.Plans[0].TerraformVersion)
	}

	got := changesByAddress(s)
	want := map[string]string{
		"terraform_data.create":  "create",
		"terraform_data.update":  "update",
		"terraform_data.secret":  "update",
		"terraform_data.replace": "delete,create",
		"terraform_data.cbd":     "create,delete",
		"terraform_data.delete":  "delete",
		"terraform_data.forget":  "forget",
	}
	if len(got) != len(want) {
		t.Errorf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for addr, actions := range want {
		rc, ok := got[addr]
		if !ok {
			t.Errorf("missing change for %s", addr)
			continue
		}
		if a := strings.Join(rc.Actions, ","); a != actions {
			t.Errorf("%s actions = %s, want %s", addr, a, actions)
		}
	}

	if rc := got["terraform_data.update"]; !contains(rc.ChangedAttributes, "input") || !contains(rc.UnknownAttributes, "output") {
		t.Errorf("update attributes = %+v", rc)
	}
	if rc := got["terraform_data.secret"]; !contains(rc.ChangedAttributes, "input") {
		t.Errorf("sensitive attribute change not reported by name: %+v", rc)
	}
	for _, addr := range []string{"terraform_data.replace", "terraform_data.cbd"} {
		if rc := got[addr]; !contains(rc.ReplacePaths, "triggers_replace") {
			t.Errorf("%s replace paths = %v", addr, rc.ReplacePaths)
		}
	}

	md := mustCLI(t, w.dir, 0, "render", summaryPath)
	for _, want := range []string{
		"<summary>📁 <b><code>env/app</code></b> · 1 to delete, 2 to replace, 1 to create, 2 to update, 1 to forget</summary>",
		"[View plan job](https://ci.example/runs/1/job/2)",
		"```diff\n- terraform_data.delete\n```",
		"```diff\n-/+ terraform_data.cbd (create before destroy)\n-/+ terraform_data.replace\n```",
		"```diff\n+ terraform_data.create\n```",
		"```diff\n! terraform_data.secret\n! terraform_data.update\n```",
		"```diff\n# terraform_data.forget\n```",
		"- `terraform_data.replace`: replaced because of `triggers_replace`; computed during apply `id`",
		"- `terraform_data.update`: changed `input`; computed during apply `output`",
		"<sub>Terraform 1.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("comment missing %q\n%s", want, md)
		}
	}

	summaryBytes, _ := os.ReadFile(summaryPath)
	for _, leaked := range []string{"hunter2", "\"v1\"", "\"v2\"", "r1", "r2", "gone", "keep"} {
		if bytes.Contains(summaryBytes, []byte(leaked)) {
			t.Errorf("summary leaks value %s", leaked)
		}
		if strings.Contains(md, leaked) {
			t.Errorf("comment leaks value %s", leaked)
		}
	}
}

// TestE2ENoChanges: an up-to-date root module.
func TestE2ENoChanges(t *testing.T) {
	w := newWorkspace(t, baseConfig)
	w.apply()
	pr := w.plan("pr")
	main := w.plan("main")

	mustCLI(t, w.dir, 0, "summarize", "-name", "env/app", "-o", "pr-summary.json", pr)
	mustCLI(t, w.dir, 0, "summarize", "-name", "env/app", "-o", "main-summary.json", main)

	if s := readSummary(t, filepath.Join(w.dir, "pr-summary.json")); hasChanges(s) {
		t.Errorf("expected no changes, got %+v", s.Plans)
	}
	if out := mustCLI(t, w.dir, 0, "compare", "pr-summary.json", "main-summary.json"); !strings.Contains(out, "Plans match") {
		t.Errorf("unexpected compare output: %s", out)
	}
	if md := mustCLI(t, w.dir, 0, "render", "pr-summary.json"); !strings.Contains(md, "✅ **No changes.**") {
		t.Errorf("unexpected comment:\n%s", md)
	}
}

// TestE2EReplanMatches: the deploy-time plan of unchanged code and state
// matches the reviewed plan, including values known only after apply.
func TestE2EReplanMatches(t *testing.T) {
	w := newWorkspace(t, baseConfig)
	w.apply()
	w.write(changedConfig)
	pr := w.plan("pr")
	main := w.plan("main")

	mustCLI(t, w.dir, 0, "summarize", "-o", "pr-summary.json", pr)
	mustCLI(t, w.dir, 0, "summarize", "-o", "main-summary.json", main)
	mustCLI(t, w.dir, 0, "compare", "pr-summary.json", "main-summary.json")
}

// TestE2EChangedUnderneath: state changes between review and deploy, so the
// deploy-time plan differs and compare fails.
func TestE2EChangedUnderneath(t *testing.T) {
	w := newWorkspace(t, `
resource "terraform_data" "a" { input = "one" }
resource "terraform_data" "b" { input = "two" }
`)
	w.apply()
	w.write(`
resource "terraform_data" "a" { input = "one-changed" }
resource "terraform_data" "b" { input = "two" }
`)
	pr := w.plan("pr")

	// Someone removes b from state outside the pipeline.
	w.tf("state", "rm", "terraform_data.b")
	main := w.plan("main")

	mustCLI(t, w.dir, 0, "summarize", "-o", "pr-summary.json", pr)
	mustCLI(t, w.dir, 0, "summarize", "-o", "main-summary.json", main)
	out := mustCLI(t, w.dir, 1, "compare", "pr-summary.json", "main-summary.json")
	if !strings.Contains(out, "+ terraform_data.b (create)") {
		t.Errorf("diff should show the unexpected create:\n%s", out)
	}
	if strings.Contains(out, "terraform_data.a") {
		t.Errorf("matching change should not be reported:\n%s", out)
	}
}

// TestE2EAlreadyApplied: the reviewed plan was applied before deploy, so the
// deploy-time plan is empty and compare exits 3.
func TestE2EAlreadyApplied(t *testing.T) {
	w := newWorkspace(t, baseConfig)
	w.apply()
	w.write(changedConfig)
	pr := w.plan("pr")
	w.applyPlan("pr")
	main := w.plan("main")

	mustCLI(t, w.dir, 0, "summarize", "-o", "pr-summary.json", pr)
	mustCLI(t, w.dir, 0, "summarize", "-o", "main-summary.json", main)
	mustCLI(t, w.dir, 3, "compare", "pr-summary.json", "main-summary.json")
}

// TestE2EUnexpectedChanges: the reviewed plan had no changes but the
// deploy-time plan does. That is a mismatch, not exit 3.
func TestE2EUnexpectedChanges(t *testing.T) {
	w := newWorkspace(t, baseConfig)
	w.apply()
	pr := w.plan("pr")
	w.tf("state", "rm", "terraform_data.delete")
	main := w.plan("main")

	mustCLI(t, w.dir, 0, "summarize", "-o", "pr-summary.json", pr)
	mustCLI(t, w.dir, 0, "summarize", "-o", "main-summary.json", main)
	out := mustCLI(t, w.dir, 1, "compare", "pr-summary.json", "main-summary.json")
	if !strings.Contains(out, "+ terraform_data.delete (create)") {
		t.Errorf("unexpected diff:\n%s", out)
	}
}

// TestE2EMultiComponentComment mirrors CI: one summary per component, written
// into a tree like the one downloaded from storage, rendered into one comment.
func TestE2EMultiComponentComment(t *testing.T) {
	network := newWorkspace(t, `resource "terraform_data" "vpc" { input = "a" }`)
	network.apply()
	network.write(`resource "terraform_data" "vpc" { input = "b" }`)
	networkPlan := network.plan("pr")

	cluster := newWorkspace(t, `resource "terraform_data" "nodes" { input = "a" }`)
	cluster.apply()
	clusterPlan := cluster.plan("pr")

	tree := t.TempDir()
	for name, plan := range map[string]string{"env/network": networkPlan, "env/cluster": clusterPlan} {
		dir := filepath.Join(tree, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustCLI(t, dir, 0, "summarize", "-name", name, "-job-url", "https://ci.example/"+name, plan)
	}
	// Unrelated files in the downloaded tree are ignored.
	if err := os.WriteFile(filepath.Join(tree, "env", "notes.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustCLI(t, tree, 0, "render", "-title", "Plan", "-o", "comment.md", ".")
	md, err := os.ReadFile(filepath.Join(tree, "comment.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Plan",
		"**1 of 2 plans have changes:** 1 to update.",
		"<summary>📁 <b><code>env/network</code></b> · 1 to update</summary>",
		"[View plan job](https://ci.example/env/network)",
		"- `terraform_data.vpc`: changed `input`; computed during apply `output`",
		"<summary>✅ <b><code>env/cluster</code></b> · no changes</summary>\n\n[View plan job](https://ci.example/env/cluster)",
		"· Generated ",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("comment missing %q\n%s", want, md)
		}
	}

	// An empty tree is an error, so a CI step can't silently post nothing.
	if _, _, code := cli(t, t.TempDir(), "render", "."); code != 1 {
		t.Errorf("render of an empty tree: exit %d, want 1", code)
	}
}

// TestE2ESummarizeDirectory: several plan JSON files under one directory, as
// written by "terragrunt plan --all --json-out-dir".
func TestE2ESummarizeDirectory(t *testing.T) {
	a := newWorkspace(t, `resource "terraform_data" "x" { input = "1" }`)
	aPlan := a.plan("pr")
	b := newWorkspace(t, `resource "terraform_data" "y" { input = "1" }`)
	b.apply()
	bPlan := b.plan("pr")

	tree := t.TempDir()
	copyFile(t, aPlan, filepath.Join(tree, "unit-a", planFileName))
	copyFile(t, bPlan, filepath.Join(tree, "unit-b", planFileName))

	mustCLI(t, tree, 0, "summarize", "-name", "env", "-o", "s.json", tree)
	s := readSummary(t, filepath.Join(tree, "s.json"))
	if len(s.Plans) != 2 || s.Plans[0].Path != "unit-a" || s.Plans[1].Path != "unit-b" {
		t.Fatalf("unexpected plans: %+v", s.Plans)
	}
	if len(s.Plans[0].ResourceChanges) != 1 || len(s.Plans[1].ResourceChanges) != 0 {
		t.Errorf("unexpected changes: %+v", s.Plans)
	}

	mustCLI(t, tree, 0, "compare", "s.json", "s.json")
	md := mustCLI(t, tree, 0, "render", "s.json")
	if !strings.Contains(md, "<code>env/unit-a</code>") || !strings.Contains(md, "<code>env/unit-b</code>") {
		t.Errorf("plan labels missing:\n%s", md)
	}
}

// TestE2EJobURLFromEnvironment: inside GitHub Actions the link is built from
// the default environment variables, to the exact job when -job-id is given.
func TestE2EJobURLFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	plan, _ := filepath.Abs("testdata/noop-plan.json")
	run := func(extraEnv []string, args ...string) string {
		cmd := exec.Command(binaryPath, append([]string{"summarize", "-o", "-"}, append(args, plan)...)...)
		cmd.Dir = dir
		cmd.Env = append(withoutGitHubEnv(os.Environ()), extraEnv...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}
		var s Summary
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatal(err)
		}
		return s.JobURL
	}
	gh := []string{"GITHUB_ACTIONS=true", "GITHUB_SERVER_URL=https://github.com", "GITHUB_REPOSITORY=org/repo", "GITHUB_RUN_ID=123", "GITHUB_RUN_ATTEMPT=2"}

	if got := run(gh, "-job-id", "456"); got != "https://github.com/org/repo/actions/runs/123/job/456" {
		t.Errorf("job URL = %q", got)
	}
	if got := run(gh); got != "https://github.com/org/repo/actions/runs/123/attempts/2" {
		t.Errorf("run URL = %q", got)
	}
	if got := run(gh, "-job-url", "https://ci.example/x", "-job-id", "456"); got != "https://ci.example/x" {
		t.Errorf("explicit URL should win, got %q", got)
	}
	if got := run(nil, "-job-id", "456"); got != "" {
		t.Errorf("outside GitHub Actions no URL is built, got %q", got)
	}
}

// TestE2EErrors checks error handling and exit codes of the binary.
func TestE2EErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"schema_version": 1, "name": "x", "plans": [{"path": ".", "resource_changes": []}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(dir, "future.json")
	if err := os.WriteFile(future, []byte(`{"schema_version": 99, "plans": []}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		args []string
		code int
	}{
		{[]string{}, 2},
		{[]string{"nope"}, 2},
		{[]string{"summarize"}, 2},
		{[]string{"summarize", "missing.json"}, 1},
		{[]string{"summarize", "bad.json"}, 1},
		{[]string{"summarize", "-o", filepath.Join(dir, "no", "such", "dir", "s.json"), "testdata"}, 1},
		{[]string{"compare", "good.json"}, 2},
		{[]string{"compare", "good.json", "bad.json"}, 2},
		{[]string{"compare", "good.json", future}, 2},
		{[]string{"render"}, 2},
		{[]string{"render", "bad.json"}, 1},
		{[]string{"version"}, 0},
		{[]string{"help"}, 0},
	}
	for _, c := range cases {
		if _, stderr, code := cli(t, dir, c.args...); code != c.code {
			t.Errorf("go-tfplan-manager %v: exit %d, want %d (stderr: %s)", c.args, code, c.code, stderr)
		}
	}

	// Output to stdout is valid JSON.
	plan, _ := filepath.Abs("testdata/plan.json")
	out := mustCLI(t, dir, 0, "summarize", "-o", "-", plan)
	var s Summary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Errorf("stdout is not a summary: %v\n%s", err, out)
	}
}

func withoutGitHubEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GITHUB_") {
			out = append(out, kv)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
