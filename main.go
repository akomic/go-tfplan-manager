package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Version information - set during build
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// now and getenv are replaced in tests.
var (
	now    = time.Now
	getenv = os.Getenv
)

const usage = `go-tfplan-manager summarizes, compares and renders Terraform plans.

Usage:
  go-tfplan-manager summarize [flags] <tfplan.json | dir>
  go-tfplan-manager render [flags] <summary.json | dir>...
  go-tfplan-manager compare <expected-summary.json> <actual-summary.json>
  go-tfplan-manager version

Commands:
  summarize  Convert "terraform show -json" output into a value-free summary.
             A directory is searched recursively for tfplan.json files.
  render     Compile one or more summaries into a single markdown comment.
  compare    Compare two summaries by resource address and actions.
             Exits 0 when they match, 1 when they differ, 2 on error,
             3 when they differ only because actual has no changes.
  version    Print version information.

Run "go-tfplan-manager <command> -h" for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "summarize":
		return runSummarize(args[1:], stdout, stderr)
	case "render":
		return runRender(args[1:], stdout, stderr)
	case "compare":
		return runCompare(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "go-tfplan-manager %s (commit %s, built %s)\n", Version, GitCommit, BuildDate)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "label for this plan in rendered comments (default: the input path)")
	jobURL := fs.String("job-url", "", "URL of the CI job that produced the plan (default: built from GitHub Actions environment variables)")
	jobID := fs.String("job-id", "", "numeric GitHub Actions job ID (${{ job.check_run_id }}) to link the exact job instead of the run")
	output := fs.String("o", summaryFileName, "output file, - for stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "summarize: expected exactly one input")
		return 2
	}

	input := fs.Arg(0)
	label := *name
	if label == "" {
		label = strings.TrimSuffix(input, "/")
	}

	s, err := summarize(input, label, resolveJobURL(*jobURL, *jobID, getenv), now())
	if err != nil {
		fmt.Fprintf(stderr, "summarize: %v\n", err)
		return 1
	}
	if err := writeOutput(*output, stdout, func(w io.Writer) error { return encodeJSON(w, s) }); err != nil {
		fmt.Fprintf(stderr, "summarize: %v\n", err)
		return 1
	}
	return 0
}

func runRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	title := fs.String("title", "📋 Terraform Plan Summary", "comment heading")
	output := fs.String("o", "-", "output file, - for stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "render: expected at least one summary file or directory")
		return 2
	}

	summaries, err := loadSummaries(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "render: %v\n", err)
		return 1
	}
	md := render(summaries, *title, now())
	if err := writeOutput(*output, stdout, func(w io.Writer) error {
		_, err := io.WriteString(w, md)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "render: %v\n", err)
		return 1
	}
	return 0
}

func runCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "compare: expected <expected-summary.json> <actual-summary.json>")
		return 2
	}

	expected, err := loadSummary(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "compare: %v\n", err)
		return 2
	}
	actual, err := loadSummary(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "compare: %v\n", err)
		return 2
	}

	diff := compareSummaries(expected, actual)
	if len(diff) == 0 {
		fmt.Fprintln(stdout, "Plans match.")
		return 0
	}
	if !hasChanges(actual) && samePlans(expected, actual) {
		fmt.Fprintln(stdout, "Actual plan has no changes; expected changes were already applied or resolved.")
		return 3
	}
	fmt.Fprintln(stdout, "Plans differ (- expected only, + actual only):")
	for _, line := range diff {
		fmt.Fprintln(stdout, line)
	}
	return 1
}

// resolveJobURL returns the explicit URL if given. Otherwise, inside GitHub
// Actions, it builds a link to the job (when jobID is set) or to the run
// attempt from the default environment variables. GitHub does not expose the
// numeric job ID as an environment variable, so it has to be passed in.
func resolveJobURL(explicit, jobID string, getenv func(string) string) string {
	if explicit != "" {
		return explicit
	}
	if getenv("GITHUB_ACTIONS") != "true" {
		return ""
	}
	server, repo, run := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || run == "" {
		return ""
	}
	base := strings.TrimSuffix(server, "/") + "/" + repo + "/actions/runs/" + run
	if jobID != "" {
		return base + "/job/" + jobID
	}
	if attempt := getenv("GITHUB_RUN_ATTEMPT"); attempt != "" {
		return base + "/attempts/" + attempt
	}
	return base
}

func writeOutput(path string, stdout io.Writer, write func(io.Writer) error) error {
	if path == "" || path == "-" {
		return write(stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
