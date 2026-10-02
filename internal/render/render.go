// Package render draws Markdown from the database. .md is always derived, never the source.
package render

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tggo/tg-archive/internal/store"
)

type Renderer struct {
	// mu serialises Flush and Index: the live writer, the resync loop and the media loop
	// all flush, and two renders of one month must not race to the same rename.
	mu     sync.Mutex
	st     *store.Store
	outDir string
	loc    *time.Location
}

func New(st *store.Store, outDir string, loc *time.Location) *Renderer {
	return &Renderer{st: st, outDir: outDir, loc: loc}
}

// errNoChat means messages exist for a chat whose row has not been saved yet.
var errNoChat = errors.New("chat not known yet")

// Month rebuilds a single chats/<slug>/<YYYY-MM>.md file.
func (r *Renderer) Month(chatID int64, month string) (bool, error) {
	chat, err := r.st.Chat(chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errNoChat
	}
	if err != nil {
		return false, err
	}
	msgs, err := r.st.MessagesOfMonth(chatID, month)
	if err != nil || len(msgs) == 0 {
		return false, err
	}
	byID := make(map[int]store.Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}

	var b strings.Builder
	fmt.Fprintf(&b, "---\nchat: %q\nchat_id: %d\nkind: %s\nmonth: %s\nmessages: %d\ngenerated: %s\ntags: [telegram, %s]\n---\n\n# %s — %s\n",
		strings.ReplaceAll(chat.Title, `"`, `'`), chat.ID, chat.Kind, month, len(msgs),
		time.Now().In(r.loc).Format("2006-01-02 15:04"), chat.Kind, chat.Title, month)

	day := ""
	for _, m := range msgs {
		t := parseTime(m.Date).In(r.loc)
		if d := t.Format("2006-01-02"); d != day {
			day = d
			fmt.Fprintf(&b, "\n## %s\n\n", d)
		}
		b.WriteString(line(m, t, byID))
		b.WriteString("\n\n")
	}

	path := filepath.Join(r.outDir, "chats", chat.Slug, month+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, writeAtomic(path, strings.TrimRight(b.String(), "\n")+"\n")
}

// writeAtomic replaces path in one rename, so an editor never sees a half-written file.
// The temp name is unique per call and synced first: a fixed ".tmp" let two writers
// interleave, and an unsynced rename can leave an empty file after a power loss.
func writeAtomic(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func line(m store.Message, t time.Time, byID map[int]store.Message) string {
	var meta []string
	if m.Fwd != "" {
		meta = append(meta, fmt.Sprintf("↪ fwd from *%s*", m.Fwd))
	}
	if m.ReplyTo != 0 {
		if target, ok := byID[m.ReplyTo]; ok {
			s := target.Text
			if s == "" {
				s = target.Media
			}
			meta = append(meta, fmt.Sprintf("↳ replying to “%s”", snippet(s, 60)))
		} else {
			meta = append(meta, fmt.Sprintf("↳ replying to #%d", m.ReplyTo))
		}
	}
	if m.Media != "" {
		if m.File != "" {
			// Obsidian embeds this; other editors show it as a normal link.
			meta = append(meta, fmt.Sprintf("![[%s]]", m.File))
		} else {
			meta = append(meta, "["+m.Media+"]")
		}
	}
	if m.Reactions != "" {
		meta = append(meta, m.Reactions)
	}
	if m.Edited != "" {
		meta = append(meta, "(edited)")
	}
	if m.Deleted {
		meta = append(meta, "(DELETED)")
	}

	head := fmt.Sprintf("**%s** · **%s**", t.Format("15:04"), m.Sender)
	if len(meta) > 0 {
		head += " " + strings.Join(meta, " ")
	}
	text := strings.TrimRight(m.Text, "\n")
	switch {
	case text == "":
	case strings.Contains(text, "\n"):
		lines := strings.Split(text, "\n")
		for i := range lines {
			lines[i] = "  " + lines[i]
		}
		head += "\n" + strings.Join(lines, "\n")
	default:
		head += ": " + text
	}
	return head + fmt.Sprintf("  <!-- #%d -->", m.ID)
}

// Flush redraws every dirty (chat, month) pair and returns how many files were written.
//
// The dirty mark is cleared before a month is drawn, not after: a message saved while it
// renders then marks it dirty again and the next Flush picks it up. Clearing afterwards
// silently dropped that change. A month that fails keeps its mark and does not stop the
// others from being drawn.
func (r *Renderer) Flush() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dirty, err := r.st.TakeDirty()
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, d := range dirty {
		chatID, month := d[0].(int64), d[1].(string)
		if err := r.st.ClearDirty(chatID, month); err != nil {
			return n, err
		}
		ok, err := r.Month(chatID, month)
		if err != nil {
			if merr := r.st.MarkDirty(chatID, month); merr != nil {
				return n, merr
			}
			if !errors.Is(err, errNoChat) { // the chat row usually lands moments later
				errs = append(errs, fmt.Errorf("chat %d %s: %w", chatID, month, err))
			}
			continue
		}
		if ok {
			n++
		}
	}
	if n > 0 {
		if err := r.index(); err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// Index writes index.md — a table of chats with message counts.
func (r *Renderer) Index() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.index()
}

func (r *Renderer) index() error {
	rows, err := r.st.Summary()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("---\ntags: [telegram, index]\n---\n\n# Telegram archive\n\n")
	fmt.Fprintf(&b, "Updated: %s\n\n", time.Now().In(r.loc).Format("2006-01-02 15:04"))
	b.WriteString("| Chat | Type | Messages | Last |\n|---|---|---:|---|\n")
	for _, c := range rows {
		last := ""
		if c.Last != "" {
			last = parseTime(c.Last).In(r.loc).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "| [[chats/%s/|%s]] | %s | %d | %s |\n",
			c.Slug, strings.ReplaceAll(c.Title, "|", `\|`), c.Kind, c.Count, last)
	}
	if err := os.MkdirAll(r.outDir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.outDir, "index.md"), b.String())
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t2, err2 := time.Parse("2006-01-02T15:04:05-07:00", s)
		if err2 == nil {
			return t2
		}
	}
	return t
}

func snippet(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n])
	}
	return s
}
