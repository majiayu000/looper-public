# Schedule a local shell job

Looper runs local shell commands on a cron schedule and exposes their status
and logs through its web observer. Follow the [source build](../README.md#quick-start)
first. This example runs `echo` only and does not invoke an agent CLI.

## Create a workflow

Save `hello.yaml` alongside your `looper` binary.

```yaml
engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: ./skills
scheduling:
  jobs:
    - name: hello
      schedule: "@every 1m"
      command: "echo 'Hello from Looper'"
      workdir: .
      type: script
      timeout: 10s
```

The current config schema requires an `engines` entry, even for a script-only
workflow. This job does not use that entry, so Claude Code does not need to be
installed to run the example. Relative `workdir` paths resolve from the workflow
file's directory, independently of where you launch the binary.

```bash
./looper -config hello.yaml -port 5567
```

The observer listens on `:5567` and has no authentication. Run it on a trusted
host and network; see [SECURITY.md](../SECURITY.md) before configuring real jobs.
The dashboard is at `http://127.0.0.1:5567/`.

## Trigger once and read the result

Keep Looper running. In a second terminal, trigger the named job and inspect it.

```bash
curl -sS -X POST 'http://127.0.0.1:5567/run?job=hello'
curl -sS http://127.0.0.1:5567/api/jobs
curl -sS 'http://127.0.0.1:5567/logs?job=hello'
```

`triggered` means the job was accepted for asynchronous execution. Wait for
`running` to become false and check `last_run_at`, `last_exit_code`, and
`last_error`. The log should contain `Hello from Looper`; it may not exist until
the first run. Logs are saved under `logs/` next to your workflow file.

The interval schedule waits until its first scheduled time; startup alone does
not run the job immediately. Stop the process with `Ctrl+C` when finished.

## Why did a scheduled job get skipped?

An already-running job is skipped. Jobs sharing the same `workdir` also share
a scheduler lock by default, so a second job may log `lock busy` instead of
waiting. An explicit `lock_key` changes that grouping. Choose a common key for
jobs that share runtime files or databases; keep the default for this example.

Check the scheduler's terminal output along with `/api/jobs`. A successful
`/health` response reports process health, not the success of an individual job.
Set `timeout` to a positive Go duration such as `10s` or `5m`.

Edit the workflow to replace `echo` with your own trusted command and adjust the
schedule. Looper watches config changes. For agent jobs, the checked-in
[WORKFLOW.md](../WORKFLOW.md) shows the Claude and Codex engine fields; these jobs
require the configured CLI and skill files.

Return to [configuration](../README.md#configuration) or include the workflow
path and sanitized logs in a [bug report](https://github.com/majiayu000/looper-public/issues).
