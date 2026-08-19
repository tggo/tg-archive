package store

import (
	"path/filepath"
	"testing"
)

func TestSaveMessageIsIdempotentAndUpdates(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := Message{ChatID: 1, ID: 10, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "a", Text: "first"}
	if err := st.SaveMessage(m); err != nil {
		t.Fatal(err)
	}
	m.Text, m.Edited = "corrected", "2026-08-19T09:05:00Z"
	if err := st.SaveMessage(m); err != nil {
		t.Fatal(err)
	}

	n, _ := st.Count("messages")
	if n != 1 {
		t.Fatalf("got %d messages, want 1 (upsert, not a duplicate)", n)
	}
	rows, _ := st.MessagesOfMonth(1, "2026-08")
	if rows[0].Text != "corrected" || rows[0].Edited == "" {
		t.Fatalf("edit was not persisted: %+v", rows[0])
	}
}

func TestStateTracksRangeAndBackfillFlag(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	for _, id := range []int{50, 10, 90} {
		if err := st.BumpState(7, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.GetState(7)
	if err != nil {
		t.Fatal(err)
	}
	if got.MinID != 10 || got.MaxID != 90 {
		t.Fatalf("state = %+v, want min 10 / max 90", got)
	}
	if got.BackfillDone {
		t.Error("backfill should not be marked done yet")
	}
	_ = st.SetBackfillDone(7)
	got, _ = st.GetState(7)
	if !got.BackfillDone {
		t.Error("SetBackfillDone had no effect")
	}
}

func TestPeerRoundTrip(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	if err := st.SavePeer(-1001234, "channel", 999, "chan"); err != nil {
		t.Fatal(err)
	}
	p, ok, err := st.Peer(-1001234)
	if err != nil || !ok || p.AccessHash != 999 || p.Type != "channel" {
		t.Fatalf("Peer() = %+v, %v, %v", p, ok, err)
	}
	if _, ok, _ := st.Peer(42); ok {
		t.Error("found a peer that was never saved")
	}
}
