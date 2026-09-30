---
# Looper example workflow.
#
# This file is intentionally sanitized. It contains no real social accounts,
# credentials, private database paths, production schedules, or posting logic.

platforms:
  demo:
    enabled: false
    cli: echo
    db: testdata/test.db
    metrics:
      candidate_backlog_table: test_queue
      candidate_backlog_status_field: status
      candidate_backlog_pending_value: pending
      action_table: test_actions
      action_type_field: action_type
      action_time_field: acted_at
      published_items_table: published_item_tracking

engines:
  claude:
    kind: claude
    cli: claude
    skills_dir: ./skills

  codex:
    kind: codex
    cli: codex
    skills_dir: ./skills
    default_sandbox: read-only
    default_reasoning_effort: low

scheduling:
  jobs:
    - name: demo_noop
      schedule: "@every 24h"
      command: "echo 'Configure WORKFLOW.md before adding real jobs.'"
      workdir: .
      type: script
      timeout: 10s

---

# Looper Workflow

This Markdown file carries YAML front matter read by Looper. Keep private
credentials, cookies, real account strategy, and local production database paths
out of version control.


The `learning` fields are reserved and unimplemented. Looper does not adjust job
weights or enforce learning guardrails. Setting `learning.enabled: true` logs a
warning when the configuration loads, including on hot reload.
