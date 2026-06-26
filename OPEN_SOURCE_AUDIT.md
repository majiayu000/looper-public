# Open Source Audit

This repository was prepared as a clean public release without importing the
private repository history.

## Included

- Go application source in `main.go` and `internal/`.
- Unit tests and synthetic test data in `testdata/`.
- Sanitized no-op example configuration in `WORKFLOW.md`.
- Placeholder local skill in `skills/example-skill/`.
- README, license, security policy, and ignore rules.

## Excluded

- Generated drafts and images.
- Runtime data and local databases.
- Evaluation run outputs.
- Local agent sessions, plans, and logs.
- Personal social account targets, blocked-account lists, ranking weights, and
  production posting schedules.
- Historical generated artifacts, backups, and private workflow notes.

## Verification

Run before publishing:

```bash
go test ./...
go build ./...
go run github.com/zricethezav/gitleaks/v8@latest detect --source . --no-git --redact --verbose
```

TruffleHog was not included as a required local check because its Go module
cannot be executed with `go run` from this repository environment.

