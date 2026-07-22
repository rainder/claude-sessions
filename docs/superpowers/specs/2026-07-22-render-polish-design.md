# Render Polish Design

## Goal

Improve session-table readability across terminal widths without changing selection behavior or data.

## Selected-row highlight

Replace reverse-video selection with a subtle ANSI 256-color dark-gray background. Keep existing foreground colors and text styling visible inside selected rows. Apply same highlight to session rows and selectable empty-host rows.

Selected and unselected rows must keep identical visual width. Highlight wrapper must end with one reset and must not leave terminal styles active after row end.

## Right padding

Every table mode—full, intermediate, and minimal—gets exactly one visible space after its final column. Header and separator widths follow padded row width so layout remains aligned. This padding belongs inside selected-row background, producing a clean one-cell margin at right edge.

## Intermediate column order

Change intermediate view from:

`NAME DIR MODEL STATUS COST AGENTS CTX CPU AGE`

to:

`NAME DIR STATUS MODEL COST AGENTS CTX CPU AGE`

Full and minimal column order remains unchanged.

## Implementation scope

Changes stay in table-rendering code and rendering tests. No session data, sorting, input handling, viewport, or remote behavior changes.

## Verification

Tests cover:

- background-only selected-row escape sequence;
- preservation of foreground styling in selected rows;
- equal selected/unselected visual widths;
- exactly one trailing visible space in all three table modes;
- intermediate header and row ordering;
- existing render suite, `go test ./...`, `go vet ./...`, and build.

## Delivery

Work occurs on dedicated worktree branch. After verification and review, commit implementation, merge branch into `main`, push `main`, and run `make install`.
