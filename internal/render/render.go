// Package render малює Markdown із бази. .md — завжди похідне, ніколи не джерело.
package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tggo/tg-archive/internal/store"
)

type Renderer struct {
	st     *store.Store
	outDir string
	loc    *time.Location
}

func New(st *store.Store, outDir string, loc *time.Location) *Renderer {
	return &Renderer{st: st, outDir: outDir, loc: loc}
}

// Month перебудовує один файл chats/<slug>/<YYYY-MM>.md.
func (r *Renderer) Month(chatID int64, month string) (bool, error) {
	chat, err := r.st.Chat(chatID)
	if err != nil {
		return false, nil // чат ще не відомий — нічого малювати
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimRight(b.String(), "\n")+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path) // атомарно: Obsidian ніколи не бачить半-файл
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
			meta = append(meta, fmt.Sprintf("↳ у відповідь на «%s»", snippet(s, 60)))
		} else {
			meta = append(meta, fmt.Sprintf("↳ у відповідь на #%d", m.ReplyTo))
		}
	}
	if m.Media != "" {
		meta = append(meta, "["+m.Media+"]")
	}
	if m.Edited != "" {
		meta = append(meta, "(ред.)")
	}
	if m.Deleted {
		meta = append(meta, "(ВИДАЛЕНО)")
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

// Flush перемальовує всі «брудні» (чат, місяць) і повертає кількість файлів.
func (r *Renderer) Flush() (int, error) {
	dirty, err := r.st.TakeDirty()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range dirty {
		chatID, month := d[0].(int64), d[1].(string)
		ok, err := r.Month(chatID, month)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
		if err := r.st.ClearDirty(chatID, month); err != nil {
			return n, err
		}
	}
	if n > 0 {
		if err := r.Index(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Index пише index.md — таблицю чатів із кількістю повідомлень.
func (r *Renderer) Index() error {
	rows, err := r.st.Summary()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("---\ntags: [telegram, index]\n---\n\n# Telegram архів\n\n")
	fmt.Fprintf(&b, "Оновлено: %s\n\n", time.Now().In(r.loc).Format("2006-01-02 15:04"))
	b.WriteString("| Чат | Тип | Повідомлень | Останнє |\n|---|---|---:|---|\n")
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
	return os.WriteFile(filepath.Join(r.outDir, "index.md"), []byte(b.String()), 0o644)
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
