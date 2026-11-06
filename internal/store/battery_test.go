package store

import (
	"context"
	"database/sql"
	"testing"
)

func batteryWatch(t *testing.T, s *Store) (User, int64) {
	t.Helper()
	ctx := context.Background()
	u, err := s.UpsertUser(ctx, "d1", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateWatch(ctx, Watch{UserID: u.ID, TargetKind: "node", TargetKey: "aa", NotifyRecovery: true}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return u, id
}

// The poller judges a rule as it was when the check started. If its owner
// changed or removed it meanwhile, the save must not land.
func TestSaveBatteryStateOnlyIfUnchanged(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, id := batteryWatch(t, s)
	if err := s.AddBatteryRule(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	old, _, _ := s.BatteryRuleFor(ctx, id)
	next := old
	next.Level, next.Announced = "low", "low"

	save := func(o BatteryRule) bool {
		var saved bool
		if err := s.WriteTx(ctx, func(tx *sql.Tx) error {
			var err error
			saved, err = s.SaveBatteryStateTx(ctx, tx, o, next)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	stale := old
	stale.Level = "ok"
	if save(stale) {
		t.Fatal("saved over a rule that had changed since it was read")
	}
	if !save(old) {
		t.Fatal("did not save over an unchanged rule")
	}
	if err := s.DeleteBatteryRule(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	if save(next) {
		t.Fatal("saved a rule its owner had removed")
	}
}

// The automatic add re-checks the owner's choice inside the transaction.
func TestAutoBatteryRuleRespectsChoicesMadeMeanwhile(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	u, id := batteryWatch(t, s)
	add := func() bool {
		var ok bool
		if err := s.WriteTx(ctx, func(tx *sql.Tx) error {
			var err error
			ok, err = s.InsertAutoBatteryRuleTx(ctx, tx, BatteryRule{WatchID: id, Level: "ok"}, s.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := s.SetAutoBattery(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if add() {
		t.Fatal("added with automatic adding turned off")
	}
	if err := s.SetAutoBattery(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if !add() {
		t.Fatal("not added when it should be")
	}
	if err := s.DeleteBatteryRule(ctx, u.ID, id); err != nil {
		t.Fatal(err)
	}
	if add() {
		t.Fatal("a removed rule was added back")
	}
}
