# go-tfplan-manager

Summarize, compare and render Terraform plans in CI.

`go-tfplan-manager` turns `terraform show -json` output into a small summary
that contains **no attribute values**: only resource addresses, actions and the
names of attributes that change. Summaries are safe to store as CI artifacts,
can be compiled into a single pull request comment, and can be compared to
confirm that the plan being applied matches the plan that was reviewed.

## Install

Download a binary for your platform from the
[releases page](https://github.com/akomic/go-tfplan-manager/releases) and verify
it against `checksums.txt`:

```bash
VERSION=v0.1.0
BIN=go-tfplan-manager-linux-amd64
curl -sSLO "https://github.com/akomic/go-tfplan-manager/releases/download/${VERSION}/${BIN}"
curl -sSLO "https://github.com/akomic/go-tfplan-manager/releases/download/${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
install -m 0755 "$BIN" /usr/local/bin/go-tfplan-manager
```

In CI, pin the SHA-256 of the binary rather than trusting a downloaded
`checksums.txt`.

## Usage

### summarize

```bash
terraform plan -out tfplan
terraform show -json tfplan > tfplan.json
go-tfplan-manager summarize -name network -job-url "$JOB_URL" tfplan.json
# writes ./tfplan-summary.json
```

A directory is searched recursively for files named `tfplan.json`; each becomes
a separate plan in the summary, keyed by its relative path.

| Flag | Description |
|------|-------------|
| `-name` | Label shown in rendered comments (default: the input path) |
| `-job-url` | Link to the CI job that produced the plan (default: built from the GitHub Actions environment, see below) |
| `-job-id` | Numeric GitHub Actions job ID, `${{ job.check_run_id }}`, to link the exact job |
| `-o` | Output file, `-` for stdout (default: `tfplan-summary.json`) |

Inside GitHub Actions, when `-job-url` is not given, the link is built from
`GITHUB_SERVER_URL`, `GITHUB_REPOSITORY`, `GITHUB_RUN_ID` and
`GITHUB_RUN_ATTEMPT`. GitHub does not expose the numeric job ID as an
environment variable, so pass `-job-id "${{ job.check_run_id }}"` to link the
job itself; without it the link points to the run.

No-op changes and data source reads are left out. For updates and
replacements the summary lists changed attribute names, attributes known only
after apply, and the attribute paths that force replacement.

### render

```bash
go-tfplan-manager render -o comment.md summaries/
```

Compiles any number of summary files into one markdown comment:

- an overall count of changes across all plans;
- one collapsible section per plan, with a link to its job. Plans with
  changes list their resources in colour-coded `diff` blocks per action
  (delete, replace, create, update, forget), destructive actions first, with
  a collapsible "Attribute changes" list under updates and replacements;
- a footer with the Terraform version and the time the comment was generated.

A single plan with changes is shown expanded; with several, each starts
collapsed. If the comment would exceed GitHub's size limit, attribute details
are dropped first, then resource lists, leaving per-plan counts.

Arguments can be summary files or directories. Directories are searched
recursively for files named `tfplan-summary.json`, so a tree such as
`summaries/<env>/<component>/tfplan-summary.json` can be passed as is.

| Flag | Description |
|------|-------------|
| `-title` | Comment heading |
| `-o` | Output file (default: stdout) |

### compare

```bash
go-tfplan-manager compare reviewed-summary.json current-summary.json
```

Compares resource addresses and actions per plan. Attribute names are ignored,
since they can differ between two plans of the same configuration (unknown
values, refreshed state) without changing what apply will do.

| Exit code | Meaning |
|-----------|---------|
| 0 | Plans match |
| 1 | Plans differ; differences are printed with `-` (expected only) and `+` (actual only) |
| 2 | Usage or input error |
| 3 | Plans differ only because the actual plan has no changes (the expected changes were already applied or resolved) |

A difference in the other direction (expected no changes, actual has changes)
is a normal mismatch and exits 1.

## Example comment

[`examples/comment.md`](examples/comment.md) is a comment rendered from real
plans: one root module with every kind of change, one without changes. Open it
on GitHub to see it rendered.

## Using it in GitHub Actions

Two example workflows show the whole flow, with summaries stored in S3:

- [`examples/workflows/plan.yml`](examples/workflows/plan.yml): on a pull
  request, each root module is planned in its own job, summarized, and its
  summary stored under `pr-<number>/<module>/tfplan-summary.json`. A final job
  downloads all summaries, renders one comment and posts it, updating the
  previous one on later pushes.
- [`examples/workflows/apply.yml`](examples/workflows/apply.yml): on merge,
  each module is planned again, summarized, and compared with its reviewed
  summary. The job applies on exit code 0, skips with a warning on 3, and
  fails on anything else. A mismatch fails only that module; the others apply.

The plan is made and applied in the same job, so no plan file, which contains
state, is passed between jobs. Only summaries are stored.

Any storage works: summaries are small JSON files without values. Replace the
`aws s3 cp` steps with `actions/upload-artifact` and `actions/download-artifact`
(with `run-id` and `github-token` to read the pull request's run), or anything
else.

### Pinning the binary

Pin both the release version and the SHA-256 of the binary (from the release's
`checksums.txt`) in the workflow, as the examples do. The checksum lives in
your repository, so a replaced release asset fails the install instead of
running.

## OpenTofu and Terragrunt

OpenTofu writes the same JSON format:

```bash
tofu plan -out tfplan
tofu show -json tfplan > tfplan.json
go-tfplan-manager summarize -name network tfplan.json
```

With Terragrunt, for a single unit:

```bash
terragrunt plan -out=tfplan
terragrunt show -json tfplan > tfplan.json
```

For several units at once, write the JSON plans into a directory and summarize
the directory; each unit becomes a plan keyed by its path:

```bash
terragrunt run --all --out-dir plans --json-out-dir plans -- plan
go-tfplan-manager summarize -name staging plans
```

Note that with dependencies between units, `run --all plan` plans dependents
against the current outputs of their dependencies, before those are applied.

## Summary format

```json
{
  "schema_version": 1,
  "name": "network",
  "job_url": "https://github.com/org/repo/actions/runs/1/job/2",
  "generated_at": "2026-01-02T03:04:05Z",
  "plans": [
    {
      "path": ".",
      "terraform_version": "1.15.9",
      "resource_changes": [
        {
          "address": "aws_security_group_rule.ingress",
          "actions": ["delete", "create"],
          "changed_attributes": ["cidr_blocks"],
          "unknown_attributes": ["id"],
          "replace_paths": ["cidr_blocks"]
        }
      ]
    }
  ]
}
```

## Development

```bash
make test        # run unit and end-to-end tests
make build       # build ./go-tfplan-manager
make release     # build all platforms into dist/ with checksums.txt
```

End-to-end tests run real plans through the compiled binary using only the
built-in `terraform_data` resource, so no providers or network access are
needed. They use `terraform` from `PATH`; set `TF_BINARY=tofu` to test with
OpenTofu. They are skipped when the binary is missing, unless
`E2E_REQUIRED=1` is set, as it is in CI. CI runs them against both Terraform
and OpenTofu.

Releases are published by pushing a `vMAJOR.MINOR.PATCH` tag on a commit on
`main`. The release workflow runs the full CI checks before building.
