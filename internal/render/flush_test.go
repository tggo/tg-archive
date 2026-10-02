package render

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tggo/tg-archive/internal/store"
)

// A month whose chat row is not saved yet must stay dirty, not be silently dropped.
func TestFlushKeepsMonthOfUnknownChat(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "s.db"))
	defer st.Close()
	_ = st.SaveMessage(store.Message{ChatID: 9, ID: 1, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "x", Text: "hi"})
	r := New(st, dir, time.UTC)
	if n, err := r.Flush(); n != 0 || err != nil {
		t.Fatalf("Flush = %d, %v; want 0, nil", n, err)
	}
	_ = st.UpsertChat(store.Chat{ID: 9, Kind: "private", Title: "X", Slug: "x-9"})
	if n, err := r.Flush(); n != 1 || err != nil {
		t.Fatalf("second Flush = %d, %v; want the month drawn now", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chats", "x-9", "2026-08.md")); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "chats", "x-9", ".*tmp")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
