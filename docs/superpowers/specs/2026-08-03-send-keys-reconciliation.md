# send-keys / input reconciliation (TUI compose + iOS phone answer)

Date: 2026-08-03. Reconciles `2026-08-03-inspector-send-keys-design.md` (TUI,
already `/ask-codex`-reviewed) against `claude-sessions-ios/docs/tasks/TASK2-send-input.md`
(phone). Reviewed via `/ask-codex` (DISAGREE on the first draft — this
revision incorporates every point raised).

## One endpoint, not two

`POST /sessions/{pid}/send-keys`. A second "hardened" route would not create
a real security boundary — any caller could hit the minimal one — so a
single strict, fail-closed handler used by both TUI and phone is correct.

```
{ "session_id": "<uuid>",   // required always, no legacy fail-open
  "text": "<line>",         // exactly one of text/key
  "key": "Enter",           // allowlist: Enter, Escape, Tab, Up, Down, Left, Right, C-c, 0-9
  "force": false            // wire default false, ALWAYS explicit — never server-inferred
}
```

### text vs key semantics (fixes the `y\n` contradiction)

- `text` never includes a trailing newline. iOS sends `"y"`, not `"y\n"` —
  TASK2's doc example is wrong and must be corrected in the next revision.
- `text` present → deliver literal text, then a separate `Enter` — the
  "submit a line" flow. Reject if it contains any C0 (0x00–0x1F) or C1
  (0x80–0x9F) control byte or DEL (0x7F), not just CR/LF/NUL — a phone
  client must not be able to smuggle ESC-equivalent behavior past the `key`
  allowlist by stuffing it into `text`.
- `key` present → deliver exactly that named key, no auto-appended Enter.
- Reject if both or neither field is set.
- 4KB cap applies to the **decoded** `text` value, not the raw HTTP body
  (JSON escaping inflates wire size — size the outer body limit separately,
  generously, e.g. 16KB).

### force semantics (fixes the foot-gun)

- Wire default is `false`. The server never infers "this is the TUI, so
  force is implied" from caller identity — that inference is exactly the
  kind of bug that silently flips to permissive behavior later.
- The TUI inspector's compose path explicitly sets `force: true` on every
  request it builds (the human is watching the live pane in real time).
- The phone's quick-reply chips (answering a currently-waiting prompt) leave
  it `false` — the session is expected to be waiting, so the check should
  pass on its own. The phone's free-text "send anyway" path is the only
  phone caller that sets `force: true`, behind an explicit confirmation.
- When the session is not waiting and `force` is not `true`: reject with
  `codeNotWaiting` (new).
- When the session has no tmux pane: reject with `codeNoPane` (new).

### Wire error codes — one vocabulary, not two

Use the Go server's actual wire codes throughout: `codeSessionMismatch`,
`codeNotLive` (existing, shared with kill/migrate), plus new `codeNotWaiting`
and `codeNoPane`. TASK2-send-input.md's `gone`/`moved_pid` names are iOS-side
internal/display naming over these same wire codes, not a competing wire
vocabulary — the next revision of that doc should say so explicitly instead
of implying new codes.

## Shared validated-delivery function, not duplicated validation

Both dispatch paths — the TUI's local direct call and the HTTP handler for
remote/phone — must go through **one** Go function that does: mutual-
exclusion check, control-char rejection, length cap, `not_waiting`/`no_pane`
checks, then delivery. The local TUI path bypassing the HTTP decoder must not
mean it bypasses these checks too (it would today, per the 8-task plan as
written) — factor validation out of the HTTP decoder into something both
paths call.

## Attestation: what's actually closed vs residual

Fresh collect-then-attest (recompute `Tmux` from a live re-collect,
`resolveLivePID` mismatch check) closes: wrong/stale PID, wrong/stale
session_id, pane recycled to a visibly different session. It does **not**
close, and this must be documented in the handler's doc comment rather than
implied as solved:

- TOCTOU between attest and the actual `tmux send-keys` call — collection
  captures the pane map near the *start* of `CollectLocal`, with further work
  happening after, so there is a real (if usually small) gap. Smaller for
  the TUI (human acts within moments) than the phone (can act minutes after
  a stale push).
- Same session_id+PID, waiting again but on a **different** prompt — only a
  wait-generation/`event_id` would close this, and both source docs
  explicitly defer that to v2. State this as a known, accepted residual
  risk, not a closed one.

## Two-call delivery: reduce the partial-failure race

`text` delivery is two tmux invocations (`send-keys -l -- <text>`, then
`send-keys Enter`). If the first succeeds and the second fails, the pane is
left with unsubmitted text while the client sees a failure and (per the
phone design) restores `composeText` for retry — a resend would duplicate
the text portion. Chain both into a **single** `tmux` process invocation
using tmux's own command separator (`tmux send-keys -t <pane> -l -- <text> \;
send-keys -t <pane> Enter`) so it is one exec call instead of two — this
narrows the race without a larger redesign. Still document the residual gap
(tmux itself could still apply the first and not the second internally) in
the same doc comment as the TOCTOU note above.

## Fallout on the existing 8-task TUI plan

Tasks 4/5/7/8 (compose UI, key routing, rendering, tests for those) are
largely unaffected. Tasks 1–3 and 6 need rework:
- The `sendKeys(s Session, text string) error` primitive signature must
  become `sendKeys(s Session, req sendKeysRequest) error` (or equivalent) to
  carry `key`/`force`, and the primitive must live in the shared
  validated-delivery function above, not duplicate it.
- `sendKeysBody`'s decoder needs `key`/`force` fields, mutual-exclusion
  validation, and a decoded-bytes cap distinct from the outer body-size
  budget (`sendKeysMaxLen(4096) + 256` as originally sized can reject a
  valid 4KB payload once JSON escaping inflates it).
- The inspector's own dispatch (local + remote) must always set `force:
  true` explicitly in the request it builds — never rely on a default.
- Test fallout: `TestSendKeysHandlerSuccessCallsSendKeysFn` needs
  `force:true` or a waiting fixture; `TestSendKeysHandlerRequiresText`
  becomes a full mutual-exclusion matrix (neither/both/text-only/key-only);
  add `not_waiting`, `no_pane`, key-allowlist, and forced-success cases.

## Design review

Reviewed via `/ask-codex`: first draft (single endpoint, force implicit for
TUI, fresh-attest claimed to fully close the pane-reuse hazard) returned
DISAGREE — foot-gun on inferred force, TOCTOU overstated as closed, `y\n`
contradiction with Design A's control-char rejection, wire-code drift
between docs, partial-failure race unaddressed, local TUI path bypassing
validation. All incorporated above.
