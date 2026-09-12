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

# learning is reserved/unimplemented. Fields are kept for YAML compatibility.
# Setting enabled: true only logs a warning; no ranking or weight updates run.
learning:
  enabled: false
  guardrails:
    max_weight: 3.0
    min_weight: 0.3
    max_daily_change: 0.2
    min_samples: 5
---

# Looper Workflow

This Markdown file carries YAML front matter read by Looper. Keep private
credentials, cookies, real account strategy, and local production database paths
out of version control.

## Learning (reserved)

The `learning:` block is parsed for forward compatibility but is not implemented.
Looper does not adjust job ranking or weights from these fields. Leave
`learning.enabled` false unless you are intentionally exercising the startup
warning path.

