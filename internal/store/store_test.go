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

	m := Message{ChatID: 1, ID: 10, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "a", Text: "перший"}
	if err := st.SaveMessage(m); err != nil {
		t.Fatal(err)
	}
	m.Text, m.Edited = "виправлений", "2026-08-19T09:05:00Z"
	if err := st.SaveMessage(m); err != nil {
		t.Fatal(err)
	}

	n, _ := st.Count("messages")
	if n != 1 {
		t.Fatalf("повідомлень %d, хотіли 1 (upsert, не дубль)", n)
	}
	rows, _ := st.MessagesOfMonth(1, "2026-08")
	if rows[0].Text != "виправлений" || rows[0].Edited == "" {
		t.Fatalf("редагування не збереглося: %+v", rows[0])
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
		t.Fatalf("state = %+v, хотіли min 10 / max 90", got)
	}
	if got.BackfillDone {
		t.Error("backfill не мав бути позначений завершеним")
	}
	_ = st.SetBackfillDone(7)
	got, _ = st.GetState(7)
	if !got.BackfillDone {
		t.Error("SetBackfillDone не спрацював")
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
		t.Error("неіснуючий peer знайдено")
	}
}
