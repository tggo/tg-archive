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

func TestFullTextSearchFoldsCyrillicCase(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	_ = st.UpsertChat(Chat{ID: 1, Kind: "private", Title: "A", Slug: "a-1"})
	for i, text := range []string{"Дякую вам за все", "дякуємо за роботу", "нічого цікавого"} {
		if err := st.SaveMessage(Message{ChatID: 1, ID: i + 1, Date: "2026-08-19T09:00:00Z",
			Month: "2026-08", Sender: "x", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	// lowercase query must find the capitalised message: this is exactly what LIKE misses
	hits, err := st.Search("дякую", SearchOpts{Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search(дякую) = %d hits, %v; want 1", len(hits), err)
	}
	if pref, _ := st.Search("дяку*", SearchOpts{Limit: 10}); len(pref) != 2 {
		t.Errorf("prefix search found %d, want 2", len(pref))
	}
	if phrase, _ := st.Search(`"за все"`, SearchOpts{Limit: 10}); len(phrase) != 1 {
		t.Errorf("phrase search found %d, want 1", len(phrase))
	}
	// an edit must update the index, not leave the old text findable
	if err := st.SaveMessage(Message{ChatID: 1, ID: 1, Date: "2026-08-19T09:00:00Z",
		Month: "2026-08", Sender: "x", Text: "передумав"}); err != nil {
		t.Fatal(err)
	}
	if stale, _ := st.Search("дякую", SearchOpts{Limit: 10}); len(stale) != 0 {
		t.Errorf("edited-away text still findable: %d hits", len(stale))
	}
}

func TestSearchDateAndSenderFilters(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	_ = st.UpsertChat(Chat{ID: 1, Kind: "private", Title: "A", Slug: "a-1"})
	rows := []Message{
		{ChatID: 1, ID: 1, Date: "2026-06-01T09:00:00Z", Month: "2026-06", Sender: "Anna", Text: "trip plan"},
		{ChatID: 1, ID: 2, Date: "2026-07-01T09:00:00Z", Month: "2026-07", Sender: "Bob", Text: "trip plan"},
	}
	for _, m := range rows {
		if err := st.SaveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if h, _ := st.Search("trip", SearchOpts{From: "2026-06-15", Limit: 10}); len(h) != 1 || h[0].ID != 2 {
		t.Errorf("from-filter returned %d rows, want just #2", len(h))
	}
	if h, _ := st.Search("trip", SearchOpts{To: "2026-06-15", Limit: 10}); len(h) != 1 || h[0].ID != 1 {
		t.Errorf("to-filter returned %d rows, want just #1", len(h))
	}
	if h, _ := st.Search("trip", SearchOpts{Sender: "ann", Limit: 10}); len(h) != 1 || h[0].Sender != "Anna" {
		t.Errorf("sender filter returned %d rows, want Anna's", len(h))
	}
}

func TestGapsIgnorePrivateChats(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	// private chat: ids jump because Telegram numbers per account — not a hole
	_ = st.UpsertChat(Chat{ID: 42, Kind: "private", Title: "Private", Slug: "p-42"})
	_ = st.SaveMessage(Message{ChatID: 42, ID: 100, Date: "2026-01-01T09:00:00Z", Month: "2026-01", Sender: "x"})
	_ = st.SaveMessage(Message{ChatID: 42, ID: 90000, Date: "2026-02-01T09:00:00Z", Month: "2026-02", Sender: "x"})
	// supergroup: ids are per-chat, so a jump really is missing history
	_ = st.UpsertChat(Chat{ID: -1001234567890, Kind: "group", Title: "Super", Slug: "s-1"})
	_ = st.SaveMessage(Message{ChatID: -1001234567890, ID: 10, Date: "2026-01-01T09:00:00Z", Month: "2026-01", Sender: "x"})
	_ = st.SaveMessage(Message{ChatID: -1001234567890, ID: 900, Date: "2026-02-01T09:00:00Z", Month: "2026-02", Sender: "x"})

	gaps, err := st.Gaps(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1 (the supergroup only)", len(gaps))
	}
	if gaps[0].ChatID != -1001234567890 || gaps[0].Missing != 889 {
		t.Errorf("unexpected gap: %+v", gaps[0])
	}
}

func TestPendingMediaAndSetFile(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	_ = st.UpsertChat(Chat{ID: 1, Kind: "private", Title: "A", Slug: "a-1"})
	_ = st.SaveMessage(Message{ChatID: 1, ID: 1, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "x", Media: "photo"})
	_ = st.SaveMessage(Message{ChatID: 1, ID: 2, Date: "2026-08-19T09:01:00Z", Month: "2026-08", Sender: "x", Text: "no media"})

	pending, err := st.PendingMedia(0, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != 1 {
		t.Fatalf("PendingMedia = %d rows, %v; want just #1", len(pending), err)
	}
	if err := st.SetFile(1, 1, "attachments/a-1/1.jpg"); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.PendingMedia(0, 10); len(again) != 0 {
		t.Errorf("downloaded message still pending")
	}
	// re-saving the message (an edit, a resync) must not wipe the downloaded file
	_ = st.SaveMessage(Message{ChatID: 1, ID: 1, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "x", Media: "photo"})
	rows, _ := st.MessagesOfMonth(1, "2026-08")
	for _, m := range rows {
		if m.ID == 1 && m.File == "" {
			t.Error("re-saving the message dropped the downloaded file path")
		}
	}
}
