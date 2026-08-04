package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeAttach stands in for `tmux attach-session` on a PTY: it records every
// byte and every resize the pump hands it, and emits whatever output a test
// pushes into it. Everything the attach pump does — forwarding input, dropping
// it on a readonly connection, resizing, idling out, detaching — is observable
// through it without a tmux server anywhere.
type fakeAttach struct {
	mu     sync.Mutex
	writes [][]byte
	sizes  []attachSize
	closed bool

	out  chan []byte
	done chan struct{}
	once sync.Once
}

func newFakeAttach() *fakeAttach {
	return &fakeAttach{out: make(chan []byte, 64), done: make(chan struct{})}
}

func (f *fakeAttach) Read(p []byte) (int, error) {
	select {
	case b := <-f.out:
		return copy(p, b), nil
	case <-f.done:
		return 0, io.EOF
	}
}

func (f *fakeAttach) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakeAttach) Resize(size attachSize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, size)
	return nil
}

func (f *fakeAttach) Close() error {
	f.once.Do(func() {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		close(f.done)
	})
	return nil
}

// emit queues one chunk of terminal output for the pump to forward.
func (f *fakeAttach) emit(b []byte) { f.out <- b }

func (f *fakeAttach) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeAttach) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, w := range f.writes {
		sb.Write(w)
	}
	return sb.String()
}

func (f *fakeAttach) resizes() []attachSize {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sizes)
}

func (f *fakeAttach) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// attachFixture is a server with one live session at PID 4242 and a fake attach
// client behind the startAttach seam.
type attachFixture struct {
	srv      *httptest.Server
	server   *server
	clients  chan *fakeAttach // every attach client the handler started
	requests chan attachStart // and what it asked for
}

type attachStart struct {
	Target   string
	ReadOnly bool
	Size     attachSize
}

func newAttachFixture(t *testing.T) *attachFixture {
	t.Helper()
	f := &attachFixture{
		clients:  make(chan *fakeAttach, 8),
		requests: make(chan attachStart, 8),
	}
	f.server = &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 4242, SessionID: "sess-live", Tmux: "claude-dev:0.0"}}, nil
		},
		startAttach: func(target string, readonly bool, size attachSize) (attachClient, error) {
			client := newFakeAttach()
			f.requests <- attachStart{Target: target, ReadOnly: readonly, Size: size}
			f.clients <- client
			return client, nil
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{pid}/attach", f.server.attach)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// dial opens an attach WebSocket. query is appended to the URL as-is.
func (f *attachFixture) dial(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(f.srv.URL, "http://", "ws://", 1) + "/sessions/4242/attach?" + query
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", query, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// get performs a plain (non-upgrade) GET, which is how every pre-upgrade
// refusal is observed — and how curl sees this endpoint.
func (f *attachFixture) get(t *testing.T, path string, auth bool) (*http.Response, actionResult) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer secret")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got actionResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp, got
}

// next pulls the client the handler just started, failing rather than hanging.
func (f *attachFixture) nextClient(t *testing.T) (*fakeAttach, attachStart) {
	t.Helper()
	select {
	case client := <-f.clients:
		return client, <-f.requests
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started an attach client")
		return nil, attachStart{}
	}
}

// readFrame reads one message with a deadline.
func readFrame(t *testing.T, conn *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return typ, data
}

func writeFrame(t *testing.T, conn *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, typ, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAttachRequiresAuth(t *testing.T) {
	f := newAttachFixture(t)
	resp, _ := f.get(t, "/sessions/4242/attach?session_id=sess-live", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if len(f.clients) != 0 {
		t.Fatal("an unauthenticated request started an attach client")
	}
}

func TestAttachRefusesBadPID(t *testing.T) {
	f := newAttachFixture(t)
	resp, _ := f.get(t, "/sessions/not-a-pid/attach", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestAttachRequiresSessionID: unlike kill/migrate, there is no legacy caller
// of this endpoint predating the identity guard, so there is no unguarded
// "act on whatever holds this PID" mode to preserve -- and that mode would be
// a full interactive shell here, not a single mutation.
func TestAttachRequiresSessionID(t *testing.T) {
	f := newAttachFixture(t)
	resp, _ := f.get(t, "/sessions/4242/attach", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(f.clients) != 0 {
		t.Fatal("a request with no session_id started an attach client")
	}
}

// TestAttachRefusesADifferentSession is the action-contract precondition: the
// phone names the session it believes is at that PID, and a recycled pane must
// not hand it somebody else's terminal.
func TestAttachRefusesADifferentSession(t *testing.T) {
	f := newAttachFixture(t)
	resp, got := f.get(t, "/sessions/4242/attach?session_id=sess-stale", true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got.OK || got.Code != codeSessionMismatch {
		t.Fatalf("result = %#v, want a %s refusal", got, codeSessionMismatch)
	}
	if len(f.clients) != 0 {
		t.Fatal("a refused attach still started a client — the PTY must not exist")
	}
}

func TestAttachRefusesDeadPID(t *testing.T) {
	f := newAttachFixture(t)
	resp, got := f.get(t, "/sessions/9999/attach?session_id=sess-live", true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got.Code != codeNotLive {
		t.Fatalf("result = %#v, want a %s refusal", got, codeNotLive)
	}
	if len(f.clients) != 0 {
		t.Fatal("a refused attach still started a client")
	}
}

// TestAttachRefusesSessionWithNoPane: a session running outside tmux has
// nothing to stream, and says so with the shared no_pane code rather than
// failing later inside the socket.
func TestAttachRefusesSessionWithNoPane(t *testing.T) {
	f := newAttachFixture(t)
	f.server.collect = func() ([]Session, error) {
		return []Session{{PID: 4242, SessionID: "sess-live", Tmux: ""}}, nil
	}
	resp, got := f.get(t, "/sessions/4242/attach?session_id=sess-live", true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got.OK || got.Code != codeNoPane {
		t.Fatalf("result = %#v, want a %s refusal", got, codeNoPane)
	}
	if len(f.clients) != 0 {
		t.Fatal("a paneless session still started an attach client")
	}
}

func TestAttachRefusesMalformedTmuxMetadata(t *testing.T) {
	f := newAttachFixture(t)
	f.server.collect = func() ([]Session, error) {
		return []Session{{PID: 4242, SessionID: "sess-live", Tmux: "no-colon-here"}}, nil
	}
	resp, got := f.get(t, "/sessions/4242/attach?session_id=sess-live", true)
	if resp.StatusCode != http.StatusConflict || got.Code != codeNoPane {
		t.Fatalf("status = %d, result = %#v, want a 409 %s", resp.StatusCode, got, codeNoPane)
	}
}

// TestAttachStreamsPTYOutput is the baseline the readonly proof depends on:
// bytes really do flow, so "no input arrived" cannot pass by the socket being
// dead.
func TestAttachStreamsPTYOutput(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, req := f.nextClient(t)
	if req.Target != "claude-dev:0.0" || req.ReadOnly {
		t.Fatalf("attach request = %#v, want the resolved pane, read-write", req)
	}
	if req.Size != (attachSize{Cols: attachDefaultCols, Rows: attachDefaultRows}) {
		t.Fatalf("initial size = %#v, want the 80x24 default", req.Size)
	}
	client.emit([]byte("hello from tmux"))
	typ, data := readFrame(t, conn)
	if typ != websocket.MessageBinary {
		t.Fatalf("output frame type = %v, want binary", typ)
	}
	if string(data) != "hello from tmux" {
		t.Fatalf("output = %q", data)
	}
}

func TestAttachForwardsInputToThePTY(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)
	writeFrame(t, conn, websocket.MessageBinary, []byte("ls\r"))
	waitUntil(t, "input to reach the PTY", func() bool { return client.written() == "ls\r" })
}

// TestAttachReadonlyDropsInputServerSide is the one that matters. The client
// asked for readonly, so tmux was started with -r — but that is a courtesy to
// the far side, not the enforcement point, and a compromised or buggy client
// could ask for readonly and type anyway.
//
// The proof is ordered so it cannot pass vacuously:
//
//  1. output flows first, so the socket is provably live;
//  2. the input frame goes out;
//  3. a resize frame goes out *after* it, and the test waits until the resize
//     is observed on the far side — the pump handles frames in order, so once
//     the resize has landed the input frame has demonstrably been processed;
//  4. only then is Write asserted to have never been called at all.
//
// TestAttachForwardsInputToThePTY above is the positive control: the identical
// path with only the readonly flag flipped does deliver the bytes.
func TestAttachReadonlyDropsInputServerSide(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live&readonly=1")
	client, req := f.nextClient(t)
	if !req.ReadOnly {
		t.Fatal("readonly=1 did not reach the attach client")
	}

	client.emit([]byte("prompt$ "))
	if _, data := readFrame(t, conn); string(data) != "prompt$ " {
		t.Fatalf("output = %q, want the stream to be live before we test input", data)
	}

	writeFrame(t, conn, websocket.MessageBinary, []byte("rm -rf /\r"))
	writeFrame(t, conn, websocket.MessageText, []byte(`{"resize":{"cols":100,"rows":30}}`))
	waitUntil(t, "the frame sent after the input to be processed", func() bool {
		return len(client.resizes()) == 1
	})

	if n := client.writeCount(); n != 0 {
		t.Fatalf("readonly connection wrote to the PTY %d time(s): %q", n, client.written())
	}
}

func TestAttachResizeControlFrameResizesThePTY(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)
	writeFrame(t, conn, websocket.MessageText, []byte(`{"resize":{"cols":132,"rows":43}}`))
	waitUntil(t, "the resize to reach the PTY", func() bool { return len(client.resizes()) == 1 })
	if got := client.resizes()[0]; got != (attachSize{Cols: 132, Rows: 43}) {
		t.Fatalf("resize = %#v, want 132x43", got)
	}
}

// TestAttachIgnoresUnusableControlFrames: a terminal somebody is working in
// must not die because one control frame was wrong, and a zero-column resize
// would make the tty unusable rather than small.
func TestAttachIgnoresUnusableControlFrames(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)
	for _, bad := range []string{
		`{"resize":{"cols":0,"rows":24}}`,
		`{"resize":{"cols":80,"rows":0}}`,
		`{"resize":{"cols":99999,"rows":24}}`,
		`{"resize":{"cols":-1,"rows":24}}`,
		`not json at all`,
		`{"lightsOut":true}`,
	} {
		writeFrame(t, conn, websocket.MessageText, []byte(bad))
	}
	// A good one behind them: once it lands, every frame above has been seen.
	writeFrame(t, conn, websocket.MessageText, []byte(`{"resize":{"cols":90,"rows":25}}`))
	waitUntil(t, "the trailing good resize", func() bool { return len(client.resizes()) > 0 })
	if got := client.resizes(); len(got) != 1 || got[0] != (attachSize{Cols: 90, Rows: 25}) {
		t.Fatalf("resizes = %#v, want only the one good frame", got)
	}
	client.emit([]byte("still here"))
	if _, data := readFrame(t, conn); string(data) != "still here" {
		t.Fatalf("output = %q, want the connection to have survived", data)
	}
}

func TestAttachInitialSizeFromQuery(t *testing.T) {
	f := newAttachFixture(t)
	f.dial(t, "session_id=sess-live&cols=120&rows=40")
	_, req := f.nextClient(t)
	if req.Size != (attachSize{Cols: 120, Rows: 40}) {
		t.Fatalf("initial size = %#v, want 120x40", req.Size)
	}
}

func TestAttachRefusesUnusableInitialSize(t *testing.T) {
	f := newAttachFixture(t)
	for _, q := range []string{"cols=0", "cols=abc", "rows=0", "rows=99999"} {
		resp, _ := f.get(t, "/sessions/4242/attach?session_id=sess-live&"+q, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

// TestAttachDisconnectClosesOnlyTheAttachClient: the client going away must
// tear down the PTY and the attach process, and nothing else. That it leaves
// the tmux session itself alive is proven against real tmux in
// attach_tmux_test.go — here we prove the handler releases what it owns.
func TestAttachDisconnectClosesTheAttachClient(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)
	client.emit([]byte("ready"))
	readFrame(t, conn)

	if err := conn.Close(websocket.StatusNormalClosure, "leaving"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the attach client to be closed", client.isClosed)
}

// TestAttachClientExitEndsTheConnection is the other direction: the user types
// the tmux detach key, the attach process exits, and the phone is told rather
// than left holding a socket that will never speak again.
func TestAttachClientExitEndsTheConnection(t *testing.T) {
	f := newAttachFixture(t)
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)
	client.Close() // the attach process exiting: the PTY reads EOF

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v (err %v), want a normal closure", websocket.CloseStatus(err), err)
	}
}

// TestAttachIdleTimeoutIgnoresPTYOutput pins the design decision inside the
// idle timer: tmux redraws its status line every status-interval seconds
// whether anybody is there or not, so a timer that output could reset would
// never fire on a real session. Output flows for the whole test and the
// connection still closes.
func TestAttachIdleTimeoutIgnoresPTYOutput(t *testing.T) {
	f := newAttachFixture(t)
	f.server.attachIdle = 200 * time.Millisecond
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				select {
				case client.out <- []byte("status bar tick"):
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = conn.Read(ctx)
	}
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (err %v), want going-away", websocket.CloseStatus(err), err)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != "idle timeout" {
		t.Fatalf("close reason = %q, want %q", ce.Reason, "idle timeout")
	}
	waitUntil(t, "the idled-out attach client to be closed", client.isClosed)
}

// TestAttachIdleTimeoutIsResetByClientActivity: the counterpart. A connection
// somebody is typing into stays up past several idle windows.
func TestAttachIdleTimeoutIsResetByClientActivity(t *testing.T) {
	f := newAttachFixture(t)
	f.server.attachIdle = 300 * time.Millisecond
	conn := f.dial(t, "session_id=sess-live")
	client, _ := f.nextClient(t)

	for i := 0; i < 6; i++ {
		writeFrame(t, conn, websocket.MessageBinary, []byte("x"))
		time.Sleep(100 * time.Millisecond)
	}
	if client.isClosed() {
		t.Fatal("a connection being typed into was idled out")
	}
	client.emit([]byte("alive"))
	if _, data := readFrame(t, conn); string(data) != "alive" {
		t.Fatalf("output = %q, want the connection to still be usable", data)
	}
}

// TestAttachCapsConcurrentAttachesPerSession: two terminals on one session is
// the limit, and the slot comes back when one of them leaves.
func TestAttachCapsConcurrentAttachesPerSession(t *testing.T) {
	f := newAttachFixture(t)
	first := f.dial(t, "session_id=sess-live")
	f.nextClient(t)
	f.dial(t, "session_id=sess-live")
	f.nextClient(t)

	resp, got := f.get(t, "/sessions/4242/attach?session_id=sess-live", true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third attach status = %d, want 429", resp.StatusCode)
	}
	if got.OK || got.Code != codeAttachBusy {
		t.Fatalf("result = %#v, want an %s refusal", got, codeAttachBusy)
	}

	if err := first.Close(websocket.StatusNormalClosure, "leaving"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the freed slot to be reusable", func() bool {
		resp, _ := f.get(t, "/sessions/4242/attach?session_id=sess-live", true)
		return resp.StatusCode != http.StatusTooManyRequests
	})
}

func TestAttachLimiterCountsPerKey(t *testing.T) {
	var l attachLimiter
	a, ok := l.acquire("one", 2, 99)
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := l.acquire("one", 2, 99); !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := l.acquire("one", 2, 99); ok {
		t.Fatal("third acquire allowed past the cap")
	}
	if _, ok := l.acquire("two", 2, 99); !ok {
		t.Fatal("a different session was blocked by the first one's attaches")
	}
	a()
	a() // idempotent: a double release must not hand out a slot that isn't free
	if _, ok := l.acquire("one", 2, 99); !ok {
		t.Fatal("releasing did not free a slot")
	}
	if _, ok := l.acquire("one", 2, 99); ok {
		t.Fatal("a double release freed a slot twice")
	}
}

// TestAttachLimiterCapsTheHostTotal: the per-session cap alone bounds nothing.
// One authenticated client can attach to as many different sessions as the host
// has, each holding a PTY, a tmux client and a pair of goroutines, so there is a
// second ceiling across all of them — and spreading the attaches over distinct
// keys must not walk past it.
func TestAttachLimiterCapsTheHostTotal(t *testing.T) {
	var l attachLimiter
	first, ok := l.acquire("a", 2, 3)
	if !ok {
		t.Fatal("first acquire refused")
	}
	for _, key := range []string{"b", "c"} {
		if _, ok := l.acquire(key, 2, 3); !ok {
			t.Fatalf("acquire %q refused below the host total", key)
		}
	}
	if _, ok := l.acquire("d", 2, 3); ok {
		t.Fatal("a fourth session attached past a host total of three")
	}
	first()
	first() // the host counter has to be as idempotent as the per-session one
	if _, ok := l.acquire("d", 2, 3); !ok {
		t.Fatal("releasing did not free a host slot")
	}
	if _, ok := l.acquire("e", 2, 3); ok {
		t.Fatal("a double release freed a host slot twice")
	}
}

// TestAttachLimiterHostTotalRefusesBeforeTheSessionCap: the two ceilings are
// independent. A session with a free slot of its own is still refused when the
// host has none left, and the refused attempt must not leave the counters
// thinking it succeeded.
func TestAttachLimiterHostTotalRefusesBeforeTheSessionCap(t *testing.T) {
	var l attachLimiter
	release, ok := l.acquire("one", 2, 1)
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := l.acquire("one", 2, 1); ok {
		t.Fatal("a session slot was handed out with the host total already full")
	}
	release()
	if _, ok := l.acquire("one", 2, 1); !ok {
		t.Fatal("the refused attempt was counted against the session anyway")
	}
}

// TestTmuxAttachArgsForceExactSessionMatching pins the "=" that keeps a stale
// or loose session name from prefix-matching its way onto a different live
// session. Proven against real tmux in attach_tmux_test.go; pinned here so the
// argument itself cannot be quietly dropped.
func TestTmuxAttachArgsForceExactSessionMatching(t *testing.T) {
	got := tmuxAttachArgs("claude-dev", false)
	if !slices.Contains(got, "=claude-dev") {
		t.Fatalf("args = %v, want the target exact-matched as =claude-dev", got)
	}
	if slices.Contains(got, "-r") {
		t.Fatalf("args = %v, want no read-only flag on a read-write attach", got)
	}
	if got := tmuxAttachArgs("claude-dev", true); !slices.Contains(got, "-r") {
		t.Fatalf("args = %v, want tmux's own read-only flag asked for", got)
	}
}

// TestReapAttachProcessGivesUpWhenTheKillFails is the deadlock guard. Close
// releases the limiter slot — now a host-wide one — so a wait that never
// returns does not merely leak a goroutine: it holds a slot for the life of the
// process. A kill that fails must therefore end the wait, not extend it.
func TestReapAttachProcessGivesUpWhenTheKillFails(t *testing.T) {
	// A process that never exits, however it is asked: the wait outlives the
	// test, which is exactly the leak reapAttachProcess chooses over blocking.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	killed := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		reapAttachProcess(
			func() error { <-stuck; return nil },
			func() error { close(killed); return errors.New("kill: no such process") },
			10*time.Millisecond, 10*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reap blocked forever after the kill failed — Close would hold its attach slot for the life of the server")
	}
	select {
	case <-killed:
	default:
		t.Fatal("the stuck process was never killed")
	}
}

// TestReapAttachProcessWaitsForACleanExit: the grace period is a grace period.
// A client that answers the hangup on its own is reaped without being killed.
func TestReapAttachProcessWaitsForACleanExit(t *testing.T) {
	killed := false
	reapAttachProcess(
		func() error { return nil },
		func() error { killed = true; return nil },
		5*time.Second, 5*time.Second)
	if killed {
		t.Fatal("a client that exited on its own was killed anyway")
	}
}

// TestAttachActivityKeepsItsMonotonicClockReading is the whole reason the idle
// timer holds a time.Time rather than a nanosecond count. UnixNano throws away
// Go's monotonic reading, and with it the guarantee that the idle window is
// measured in elapsed time: a wall clock stepped backwards — an NTP correction,
// a hand-set clock — would extend a connection's life past the timeout.
func TestAttachActivityKeepsItsMonotonicClockReading(t *testing.T) {
	var a attachActivity
	a.mark(time.Now())
	if a.at == a.at.Round(0) {
		t.Fatal("the activity timestamp has no monotonic clock reading: a backwards wall-clock step would extend the idle window")
	}
	if got := a.idleFor(a.at.Add(90 * time.Second)); got != 90*time.Second {
		t.Fatalf("idleFor = %v, want 90s", got)
	}
}

// TestAttachEnvSurvivesLaunchd: launchd starts this server with no TERM and no
// locale, and a `go test` (or a server) started from inside tmux passes TMUX
// down. All three break attach in a different way — see attachEnv.
func TestAttachEnvSurvivesLaunchd(t *testing.T) {
	got := attachEnv([]string{"PATH=/usr/bin", "TMUX=/tmp/tmux-501/default,123,0", "TMUX_PANE=%7"})
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Fatalf("env = %v, want PATH preserved", got)
	}
	if !slices.Contains(got, "TERM=xterm-256color") {
		t.Fatalf("env = %v, want an explicit TERM — tmux cannot attach without one", got)
	}
	if !slices.Contains(got, "LANG=en_US.UTF-8") {
		t.Fatalf("env = %v, want an explicit UTF-8 LANG", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Fatalf("env = %v, want TMUX dropped — a nested client refuses to attach", got)
		}
	}
}

func TestAttachEnvKeepsAUTF8LocaleAndDropsABrokenOne(t *testing.T) {
	got := attachEnv([]string{"LANG=en_GB.UTF-8", "LC_ALL=C", "LC_CTYPE=en_GB.UTF-8", "TERM=dumb"})
	if !slices.Contains(got, "LANG=en_GB.UTF-8") {
		t.Fatalf("env = %v, want the operator's own UTF-8 locale kept", got)
	}
	if !slices.Contains(got, "LC_CTYPE=en_GB.UTF-8") {
		t.Fatalf("env = %v, want a UTF-8 LC_CTYPE kept", got)
	}
	if slices.Contains(got, "LC_ALL=C") {
		t.Fatalf("env = %v, want LC_ALL=C dropped — it would undo LANG", got)
	}
	if slices.Contains(got, "TERM=dumb") {
		t.Fatalf("env = %v, want the server's own TERM replaced: this PTY is read by the phone", got)
	}
}

func TestAttachSizeFromQuery(t *testing.T) {
	got, err := attachSizeFromQuery(url.Values{})
	if err != nil || got != (attachSize{Cols: 80, Rows: 24}) {
		t.Fatalf("default size = %#v, err = %v", got, err)
	}
	got, err = attachSizeFromQuery(url.Values{"cols": {"120"}, "rows": {"40"}})
	if err != nil || got != (attachSize{Cols: 120, Rows: 40}) {
		t.Fatalf("size = %#v, err = %v", got, err)
	}
	if _, err := attachSizeFromQuery(url.Values{"cols": {"1001"}}); err == nil {
		t.Fatal("an out-of-range column count was accepted")
	}
}
