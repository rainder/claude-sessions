# Archive

## Week of 2026-10-12
FLEET page v1–v8 shipped: login, PWA, HTTPS, session view, live badges, spoof guard. Idle and finished events added; daemon auto-retire and 48h done-card prune. ClickUp watcher sends tickets to FLEET; FLEET on Grok merged and deployed. Open: grok worker may forge summary.json as FLEET; feat/fleet-derived unmerged; gitignore `.remember/tmp/` suggested.

## Week of 2026-10-05
Usage cmd and GET /usage/numbers added for Claude, Grok, Codex quotas. Hermetic test and timeout fallback fixed. Merged to main via PR #1.

## Week of 2026-09-15
Session.Waiting() prioritizes waiting:* tabs; deployed. Waiting:permission false positive during MCP server. Sec audit passed.

## Week of 2026-08-18
Grok integration cleared by sec review. Grok sessions module merged; review gaps and perm prompt states fixed. Deploy pending.

## Week of 2026-08-10
Grok-session display designed (summary.json join). Impl started in worktree.

## Week of 2026-08-03
Agent-workstation infra on port 8765; sec review passed. TUI self-ref filter and Tailscale config shipped.

## Week of 2026-07-27
Scout found SSH attach bottleneck. Shipped ControlMaster multiplexing, flock-persisted disabled-session state, worktree locking. SSH sec review passed; disabled-session review ongoing.

## Week of 2026-07-20
Fixed kill-session routing bug. Shipped session mgmt suite (disable toggle, flicker-free render, picker polish); 9 commits, tests pass.
```

Notes on this run:
- Today is 2026-10-24. The `2026-10-22` entry is two days old, so it stays in `recent.md`. Nothing met the older-than-3-days rule, so the archive is unchanged.
- The `2026-10-23` entry goes before the Identity Candidates section, so that section stays at the end.
- Identity candidates: none found. The existing `none` line is kept.