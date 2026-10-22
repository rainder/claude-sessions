# Archive

## Week of 2026-10-12
FLEET page v1–v8 shipped: login, fleet cookie, send-keys guard, PWA, tailnet HTTPS :8443, session view, read-only chat, Start FLEET, live badges, inbox-zero glyph, spoof guard. Idle and finished events added (idle off by default; idle target follows FLEET name); daemon working list, auto-retire, 48h done-card prune, confirm delay 1.2→0.4s; header compacted 143→57px. ClickUp: `cu backlog --json` and 60s watcher sends ticket new/urgent to FLEET; FLEET on Grok (findFleet, /fleet say/chat/start) merged c404428, deployed. Sec reviews clean; open: grok worker may self-edit summary.json to impersonate FLEET; feat/fleet-derived unmerged; gitignore `.remember/tmp/` suggested.

## Week of 2026-10-05
Usage cmd and GET /usage/numbers added for Claude, Grok, Codex quotas. Hermetic test and timeout fallback fixed. On main via PR #1.

## Week of 2026-09-15
Session.Waiting() prioritizes waiting:* tabs; deployed. False positive: waiting:permission during MCP server. Sec audit passed.

## Week of 2026-08-18
Sec review cleared grok integration. Grok sessions module merged; review gaps and perm prompt states fixed. Pending deploy.

## Week of 2026-08-10
Grok-session display designed (summary.json join). Impl started in worktree.

## Week of 2026-08-03
Agent-workstation infra on port 8765; sec review passed. TUI self-ref filter and Tailscale config shipped; merged, deployed.

## Week of 2026-07-27
Scout found SSH attach bottleneck. Shipped ControlMaster multiplexing, persisted disabled-session state (flock), worktree locking. SSH sec review passed; disabled-session review ongoing.

## Week of 2026-07-20
Fixed kill-session routing bug. Shipped session mgmt suite (disable toggle, flicker-free rendering, picker polish); 9 commits, tests pass.