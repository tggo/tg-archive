// Package store is the SQLite source of truth. Markdown can always be rebuilt from it.
package store

import (
	"database/sql"
	"os"
	"path/filepath"

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
	ChatID   int64
	ID       int
	Date     string // RFC3339 UTC
	Month    string // YYYY-MM in the configured timezone
	SenderID int64
	Sender   string
	Out      bool
	Text     string
	ReplyTo  int
	Media    string
	Fwd      string
	Edited   string
	Deleted  bool
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
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
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
		`INSERT INTO messages(chat_id,id,date,month,sender_id,sender,out,text,reply_to,media,fwd,edited,deleted)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0)
		 ON CONFLICT(chat_id,id) DO UPDATE SET
		   text=excluded.text, media=excluded.media, edited=excluded.edited,
		   sender=excluded.sender, fwd=excluded.fwd, deleted=0`,
		m.ChatID, m.ID, m.Date, m.Month, m.SenderID, m.Sender, b2i(m.Out), m.Text,
		m.ReplyTo, nullable(m.Media), nullable(m.Fwd), nullable(m.Edited))
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
	rows, err := s.db.Query(
		`SELECT chat_id,id,date,month,sender_id,sender,out,text,IFNULL(reply_to,0),
		        IFNULL(media,''),IFNULL(fwd,''),IFNULL(edited,''),deleted
		 FROM messages WHERE chat_id=? AND month=? ORDER BY id`, chatID, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var out_, del int
		if err := rows.Scan(&m.ChatID, &m.ID, &m.Date, &m.Month, &m.SenderID, &m.Sender, &out_,
			&m.Text, &m.ReplyTo, &m.Media, &m.Fwd, &m.Edited, &del); err != nil {
			return nil, err
		}
		m.Out, m.Deleted = out_ == 1, del == 1
		out = append(out, m)
	}
	return out, rows.Err()
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

// Search does a substring match over message text, optionally within one chat.
func (s *Store) Search(text string, chatID int64, limit int) ([]Message, error) {
	if chatID != 0 {
		return s.query(
			`SELECT `+msgCols+` FROM messages WHERE chat_id=? AND text LIKE ? AND deleted=0
			 ORDER BY id DESC LIMIT ?`, chatID, "%"+text+"%", limit)
	}
	return s.query(
		`SELECT `+msgCols+` FROM messages WHERE text LIKE ? AND deleted=0
		 ORDER BY date DESC LIMIT ?`, "%"+text+"%", limit)
}

const msgCols = `chat_id,id,date,month,sender_id,sender,out,text,IFNULL(reply_to,0),
	IFNULL(media,''),IFNULL(fwd,''),IFNULL(edited,''),deleted`

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
			&m.Text, &m.ReplyTo, &m.Media, &m.Fwd, &m.Edited, &d); err != nil {
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
