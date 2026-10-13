# Archive

## Week of 2026-09-15
Impl Session.Waiting() to prioritize waiting:* tabs in selection.go, session.go; deployed. False positive found: waiting:permission prompt during MCP server. Security audit passed (no vulns); WaitingFor predicate, data flow, and parity verified safe.

## Week of 2026-08-18
Security review cleared grok integration (no vulns). Impl grok sessions module (grok.go + 11 mods) with tmux display (attach/kill/preview/send-keys); merged. Code review found 5 gaps; grok reviewer AGREE-WITH-CAVEATS on 3 blockers (group/disable, bare-PID kill, dedupe test). Fixed permission prompt states; pending deploy.

## Week of 2026-08-10
Designed grok-session display with active_sessions.json→summary.json join and Tool field. Started impl in worktree.

## Week of 2026-08-03
Agent-workstation server infra impl on local port 8765 with security review (localhost filtering, safe slices verified, no vulns). Shipped TUI self-ref filter and Tailscale config. Merged and deployed.

## Week of 2026-07-27
Scout found SSH attach bottleneck (handshake overhead, no pooling). Shipped: ControlMaster multiplexing (60s reuse), persisted session-disabled state (DisabledStore + flock, POST endpoint), worktree locking (auto-detect/unlock/retry). SSH security review passed; disabled-session review ongoing.

## Week of 2026-07-20
Kill-session bug fixed (metadata routing via tmux/PID). Session mgmt suite shipped: disable toggle (loopback→Tailscale fallback), flicker-free rendering via row-diff sync, picker polish (type-to-filter, selection tracking, status sort, filters, load display, home collapse). 9 commits, tests pass.