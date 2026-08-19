// Package store is the SQLite source of truth. Markdown can always be rebuilt from it.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS chats (
    id       INTEGER PRIMARY KEY,
    kind     TEXT,
    title    TEXT,
    username TEXT,
    slug     TEXT UNIQUE
);
CREATE TABLE IF NOT EXISTS messages (
    chat_id   INTEGER,
    id        INTEGER,
    date      TEXT,
    month     TEXT,
    sender_id INTEGER,
    sender    TEXT,
    out       INTEGER,
    text      TEXT,
    reply_to  INTEGER,
    media     TEXT,
    fwd       TEXT,
    edited    TEXT,
    deleted   INTEGER DEFAULT 0,
    reactions TEXT,           -- "👍3 ❤️1", rendered as-is
    file      TEXT,           -- relative path under the archive, once media is downloaded
    PRIMARY KEY (chat_id, id)
);
CREATE INDEX IF NOT EXISTS idx_msg_month ON messages(chat_id, month);
CREATE TABLE IF NOT EXISTS state (
    chat_id       INTEGER PRIMARY KEY,
    min_id        INTEGER,
    max_id        INTEGER,
    backfill_done INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS dirty (
    chat_id INTEGER,
    month   TEXT,
    PRIMARY KEY (chat_id, month)
);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT);
-- Full-text index. Kept in sync by triggers; unicode61 folds case for Cyrillic too,
-- which plain LIKE does not do.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    text,
    content='messages',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
    INSERT INTO messages_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TABLE IF NOT EXISTS peers (
    id          INTEGER PRIMARY KEY,   -- marked id (Telethon style: user>0, chat<0, channel -100…)
    type        TEXT,                  -- user | chat | channel
    access_hash INTEGER,
    username    TEXT
);
`

type Store struct{ db *sql.DB }

type Chat struct {
	ID       int64
	Kind     string
	Title    string
	Username string
	Slug     string
}

type Message struct {
	ChatID    int64
	ID        int
	Date      string // RFC3339 UTC
	Month     string // YYYY-MM in the configured timezone
	SenderID  int64
	Sender    string
	Out       bool
	Text      string
	ReplyTo   int
	Media     string
	Fwd       string
	Edited    string
	Deleted   bool
	Reactions string
	File      string
}

type State struct {
	MinID, MaxID int
	BackfillDone bool
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite + WAL: a single writer avoids SQLITE_BUSY
	if err := migrate(db); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	st := &Store{db: db}
	if err := st.ensureFTS(); err != nil {
		return nil, err
	}
	return st, nil
}

// migrate adds columns that older databases lack, before the schema (and its triggers)
// are applied. Archives created by earlier versions must keep working untouched.
func migrate(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range []string{"reactions", "file"} {
		if !have[col] {
			if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN ` + col + ` TEXT`); err != nil {
				return err
			}
		}
	}
	return nil
}

// ftsVersion bumps whenever the index definition changes, forcing one rebuild.
const ftsVersion = "1"

// ensureFTS builds the full-text index once, for archives that predate it.
//
// Do not test this with COUNT(*) on the FTS table: with content='messages' that counts the
// source table, not the index, so an empty index reports as full. The meta flag is the
// honest signal; messages_fts_data is the fallback check for a genuinely empty index.
func (s *Store) ensureFTS() error {
	var built string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='fts_version'`).Scan(&built)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var indexRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages_fts_data`).Scan(&indexRows); err != nil {
		return err
	}
	if built == ftsVersion && indexRows > 2 {
		return nil
	}
	if _, err := s.db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO meta(k,v) VALUES('fts_version',?)
	                    ON CONFLICT(k) DO UPDATE SET v=excluded.v`, ftsVersion)
	return err
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) UpsertChat(c Chat) error {
	_, err := s.db.Exec(
		`INSERT INTO chats(id,kind,title,username,slug) VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, title=excluded.title, username=excluded.username`,
		c.ID, c.Kind, c.Title, c.Username, c.Slug)
	return err
}

func (s *Store) SaveMessage(m Message) error {
	_, err := s.db.Exec(
		`INSERT INTO messages(chat_id,id,date,month,sender_id,sender,out,text,reply_to,media,fwd,edited,deleted,reactions,file)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)
		 ON CONFLICT(chat_id,id) DO UPDATE SET
		   text=excluded.text, media=excluded.media, edited=excluded.edited,
		   sender=excluded.sender, fwd=excluded.fwd, deleted=0,
		   reactions=excluded.reactions,
		   -- keep an already downloaded file if this update carries none
		   file=COALESCE(excluded.file, messages.file)`,
		m.ChatID, m.ID, m.Date, m.Month, m.SenderID, m.Sender, b2i(m.Out), m.Text,
		m.ReplyTo, nullable(m.Media), nullable(m.Fwd), nullable(m.Edited),
		nullable(m.Reactions), nullable(m.File))
	if err != nil {
		return err
	}
	return s.MarkDirty(m.ChatID, m.Month)
}

func (s *Store) MarkDeleted(chatID int64, msgID int) error {
	var month string
	err := s.db.QueryRow(`SELECT month FROM messages WHERE chat_id=? AND id=?`, chatID, msgID).Scan(&month)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE messages SET deleted=1 WHERE chat_id=? AND id=?`, chatID, msgID); err != nil {
		return err
	}
	return s.MarkDirty(chatID, month)
}

// FindChatByMessage locates the chat by message id — needed for delete updates, which in
// private chats arrive without naming their peer.
func (s *Store) FindChatByMessage(msgID int) (int64, bool, error) {
	var chatID int64
	err := s.db.QueryRow(`SELECT chat_id FROM messages WHERE id=? LIMIT 1`, msgID).Scan(&chatID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return chatID, err == nil, err
}

func (s *Store) MarkDirty(chatID int64, month string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO dirty(chat_id,month) VALUES(?,?)`, chatID, month)
	return err
}

func (s *Store) BumpState(chatID int64, msgID int) error {
	_, err := s.db.Exec(
		`INSERT INTO state(chat_id,min_id,max_id) VALUES(?,?,?)
		 ON CONFLICT(chat_id) DO UPDATE SET min_id=MIN(min_id,excluded.min_id), max_id=MAX(max_id,excluded.max_id)`,
		chatID, msgID, msgID)
	return err
}

func (s *Store) GetState(chatID int64) (State, error) {
	var st State
	var done int
	err := s.db.QueryRow(`SELECT min_id,max_id,backfill_done FROM state WHERE chat_id=?`, chatID).
		Scan(&st.MinID, &st.MaxID, &done)
	if err == sql.ErrNoRows {
		return State{}, nil
	}
	st.BackfillDone = done == 1
	return st, err
}

func (s *Store) SetBackfillDone(chatID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO state(chat_id,min_id,max_id,backfill_done) VALUES(?,0,0,1)
		 ON CONFLICT(chat_id) DO UPDATE SET backfill_done=1`, chatID)
	return err
}

func (s *Store) TakeDirty() ([][2]any, error) {
	rows, err := s.db.Query(`SELECT chat_id, month FROM dirty`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]any
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out = append(out, [2]any{id, m})
	}
	return out, rows.Err()
}

func (s *Store) ClearDirty(chatID int64, month string) error {
	_, err := s.db.Exec(`DELETE FROM dirty WHERE chat_id=? AND month=?`, chatID, month)
	return err
}

func (s *Store) MarkAllDirty() error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO dirty(chat_id,month) SELECT DISTINCT chat_id,month FROM messages`)
	return err
}

func (s *Store) Chat(id int64) (Chat, error) {
	var c Chat
	var username, slug sql.NullString
	err := s.db.QueryRow(`SELECT id,kind,title,username,slug FROM chats WHERE id=?`, id).
		Scan(&c.ID, &c.Kind, &c.Title, &username, &slug)
	c.Username, c.Slug = username.String, slug.String
	return c, err
}

func (s *Store) Chats() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id,kind,title,IFNULL(username,''),IFNULL(slug,'') FROM chats ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Username, &c.Slug); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchChats finds a chat by title fragment or @username (for `send --chat`).
func (s *Store) SearchChats(q string) ([]Chat, error) {
	rows, err := s.db.Query(
		`SELECT id,kind,title,IFNULL(username,''),IFNULL(slug,'') FROM chats
		 WHERE title LIKE ? OR username LIKE ? ORDER BY title LIMIT 10`, "%"+q+"%", "%"+q+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Username, &c.Slug); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) MessagesOfMonth(chatID int64, month string) ([]Message, error) {
	msgs, err := s.query(`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND month=? ORDER BY id DESC`,
		chatID, month)
	return msgs, err
}

type ChatSummary struct {
	Chat
	Count int
	Last  string
}

func (s *Store) Summary() ([]ChatSummary, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.kind, c.title, IFNULL(c.username,''), IFNULL(c.slug,''),
		        COUNT(m.id), IFNULL(MAX(m.date),'')
		 FROM chats c LEFT JOIN messages m ON m.chat_id=c.id
		 GROUP BY c.id HAVING COUNT(m.id) > 0 ORDER BY MAX(m.date) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSummary
	for rows.Next() {
		var c ChatSummary
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Username, &c.Slug, &c.Count, &c.Last); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Count(table string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n, err
}

// SavePeer records the access_hash, without which we can neither send nor fetch history.
func (s *Store) SavePeer(id int64, typ string, accessHash int64, username string) error {
	_, err := s.db.Exec(
		`INSERT INTO peers(id,type,access_hash,username) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET type=excluded.type, access_hash=excluded.access_hash, username=excluded.username`,
		id, typ, accessHash, username)
	return err
}

type Peer struct {
	ID         int64
	Type       string
	AccessHash int64
	Username   string
}

func (s *Store) Peer(id int64) (Peer, bool, error) {
	var p Peer
	err := s.db.QueryRow(`SELECT id,type,access_hash,IFNULL(username,'') FROM peers WHERE id=?`, id).
		Scan(&p.ID, &p.Type, &p.AccessHash, &p.Username)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	return p, err == nil, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Tail returns the newest messages of a chat, oldest-first for readability.
func (s *Store) Tail(chatID int64, limit int) ([]Message, error) {
	return s.query(
		`SELECT `+msgCols+` FROM messages WHERE chat_id=?
		 ORDER BY id DESC LIMIT ?`, chatID, limit)
}

// Before returns messages older than a given message id (for paging back).
func (s *Store) Before(chatID int64, beforeID, limit int) ([]Message, error) {
	return s.query(
		`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND id<?
		 ORDER BY id DESC LIMIT ?`, chatID, beforeID, limit)
}

// Around returns messages surrounding a given message id, so a search hit has context.
func (s *Store) Around(chatID int64, msgID, span int) ([]Message, error) {
	return s.query(
		`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND id BETWEEN ? AND ?
		 ORDER BY id DESC LIMIT ?`, chatID, msgID-span, msgID+span, span*2+1)
}

// SearchOpts narrows a search. Zero values mean "no constraint".
type SearchOpts struct {
	ChatID int64
	From   string // YYYY-MM-DD, inclusive
	To     string // YYYY-MM-DD, inclusive
	Sender string
	Limit  int
}

// Search runs a full-text query. FTS5 with unicode61 folds case for Cyrillic, which LIKE
// does not, and it ranks by relevance instead of scanning every row.
func (s *Store) Search(text string, o SearchOpts) ([]Message, error) {
	q := `SELECT ` + msgColsM + `
	      FROM messages_fts f JOIN messages m ON m.rowid = f.rowid
	      WHERE messages_fts MATCH ? AND m.deleted=0`
	args := []any{ftsQuery(text)}
	if o.ChatID != 0 {
		q += ` AND m.chat_id=?`
		args = append(args, o.ChatID)
	}
	if o.Sender != "" {
		q += ` AND m.sender LIKE ?`
		args = append(args, "%"+o.Sender+"%")
	}
	q, args = withDates(q, args, "m.", o.From, o.To)
	q += ` ORDER BY m.date DESC LIMIT ?`
	args = append(args, o.Limit)

	msgs, err := s.query(q, args...)
	if err == nil || !isFTSSyntaxErr(err) {
		return msgs, err
	}
	// A query FTS5 cannot parse (stray quote, bare punctuation) falls back to substring
	// matching rather than handing the user a syntax error they did not write.
	return s.searchLike(text, o)
}

func (s *Store) searchLike(text string, o SearchOpts) ([]Message, error) {
	q := `SELECT ` + msgColsM + ` FROM messages m
	      WHERE m.text LIKE ? AND m.deleted=0`
	args := []any{"%" + text + "%"}
	if o.ChatID != 0 {
		q += ` AND m.chat_id=?`
		args = append(args, o.ChatID)
	}
	q, args = withDates(q, args, "m.", o.From, o.To)
	q += ` ORDER BY m.date DESC LIMIT ?`
	args = append(args, o.Limit)
	return s.query(q, args...)
}

// Range returns messages of a chat between two dates (inclusive), oldest-first.
func (s *Store) Range(chatID int64, from, to string, limit int) ([]Message, error) {
	q := `SELECT ` + msgCols + ` FROM messages WHERE chat_id=?`
	args := []any{chatID}
	q, args = withDates(q, args, "", from, to)
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.query(q, args...)
}

// withDates appends half-open date bounds. Dates are compared as ISO strings, which sorts
// correctly because every stored date is RFC3339 UTC.
func withDates(q string, args []any, prefix, from, to string) (string, []any) {
	if from != "" {
		q += ` AND ` + prefix + `date >= ?`
		args = append(args, from+"T00:00:00Z")
	}
	if to != "" {
		q += ` AND ` + prefix + `date <= ?`
		args = append(args, to+"T23:59:59Z")
	}
	return q, args
}

// ftsQuery turns human input into an FTS5 expression: bare words are ANDed, a quoted
// "exact phrase" is kept as a phrase, and a trailing * still works as a prefix search.
func ftsQuery(in string) string {
	in = strings.TrimSpace(in)
	if strings.Contains(in, `"`) {
		return in
	}
	fields := strings.Fields(in)
	for i, f := range fields {
		if strings.HasSuffix(f, "*") {
			fields[i] = `"` + strings.TrimSuffix(f, "*") + `"*`
			continue
		}
		fields[i] = `"` + f + `"`
	}
	return strings.Join(fields, " ")
}

func isFTSSyntaxErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "fts5")
}

// Gap is a hole in a chat's archived history: ids between two stored messages that were
// never fetched, usually because a backfill was interrupted.
type Gap struct {
	ChatID     int64
	Title      string
	AfterID    int // last id we have before the hole
	BeforeID   int // first id we have after the hole
	Missing    int // how many ids are unaccounted for
	AfterDate  string
	BeforeDate string
}

// Gaps finds holes in archived history — but only where the question is answerable.
//
// Telegram numbers messages per *account* in private chats and small groups, so a jump of
// 100k ids there just means you wrote elsewhere in between; treating that as a hole
// produces thousands of false alarms. Only supergroups and channels (marked id below
// -1000000000000) number messages per chat, so only they are checked.
func (s *Store) Gaps(minJump int) ([]Gap, error) {
	rows, err := s.db.Query(`
		SELECT chat_id, title, prev_id, id, prev_date, date FROM (
		    SELECT m.chat_id, c.title, m.id, m.date,
		           LAG(m.id)   OVER (PARTITION BY m.chat_id ORDER BY m.id) AS prev_id,
		           LAG(m.date) OVER (PARTITION BY m.chat_id ORDER BY m.id) AS prev_date
		    FROM messages m JOIN chats c ON c.id = m.chat_id
		    WHERE m.chat_id < -1000000000000
		)
		WHERE prev_id IS NOT NULL AND id - prev_id > ?
		ORDER BY id - prev_id DESC`, minJump)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gap
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.ChatID, &g.Title, &g.AfterID, &g.BeforeID, &g.AfterDate, &g.BeforeDate); err != nil {
			return nil, err
		}
		g.Missing = g.BeforeID - g.AfterID - 1
		out = append(out, g)
	}
	return out, rows.Err()
}

// Unfinished lists chats whose history was never walked to the beginning — the honest
// completeness check for private chats, where id gaps mean nothing.
func (s *Store) Unfinished() ([]ChatSummary, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.kind, c.title, IFNULL(c.username,''), IFNULL(c.slug,''),
		       COUNT(m.id), IFNULL(MIN(m.date),'')
		FROM chats c
		JOIN messages m ON m.chat_id = c.id
		LEFT JOIN state s ON s.chat_id = c.id
		WHERE IFNULL(s.backfill_done, 0) = 0
		GROUP BY c.id ORDER BY COUNT(m.id) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSummary
	for rows.Next() {
		var c ChatSummary
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Username, &c.Slug, &c.Count, &c.Last); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingMedia lists archived messages that carry media but no downloaded file yet.
func (s *Store) PendingMedia(chatID int64, limit int) ([]Message, error) {
	q := `SELECT ` + msgCols + ` FROM messages
	      WHERE media != '' AND media IS NOT NULL AND (file IS NULL OR file = '') AND deleted = 0`
	args := []any{}
	if chatID != 0 {
		q += ` AND chat_id = ?`
		args = append(args, chatID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.query(q, args...)
}

// SetFile records where a downloaded attachment landed.
func (s *Store) SetFile(chatID int64, msgID int, path string) error {
	if _, err := s.db.Exec(`UPDATE messages SET file=? WHERE chat_id=? AND id=?`, path, chatID, msgID); err != nil {
		return err
	}
	var month string
	if err := s.db.QueryRow(`SELECT month FROM messages WHERE chat_id=? AND id=?`, chatID, msgID).Scan(&month); err != nil {
		return err
	}
	return s.MarkDirty(chatID, month)
}

const msgCols = `chat_id,id,date,month,sender_id,sender,out,text,IFNULL(reply_to,0),
	IFNULL(media,''),IFNULL(fwd,''),IFNULL(edited,''),deleted,IFNULL(reactions,''),IFNULL(file,'')`

// same columns qualified with the alias used when joining the FTS table
const msgColsM = `m.chat_id,m.id,m.date,m.month,m.sender_id,m.sender,m.out,m.text,IFNULL(m.reply_to,0),
	IFNULL(m.media,''),IFNULL(m.fwd,''),IFNULL(m.edited,''),m.deleted,IFNULL(m.reactions,''),IFNULL(m.file,'')`

// query runs a message query and returns rows oldest-first regardless of SQL order.
func (s *Store) query(q string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var o, d int
		if err := rows.Scan(&m.ChatID, &m.ID, &m.Date, &m.Month, &m.SenderID, &m.Sender, &o,
			&m.Text, &m.ReplyTo, &m.Media, &m.Fwd, &m.Edited, &d, &m.Reactions, &m.File); err != nil {
			return nil, err
		}
		m.Out, m.Deleted = o == 1, d == 1
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
