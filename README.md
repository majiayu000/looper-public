# Looper

This repository, [majiayu000/looper-public](https://github.com/majiayu000/looper-public), contains Looper, a small Go scheduler and observer for running local automation jobs.
Jobs can be plain shell commands or agent skill runs, with a web observer for
status, logs, and lightweight SQLite-backed metrics.

This public repository is a sanitized source release. It intentionally excludes
private drafts, social account strategy, run logs, local databases, model
sessions, and historical generated artifacts.

## Features

- YAML/Markdown front matter workflow config.
- Cron-style job scheduling with per-job timeouts.
- Script jobs and agent skill jobs.
- Local web observer with status and log endpoints.
- Hot reload when the workflow config changes.
- SQLite-backed platform metrics.

## Quick Start

Requires Go 1.25 or newer. Shell jobs use your local shell; agent skill jobs
also require the configured Claude Code or Codex CLI.

```bash
git clone https://github.com/majiayu000/looper-public.git
cd looper-public
go test ./...
go build -o looper .
./looper -config WORKFLOW.md -port 5567
```

Open `http://127.0.0.1:5567/health` to confirm the process is running.

The observer listens only on `127.0.0.1`; `-port` changes the port, not the
bind address. Its dashboard and APIs, including `POST /run`, are for trusted
local use and do not require authentication. Do not expose the observer through
a public reverse proxy or port forward.
Manual runs reject non-local `Host` values and cross-origin browser requests;
the local dashboard and command-line clients can still trigger configured jobs.

The included [WORKFLOW.md](WORKFLOW.md) is a no-op example. Replace it with
your own local workflow before running real jobs.

See [Configuration](#configuration) for workflow fields and
[SECURITY.md](SECURITY.md) for handling trusted local commands.

For a runnable shell workflow, manual triggering, logs, and skipped-job
troubleshooting, see [Schedule a local shell job](docs/local-jobs.md).

## Configuration

`WORKFLOW.md` contains the runnable example config. The same schema can be kept
in a plain YAML file if you prefer.

Important fields:

- `platforms`: optional metric sources backed by local SQLite databases.
- `engines`: configured agent engines, such as `claude` or `codex`.
- `scheduling.jobs`: cron entries for `script` or `skill` jobs.

Do not commit real credentials, cookies, private databases, generated drafts, or
production account strategy. Keep those in local ignored files.

## Repository Hygiene

The public release excludes these private categories:

- `drafts/`
- `data/`
- `eval/runs/`
- `.claude/`
- `.omx/`
- `logs/`
- personal artifact and backup histories

Run these checks before publishing changes:

```bash
go test ./...
go build ./...
```

Use a secret scanner such as Gitleaks or TruffleHog before pushing to a public
remote.

## License and Support

Looper is distributed under the [MIT license](LICENSE). The installation path
documented here is a source build; include your Git commit when reporting an
issue because this CLI does not have a version flag.

Use [GitHub issues](https://github.com/majiayu000/looper-public/issues) for bugs,
with your OS, config path, job name, schedule, and sanitized logs. Follow
[SECURITY.md](SECURITY.md) for private vulnerability reports.
