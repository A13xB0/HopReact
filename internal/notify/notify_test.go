package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hopreact/internal/discord"
	"hopreact/internal/poller"
	"hopreact/internal/store"
)

var t0 = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// recorder stands in for Discord and can be told to fail.
type recorder struct {
	sent []struct{ user, content string }
	err  error
	// failFirst makes only the first attempt fail, so retry behaviour is
	// observable.
	failFirst bool
	calls     int
}

func (r *recorder) SendDM(ctx context.Context, userID, content string) error {
	r.calls++
	if r.err != nil && (!r.failFirst || r.calls == 1) {
		return r.err
	}
	r.sent = append(r.sent, struct{ user, content string }{userID, content})
	return nil
}

func harness(t *testing.T) (*store.Store, *recorder, *Notifier, *time.Time) {
	t.Helper()
	now := t0
	st, err := store.Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := &recorder{}
	n := &Notifier{
		Store: st, Sender: rec,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		BaseURL: "https://hopreact.example",
		Now:     func() time.Time { return now },
	}
	return st, rec, n, &now
}

func queue(t *testing.T, st *store.Store, userID int64, kind, name, ruleLabel string) {
	t.Helper()
	payload, _ := json.Marshal(poller.AlertPayload{
		Kind: kind, TargetKind: "node", TargetKey: "aa", TargetName: name,
		Rules: []poller.RuleTrip{{
			Label: ruleLabel, ThresholdHours: 6,
			LastSeenUnix: t0.Add(-8 * time.Hour).Unix(),
		}},
	})
	if err := st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return st.QueueNotification(context.Background(), tx, userID, kind, string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSendsAndMarksSent(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queue(t, st, u.ID, "alert", "Ben Nevis", "seen")

	sent, err := n.DrainOnce(ctx, 10)
	if err != nil || sent != 1 {
		t.Fatalf("DrainOnce = %d, %v", sent, err)
	}
	if len(rec.sent) != 1 || rec.sent[0].user != "d1" {
		t.Fatalf("sent %+v", rec.sent)
	}
	if !strings.Contains(rec.sent[0].content, "Ben Nevis") {
		t.Errorf("message should name the node, got %q", rec.sent[0].content)
	}
	// Nothing left to send.
	if again, _ := n.DrainOnce(ctx, 10); again != 0 {
		t.Error("a sent message must not be sent twice")
	}
}

// Several transitions for one person in the same poll become ONE message.
// This is a courtesy, and also a structural limit on how much noise any
// single event can produce.
func TestBatchesPerUser(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	for i := 0; i < 4; i++ {
		queue(t, st, u.ID, "alert", fmt.Sprintf("node-%d", i), "seen")
	}

	sent, err := n.DrainOnce(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 4 {
		t.Errorf("marked %d sent, want 4", sent)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("made %d DM calls, want them batched into 1", len(rec.sent))
	}
	body := rec.sent[0].content
	for i := 0; i < 4; i++ {
		if !strings.Contains(body, fmt.Sprintf("node-%d", i)) {
			t.Errorf("batched message is missing node-%d: %q", i, body)
		}
	}
	if !strings.Contains(body, "4 of your nodes") {
		t.Errorf("message should lead with the count, got %q", body)
	}
}

// Different people must not be merged into one another's message.
func TestDoesNotMixUsers(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	a, _ := st.UpsertUser(ctx, "d1", "alice", "")
	b, _ := st.UpsertUser(ctx, "d2", "bob", "")
	queue(t, st, a.ID, "alert", "alice-node", "seen")
	queue(t, st, b.ID, "alert", "bob-node", "seen")

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 2 {
		t.Fatalf("made %d calls, want one per user", len(rec.sent))
	}
	for _, s := range rec.sent {
		if s.user == "d1" && strings.Contains(s.content, "bob-node") {
			t.Error("alice was told about bob's node")
		}
		if s.user == "d2" && strings.Contains(s.content, "alice-node") {
			t.Error("bob was told about alice's node")
		}
	}
}

// An undeliverable user is recorded and the message dropped: retrying can
// never fix "you left the server", and the dashboard is where they will find
// out why they hear nothing.
func TestUndeliverableRecordsStatusAndStopsRetrying(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queue(t, st, u.ID, "alert", "Ben Nevis", "seen")
	rec.err = fmt.Errorf("%w: Cannot send messages to this user", discord.ErrUndeliverable)

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserByID(ctx, u.ID)
	if got.DMOK {
		t.Error("the user should be marked undeliverable")
	}
	if got.DMFailedReason == "" {
		t.Error("a reason must be recorded — silence with no explanation is the worst outcome")
	}
	// Not requeued.
	if pending, _ := st.PendingNotifications(ctx, 10); len(pending) != 0 {
		t.Errorf("%d messages still pending, want them dropped", len(pending))
	}
}

// A transient failure must be retried, not dropped, and must not mark the
// user unreachable.
func TestTransientFailureRetries(t *testing.T) {
	st, rec, n, now := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queue(t, st, u.ID, "alert", "Ben Nevis", "seen")
	rec.err = errors.New("502 bad gateway")
	rec.failFirst = true

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserByID(ctx, u.ID)
	if !got.DMOK {
		t.Error("a transient failure must not mark the user unreachable")
	}
	// Backed off, so not immediately due...
	if pending, _ := st.PendingNotifications(ctx, 10); len(pending) != 0 {
		t.Error("a failed message should be backed off, not immediately retried")
	}
	// ...but retried later, and delivered.
	*now = t0.Add(10 * time.Minute)
	sent, err := n.DrainOnce(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 || len(rec.sent) != 1 {
		t.Errorf("sent=%d calls=%d, want the retry to succeed", sent, len(rec.sent))
	}
}

// A successful send is also proof of reachability, so a previously-recorded
// problem must clear itself rather than needing manual intervention.
func TestSuccessClearsAPreviousDeliveryProblem(t *testing.T) {
	st, _, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	if err := st.SetDMStatus(ctx, u.ID, false, "left the server"); err != nil {
		t.Fatal(err)
	}
	queue(t, st, u.ID, "alert", "Ben Nevis", "seen")

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserByID(ctx, u.ID)
	if !got.DMOK || got.DMFailedReason != "" {
		t.Errorf("a successful send should clear the problem, got DMOK=%v reason=%q", got.DMOK, got.DMFailedReason)
	}
}

func TestRendersDownAndRecoveredSeparately(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queue(t, st, u.ID, "alert", "down-node", "seen")
	queue(t, st, u.ID, "recovered", "up-node", "seen")

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	body := rec.sent[0].content
	if !strings.Contains(body, "gone quiet") || !strings.Contains(body, "Back online") {
		t.Errorf("message should separate the two, got:\n%s", body)
	}
	if !strings.Contains(body, "hopreact.example") {
		t.Error("message should link back so the user can manage alerts")
	}
}

// A relay alert has to read differently from a plain one, or the message is
// misleading — the node might be perfectly reachable and simply not passing
// anything on. Each rule names itself in the message, which is what carries
// that distinction now that a watch can hold several.
func TestRelaySignalReadsDifferently(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")
	queue(t, st, u.ID, "alert", "relay-node", "Stopped passing traffic")

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.sent[0].content, "Stopped passing traffic") {
		t.Errorf("expected the relay wording, got %q", rec.sent[0].content)
	}
}

// A watch whose rules all trip in the same poll must arrive as ONE bullet
// listing its reasons. Setting up careful rules should sharpen the alerting,
// not multiply the pings.
func TestSeveralRulesOnOneWatchReadAsOneEntry(t *testing.T) {
	st, rec, n, _ := harness(t)
	ctx := context.Background()
	u, _ := st.UpsertUser(ctx, "d1", "alice", "")

	payload, _ := json.Marshal(poller.AlertPayload{
		Kind: "alert", TargetKind: "node", TargetKey: "aa", TargetName: "Ben Nevis",
		Rules: []poller.RuleTrip{
			{Label: "Adverts", ThresholdHours: 6, LastSeenUnix: t0.Add(-8 * time.Hour).Unix()},
			{Label: "Channel messages", ThresholdHours: 12, LastSeenUnix: t0.Add(-20 * time.Hour).Unix()},
		},
	})
	if err := st.WriteTx(ctx, func(tx *sql.Tx) error {
		return st.QueueNotification(ctx, tx, u.ID, "alert", string(payload), time.Time{})
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := n.DrainOnce(ctx, 10); err != nil {
		t.Fatal(err)
	}
	got := rec.sent[0].content
	if strings.Count(got, "Ben Nevis") != 1 {
		t.Errorf("the node should be named once, not once per rule: %q", got)
	}
	for _, want := range []string{"Adverts", "Channel messages"} {
		if !strings.Contains(got, want) {
			t.Errorf("message should give the reason %q, got %q", want, got)
		}
	}
	if !strings.Contains(got, "One of your nodes") {
		t.Errorf("two rules on one node is still one node: %q", got)
	}
}
