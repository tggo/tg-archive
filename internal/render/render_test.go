package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tggo/tg-archive/internal/store"
)

func TestMonthRendersEditsRepliesAndDeletions(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertChat(store.Chat{ID: 1, Kind: "private", Title: "Анна", Slug: "anna-1"}); err != nil {
		t.Fatal(err)
	}
	msgs := []store.Message{
		{ChatID: 1, ID: 1, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "Анна", Text: "привіт\nяк ти?"},
		{ChatID: 1, ID: 2, Date: "2026-08-19T09:01:00Z", Month: "2026-08", Sender: "я", Out: true,
			Text: "норм", ReplyTo: 1, Edited: "2026-08-19T09:02:00Z"},
		{ChatID: 1, ID: 3, Date: "2026-08-19T09:03:00Z", Month: "2026-08", Sender: "Анна", Media: "voice 12s"},
	}
	for _, m := range msgs {
		if err := st.SaveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkDeleted(1, 3); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "archive")
	n, err := New(st, out, time.UTC).Flush()
	if err != nil || n != 1 {
		t.Fatalf("Flush() = %d, %v", n, err)
	}

	b, err := os.ReadFile(filepath.Join(out, "chats", "anna-1", "2026-08.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"chat: \"Анна\"", "## 2026-08-19",
		"  привіт\n  як ти?", // multi-line text keeps its indent
		"↳ replying to “привіт як ти?”", // reply pulls in the target text
		"(edited)", "[voice 12s]", "(DELETED)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("file is missing %q\n---\n%s", want, got)
		}
	}

	// a second Flush is a no-op: the dirty set was drained
	if n, err := New(st, out, time.UTC).Flush(); err != nil || n != 0 {
		t.Errorf("second Flush() = %d, %v; want 0", n, err)
	}
}

func TestIndexListsOnlyChatsWithMessages(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "state.db"))
	defer st.Close()
	_ = st.UpsertChat(store.Chat{ID: 1, Kind: "private", Title: "З повідомленнями", Slug: "a-1"})
	_ = st.UpsertChat(store.Chat{ID: 2, Kind: "private", Title: "Порожній", Slug: "b-2"})
	_ = st.SaveMessage(store.Message{ChatID: 1, ID: 1, Date: "2026-08-19T09:00:00Z", Month: "2026-08", Sender: "x", Text: "hi"})

	out := filepath.Join(dir, "archive")
	if _, err := New(st, out, time.UTC).Flush(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "index.md"))
	if !strings.Contains(string(b), "З повідомленнями") {
		t.Error("index.md is missing a chat that has messages")
	}
	if strings.Contains(string(b), "Порожній") {
		t.Error("index.md lists a chat with no messages")
	}
}
