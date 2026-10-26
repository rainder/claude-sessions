package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// FLEET Clarify routes and the worker that runs a fork for them. The store
// (fleet_clarify.go) keeps the chats; the runner (fleet_clarify_run.go)
// builds the argv and runs claude. Every refusal is JSON {code}; a fork's
// error text and any path go to the server's stderr only, never into a
// response or a stored turn.

const (
	clarifyQuestionMax       = 4000     // runes
	clarifyAnswerMax         = 32 << 10 // bytes of a stored answer or draft
	clarifyBodyMax           = 32 << 10
	clarifyCardIDMax         = 200
	clarifyRetryDelayDefault = 5 * time.Second

	clarifyErrIsError = "the fork answered with an error"
	clarifyErrTimeout = "no answer within 5 minutes"
	clarifyErrMidStep = "FLEET is mid-step; ask again in a moment"
	clarifyErrFailed  = "the fork failed; see the server log"
)

// clarifyDraftPrompt is the fixed question a draft asks the fork.
func clarifyDraftPrompt(cardID string) string {
	return "Write the decision I reached in this chat about card " + cardID +
		" as one short paragraph addressed to FLEET. No greeting, no preamble: output only the message."
}

// clarifyChatResponse is GET /fleet/clarify. Turns is never null.
type clarifyChatResponse struct {
	Card    string          `json:"card"`
	Tool    string          `json:"tool"`
	Turns   []clarifyTurn   `json:"turns"`
	Pending *clarifyPending `json:"pending"`
	Draft   *clarifyDraft   `json:"draft"`
}

func clarifyRefuse(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"code": code})
}

// clarifyGuard is the shared auth check: fleet auth, and same origin for a
// cookie POST. It writes the refusal and returns false.
func (s *server) clarifyGuard(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		clarifyRefuse(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if r.Method == http.MethodPost && kind == fleetAuthCookie && !sameOrigin(r) {
		clarifyRefuse(w, http.StatusForbidden, "origin")
		return false
	}
	return true
}

func clarifyCardIDOK(id string) bool {
	return id != "" && len(id) <= clarifyCardIDMax
}

// fleetClarifyGet is GET /fleet/clarify?card=<id>: the card's chat, or an
// empty one for a card with none.
func (s *server) fleetClarifyGet(w http.ResponseWriter, r *http.Request) {
	if !s.clarifyGuard(w, r) {
		return
	}
	id := r.URL.Query().Get("card")
	if !clarifyCardIDOK(id) {
		clarifyRefuse(w, http.StatusBadRequest, "bad_request")
		return
	}
	e, _ := s.fleet.clarify.get(id)
	out := clarifyChatResponse{Card: id, Tool: e.Tool, Turns: e.Turns, Pending: e.Pending, Draft: e.Draft}
	if out.Turns == nil {
		out.Turns = []clarifyTurn{}
	}
	writeJSON(w, http.StatusOK, out)
}

// fleetClarifyAsk is POST /fleet/clarify/ask {cardId, text}.
func (s *server) fleetClarifyAsk(w http.ResponseWriter, r *http.Request) {
	if !s.clarifyGuard(w, r) {
		return
	}
	var body *struct {
		CardID string `json:"cardId"`
		Text   string `json:"text"`
	}
	if err := decodeStrictJSONLimit(w, r, &body, clarifyBodyMax); err != nil || body == nil || !clarifyCardIDOK(body.CardID) {
		clarifyRefuse(w, http.StatusBadRequest, "bad_request")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		clarifyRefuse(w, http.StatusBadRequest, "empty")
		return
	}
	if utf8.RuneCountInString(text) > clarifyQuestionMax {
		clarifyRefuse(w, http.StatusBadRequest, "too_long")
		return
	}
	e, code, status := s.clarifyStart(body.CardID, clarifyKindAsk, text)
	if code != "" {
		clarifyRefuse(w, status, code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": e.Pending})
}

// fleetClarifyDraft is POST /fleet/clarify/draft: {cardId} asks the fork for
// a draft answer; {cardId, discard: true} drops the draft there is.
func (s *server) fleetClarifyDraft(w http.ResponseWriter, r *http.Request) {
	if !s.clarifyGuard(w, r) {
		return
	}
	var body *struct {
		CardID  string `json:"cardId"`
		Discard bool   `json:"discard"`
	}
	if err := decodeStrictJSONLimit(w, r, &body, clarifyBodyMax); err != nil || body == nil || !clarifyCardIDOK(body.CardID) {
		clarifyRefuse(w, http.StatusBadRequest, "bad_request")
		return
	}
	if body.Discard {
		if !s.fleet.clarify.discardDraft(body.CardID) {
			clarifyRefuse(w, http.StatusNotFound, "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	e, code, status := s.clarifyStart(body.CardID, clarifyKindDraft, "")
	if code != "" {
		clarifyRefuse(w, status, code)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": e.Pending})
}

// clarifyStart checks the card, begins a fork in the store and hands it to
// the worker. It returns the entry as begun, or a refusal code and its HTTP
// status. Discovery runs only for a card with no chat yet: a later question
// resumes the chat's own fork and needs no live FLEET.
func (s *server) clarifyStart(cardID, kind, question string) (clarifyEntry, string, int) {
	state, stateErr := s.loadFleetState()
	if stateErr != "" {
		return clarifyEntry{}, "card_gone", http.StatusConflict
	}
	card, ok := fleetDecisionCards(state)[cardID]
	if !ok {
		return clarifyEntry{}, "card_gone", http.StatusConflict
	}
	var init *clarifyEntry
	if _, has := s.fleet.clarify.get(cardID); !has && kind == clarifyKindAsk {
		sessions, err := s.collectLocal()
		if err != nil {
			fmt.Fprintf(os.Stderr, "clarify: card %s: session list failed: %v\n", cardID, err)
			return clarifyEntry{}, "not_live", http.StatusConflict
		}
		sess, reason := findFleet(sessions)
		switch {
		case reason == "ambiguous":
			return clarifyEntry{}, "ambiguous", http.StatusConflict
		case reason != "":
			return clarifyEntry{}, "not_live", http.StatusConflict
		case sess.Tool == toolGrok:
			return clarifyEntry{}, "unsupported_tool", http.StatusConflict
		}
		init = &clarifyEntry{Tool: "", FleetSessionID: sess.SessionID, Model: sess.Model, CardHash: card.Hash, Cwd: s.clarifyCwd(sess)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), clarifyTimeout)
	e, token, code := s.fleet.clarify.begin(cardID, kind, init, question, cancel, s.fleetNow())
	if code != "" {
		cancel()
		status := http.StatusConflict
		if code == "empty" {
			status = http.StatusBadRequest
		}
		return clarifyEntry{}, code, status
	}
	if kind == clarifyKindDraft {
		question = clarifyDraftPrompt(cardID)
	}
	preamble := clarifyPreamble(card.Card)
	run := func() { s.clarifyWork(ctx, cancel, cardID, kind, token, e, preamble, question) }
	if s.fleet.clarifyGo != nil {
		s.fleet.clarifyGo(run)
	} else {
		go run()
	}
	return e, "", 0
}

// clarifyCwd is FLEET's transcript folder: the cwd in the transcript head,
// as a resume start reads it, else the session's own cwd. A claude transcript
// is keyed to the folder it started in, so --resume finds it only there.
func (s *server) clarifyCwd(sess Session) string {
	if home, err := s.fleetHome(); err == nil {
		if p := findTranscript(home, sess.SessionID); p != "" {
			if head, _ := readResumableHeadFn(p); head.cwd != "" {
				return head.cwd
			}
		}
	}
	return sess.CWD
}

func (s *server) clarifyRunFork(ctx context.Context, cwd string, argv []string) (clarifyRunResult, error) {
	if s.fleet.clarifyRun != nil {
		return s.fleet.clarifyRun(ctx, cwd, argv)
	}
	bin := clarifyClaudeBin()
	if bin == "" {
		fmt.Fprintln(os.Stderr, "clarify: no claude binary on PATH")
		return clarifyRunResult{}, errClarifyExit
	}
	return runClarifyFork(ctx, bin, cwd, argv)
}

// clarifyWork runs one fork and records its outcome. A first question (no
// fork id yet) forks FLEET's session; a later one resumes the fork. A first
// question that fails to run is retried once after a pause: FLEET is often
// mid-step, and the fork of an unfinished turn can fail where a moment later
// it would not. finish is a no-op when the chat was pruned meanwhile.
func (s *server) clarifyWork(ctx context.Context, cancel context.CancelFunc, cardID, kind string, token uint64, e clarifyEntry, preamble, question string) {
	defer cancel()
	p := clarifyRunParams{Model: e.Model, CuPath: clarifyCuPath(), Preamble: preamble, Question: question}
	first := e.ForkID == ""
	if first {
		p.ResumeID, p.Fork = e.FleetSessionID, true
	} else {
		p.ResumeID = e.ForkID
	}
	var res clarifyRunResult
	var err error
	retried := false
	if p.ResumeID == "" {
		// The chat was pruned and begun again from nothing between the
		// lookup and begin: there is no session to resume.
		err = errors.New("no session to resume")
	} else {
		argv := clarifyArgv(p)
		res, err = s.clarifyRunFork(ctx, e.Cwd, argv)
		if err != nil && first && (errors.Is(err, errClarifyExit) || errors.Is(err, errClarifyBadOutput)) {
			fmt.Fprintf(os.Stderr, "clarify: card %s: %v; retrying once\n", cardID, err)
			retried = true
			delay := s.fleet.clarifyRetryDelay
			if delay <= 0 {
				delay = clarifyRetryDelayDefault
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				err = ctx.Err()
			case <-t.C:
				res, err = s.clarifyRunFork(ctx, e.Cwd, argv)
			}
		}
	}

	at := s.fleetNow().UTC().Format(time.RFC3339)
	turn := func(role, text string) *clarifyTurn { return &clarifyTurn{Role: role, Text: text, At: at} }
	var t *clarifyTurn
	var d *clarifyDraft
	switch {
	case err == nil && res.IsError:
		fmt.Fprintf(os.Stderr, "clarify: card %s: the fork answered is_error\n", cardID)
		t = turn(clarifyRoleError, clarifyErrIsError)
	case err == nil && kind == clarifyKindDraft:
		text := truncBytesHead(strings.TrimSpace(res.Text), clarifyAnswerMax)
		if text == "" {
			fmt.Fprintf(os.Stderr, "clarify: card %s: the fork wrote an empty draft\n", cardID)
			t = turn(clarifyRoleError, clarifyErrIsError)
		} else {
			d = &clarifyDraft{Text: text, At: at}
		}
	case err == nil:
		t = turn(clarifyRoleFleet, truncBytesHead(res.Text, clarifyAnswerMax))
	case errors.Is(err, errClarifyTimeout) || errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(os.Stderr, "clarify: card %s: %v\n", cardID, err)
		t = turn(clarifyRoleError, clarifyErrTimeout)
	case retried && (errors.Is(err, errClarifyExit) || errors.Is(err, errClarifyBadOutput)):
		fmt.Fprintf(os.Stderr, "clarify: card %s: %v\n", cardID, err)
		t = turn(clarifyRoleError, clarifyErrMidStep)
	default:
		// Cancelled lands here too: a prune cancelled it and finish is a
		// no-op. Anything else gets the plain failure turn.
		fmt.Fprintf(os.Stderr, "clarify: card %s: %v\n", cardID, err)
		t = turn(clarifyRoleError, clarifyErrFailed)
	}
	s.fleet.clarify.finish(cardID, token, res.SessionID, t, d, s.fleetNow())
}
