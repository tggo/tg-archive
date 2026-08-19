// Package mcpserver exposes the archive to an MCP client (Claude Code, Claude Desktop).
//
// Reads are served from the local SQLite archive, so they are instant and work even when
// Telegram is unreachable. Only sync and send touch the network, and sending is off unless
// the operator passes --allow-send.
package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tggo/tg-archive/internal/config"
	"github.com/tggo/tg-archive/internal/store"
	"github.com/tggo/tg-archive/internal/tgclient"
)

type Server struct {
	cfg       *config.Config
	st        *store.Store
	client    *tgclient.Client
	allowSend bool

	sess *tgclient.Session // opened lazily: read-only sessions never dial Telegram
}

func New(cfg *config.Config, st *store.Store, allowSend bool) *Server {
	return &Server{cfg: cfg, st: st, client: tgclient.New(cfg, st), allowSend: allowSend}
}

// Run serves MCP over stdio until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "tg-archive",
		Title:   "Telegram archive",
		Version: version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_chats",
		Description: "List archived Telegram chats: id, kind (private/group/saved), title, " +
			"message count and last activity. Use it to find the chat id other tools need.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.listChats)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "read_chat",
		Description: "Read messages of one chat from the local archive, oldest-first. Defaults " +
			"to the most recent messages; page back with before_id, or ask for a period with " +
			"from/to dates.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.readChat)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_messages",
		Description: "Full-text search over archived messages, newest first. Several words " +
			"must all appear; \"quoted words\" match an exact phrase, trailing * matches a " +
			"prefix. Can be narrowed by chat, sender and date range.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.searchMessages)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "archive_status",
		Description: "Where the archive lives and how much it holds: paths, message and chat " +
			"counts, most recently active chats.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.status)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "sync_chat",
		Description: "Fetch messages newer than the archive has for one chat, straight from " +
			"Telegram, and write them to the archive. Use before reading if freshness matters.",
	}, s.syncChat)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "download_media",
		Description: "Download attachments (photos, voice notes, files) for archived messages " +
			"that have none yet, so they can be opened from the Markdown. Requires media " +
			"downloading to be enabled in the config.",
	}, s.downloadMedia)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "check_archive",
		Description: "Report holes in the archived history: chats never walked back to the " +
			"beginning, and missing id ranges in supergroups and channels.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.checkArchive)

	if s.allowSend {
		mcp.AddTool(srv, &mcp.Tool{
			Name: "send_message",
			Description: "Send a Telegram message as the account that owns this archive. " +
				"This is visible to the recipient immediately and cannot be unsent by this tool. " +
				"Confirm the exact chat and text with the user before calling it.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		}, s.sendMessage)
	}

	if err := s.addResources(srv); err != nil {
		return err
	}

	return srv.Run(ctx, &mcp.StdioTransport{})
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- tools

type ListChatsIn struct {
	Query string `json:"query,omitempty" jsonschema:"filter by title or @username substring"`
	Kind  string `json:"kind,omitempty" jsonschema:"filter by kind: private, group, channel, saved"`
	Limit int    `json:"limit,omitempty" jsonschema:"max chats to return (default 50)"`
}

type ChatOut struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Username string `json:"username,omitempty"`
	Messages int    `json:"messages"`
	LastSeen string `json:"last_seen,omitempty"`
}

type ListChatsOut struct {
	Chats []ChatOut `json:"chats"`
	Total int       `json:"total"`
}

func (s *Server) listChats(ctx context.Context, _ *mcp.CallToolRequest, in ListChatsIn) (*mcp.CallToolResult, ListChatsOut, error) {
	rows, err := s.st.Summary()
	if err != nil {
		return nil, ListChatsOut{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	var out ListChatsOut
	var b strings.Builder
	for _, c := range rows {
		if in.Kind != "" && c.Kind != in.Kind {
			continue
		}
		if in.Query != "" && !containsFold(c.Title, in.Query) && !containsFold(c.Username, in.Query) {
			continue
		}
		out.Total++
		if len(out.Chats) >= limit {
			continue
		}
		item := ChatOut{ID: c.ID, Kind: c.Kind, Title: c.Title, Username: c.Username,
			Messages: c.Count, LastSeen: s.local(c.Last)}
		out.Chats = append(out.Chats, item)
		fmt.Fprintf(&b, "%16d  %-8s %-40s %6d msgs  last %s\n",
			item.ID, item.Kind, trunc(item.Title, 40), item.Messages, item.LastSeen)
	}
	if out.Total > len(out.Chats) {
		fmt.Fprintf(&b, "\n(%d more not shown; narrow with query/kind or raise limit)\n", out.Total-len(out.Chats))
	}
	return text(b.String()), out, nil
}

type ReadChatIn struct {
	Chat     string `json:"chat" jsonschema:"chat id, @username, or part of the title"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many messages (default 50, max 500)"`
	BeforeID int    `json:"before_id,omitempty" jsonschema:"return messages older than this message id"`
	AroundID int    `json:"around_id,omitempty" jsonschema:"return messages surrounding this message id"`
	From     string `json:"from,omitempty" jsonschema:"only messages on or after this date, YYYY-MM-DD"`
	To       string `json:"to,omitempty" jsonschema:"only messages on or before this date, YYYY-MM-DD"`
}

type MessageOut struct {
	ID        int    `json:"id"`
	Date      string `json:"date"`
	Sender    string `json:"sender"`
	Mine      bool   `json:"mine,omitempty"`
	Text      string `json:"text,omitempty"`
	Media     string `json:"media,omitempty"`
	ReplyTo   int    `json:"reply_to,omitempty"`
	Fwd       string `json:"forwarded_from,omitempty"`
	Edited    bool   `json:"edited,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
	Reactions string `json:"reactions,omitempty"`
	File      string `json:"file,omitempty"`
}

type ReadChatOut struct {
	ChatID   int64        `json:"chat_id"`
	Title    string       `json:"title"`
	Messages []MessageOut `json:"messages"`
}

func (s *Server) readChat(ctx context.Context, _ *mcp.CallToolRequest, in ReadChatIn) (*mcp.CallToolResult, ReadChatOut, error) {
	chat, err := s.resolve(in.Chat)
	if err != nil {
		return nil, ReadChatOut{}, err
	}
	limit := clamp(in.Limit, 50, 500)

	if err := checkDates(in.From, in.To); err != nil {
		return nil, ReadChatOut{}, err
	}
	var msgs []store.Message
	switch {
	case in.AroundID > 0:
		msgs, err = s.st.Around(chat.ID, in.AroundID, limit/2)
	case in.From != "" || in.To != "":
		msgs, err = s.st.Range(chat.ID, in.From, in.To, limit)
	case in.BeforeID > 0:
		msgs, err = s.st.Before(chat.ID, in.BeforeID, limit)
	default:
		msgs, err = s.st.Tail(chat.ID, limit)
	}
	if err != nil {
		return nil, ReadChatOut{}, err
	}

	out := ReadChatOut{ChatID: chat.ID, Title: chat.Title}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (id %d, %s)\n\n", chat.Title, chat.ID, chat.Kind)
	day := ""
	for _, m := range msgs {
		out.Messages = append(out.Messages, s.msgOut(m))
		t := s.time(m.Date)
		if d := t.Format("2006-01-02"); d != day {
			day = d
			fmt.Fprintf(&b, "-- %s\n", d)
		}
		fmt.Fprintf(&b, "[#%d %s] %s%s\n", m.ID, t.Format("15:04"), m.Sender, describeBody(m))
	}
	if len(msgs) > 0 {
		fmt.Fprintf(&b, "\n(oldest shown: #%d — pass before_id=%d to page back)\n", msgs[0].ID, msgs[0].ID)
	}
	return text(b.String()), out, nil
}

type SearchIn struct {
	Query  string `json:"query" jsonschema:"words to look for; several words must all appear. Use \"quoted words\" for an exact phrase and trailing * for a prefix"`
	Chat   string `json:"chat,omitempty" jsonschema:"limit the search to one chat"`
	Sender string `json:"sender,omitempty" jsonschema:"only messages from senders whose name contains this"`
	From   string `json:"from,omitempty" jsonschema:"only messages on or after this date, YYYY-MM-DD"`
	To     string `json:"to,omitempty" jsonschema:"only messages on or before this date, YYYY-MM-DD"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max hits (default 30, max 200)"`
}

type SearchHit struct {
	MessageOut
	ChatID int64  `json:"chat_id"`
	Chat   string `json:"chat"`
}

type SearchOut struct {
	Hits []SearchHit `json:"hits"`
}

func (s *Server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, SearchOut{}, fmt.Errorf("query is required")
	}
	var chatID int64
	title := ""
	if in.Chat != "" {
		c, err := s.resolve(in.Chat)
		if err != nil {
			return nil, SearchOut{}, err
		}
		chatID, title = c.ID, c.Title
	}
	if err := checkDates(in.From, in.To); err != nil {
		return nil, SearchOut{}, err
	}
	msgs, err := s.st.Search(in.Query, store.SearchOpts{
		ChatID: chatID, Sender: in.Sender, From: in.From, To: in.To,
		Limit: clamp(in.Limit, 30, 200),
	})
	if err != nil {
		return nil, SearchOut{}, err
	}
	var out SearchOut
	var b strings.Builder
	titles := map[int64]string{}
	for _, m := range msgs {
		name := title
		if name == "" {
			if t, ok := titles[m.ChatID]; ok {
				name = t
			} else if c, err := s.st.Chat(m.ChatID); err == nil {
				name, titles[m.ChatID] = c.Title, c.Title
			}
		}
		out.Hits = append(out.Hits, SearchHit{MessageOut: s.msgOut(m), ChatID: m.ChatID, Chat: name})
		fmt.Fprintf(&b, "%s [#%d %s] %s%s\n", name, m.ID,
			s.time(m.Date).Format("2006-01-02 15:04"), m.Sender, describeBody(m))
	}
	if len(out.Hits) == 0 {
		return text("no matches"), out, nil
	}
	return text(b.String()), out, nil
}

type StatusOut struct {
	Archive  string    `json:"archive_dir"`
	Database string    `json:"database"`
	Messages int       `json:"messages"`
	Chats    int       `json:"chats"`
	Recent   []ChatOut `json:"recent"`
}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOut, error) {
	msgs, err := s.st.Count("messages")
	if err != nil {
		return nil, StatusOut{}, err
	}
	rows, err := s.st.Summary()
	if err != nil {
		return nil, StatusOut{}, err
	}
	out := StatusOut{Archive: s.cfg.OutDir, Database: s.cfg.DBPath(), Messages: msgs, Chats: len(rows)}
	for i, c := range rows {
		if i == 10 {
			break
		}
		out.Recent = append(out.Recent, ChatOut{ID: c.ID, Kind: c.Kind, Title: c.Title,
			Messages: c.Count, LastSeen: s.local(c.Last)})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "archive:  %s\ndatabase: %s\nmessages: %d\nchats:    %d\n\nmost recent:\n",
		out.Archive, out.Database, out.Messages, out.Chats)
	for _, c := range out.Recent {
		fmt.Fprintf(&b, "  %-40s %6d  %s\n", trunc(c.Title, 40), c.Messages, c.LastSeen)
	}
	return text(b.String()), out, nil
}

type SyncIn struct {
	Chat string `json:"chat" jsonschema:"chat id, @username, or part of the title"`
}

type SyncOut struct {
	ChatID  int64 `json:"chat_id"`
	Fetched int   `json:"fetched"`
}

func (s *Server) syncChat(ctx context.Context, _ *mcp.CallToolRequest, in SyncIn) (*mcp.CallToolResult, SyncOut, error) {
	chat, err := s.resolve(in.Chat)
	if err != nil {
		return nil, SyncOut{}, err
	}
	sess, err := s.connect(ctx)
	if err != nil {
		return nil, SyncOut{}, err
	}
	var n int
	err = sess.Do(ctx, func(ctx context.Context) error {
		n, err = s.client.SyncChat(ctx, chat.ID)
		return err
	})
	if err != nil {
		return nil, SyncOut{}, err
	}
	return text(fmt.Sprintf("%s: %d new message(s) archived", chat.Title, n)),
		SyncOut{ChatID: chat.ID, Fetched: n}, nil
}

type SendIn struct {
	Chat    string `json:"chat" jsonschema:"chat id, @username, or part of the title"`
	Text    string `json:"text" jsonschema:"message body to send"`
	ReplyTo int    `json:"reply_to,omitempty" jsonschema:"id of the message being replied to"`
}

type SendOut struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
}

func (s *Server) sendMessage(ctx context.Context, _ *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, SendOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, SendOut{}, fmt.Errorf("text is required")
	}
	chat, err := s.resolve(in.Chat)
	if err != nil {
		return nil, SendOut{}, err
	}
	sess, err := s.connect(ctx)
	if err != nil {
		return nil, SendOut{}, err
	}
	var id int
	err = sess.Do(ctx, func(ctx context.Context) error {
		id, err = s.client.SendTo(ctx, chat.ID, in.Text, in.ReplyTo)
		return err
	})
	if err != nil {
		return nil, SendOut{}, err
	}
	return text(fmt.Sprintf("sent to %s (message #%d)", chat.Title, id)),
		SendOut{ChatID: chat.ID, MessageID: id}, nil
}

type MediaIn struct {
	Chat  string `json:"chat,omitempty" jsonschema:"only this chat; omit for all"`
	Limit int    `json:"limit,omitempty" jsonschema:"max files in this pass (default 100)"`
}

type MediaOut struct {
	Downloaded int `json:"downloaded"`
	Skipped    int `json:"skipped"`
}

func (s *Server) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in MediaIn) (*mcp.CallToolResult, MediaOut, error) {
	var chatID int64
	if in.Chat != "" {
		c, err := s.resolve(in.Chat)
		if err != nil {
			return nil, MediaOut{}, err
		}
		chatID = c.ID
	}
	sess, err := s.connect(ctx)
	if err != nil {
		return nil, MediaOut{}, err
	}
	var got, skipped int
	err = sess.Do(ctx, func(ctx context.Context) error {
		got, skipped, err = s.client.DownloadMedia(ctx, chatID, clamp(in.Limit, 100, 1000))
		return err
	})
	if err != nil {
		return nil, MediaOut{}, err
	}
	return text(fmt.Sprintf("downloaded %d file(s), skipped %d", got, skipped)),
		MediaOut{Downloaded: got, Skipped: skipped}, nil
}

type CheckOut struct {
	Unfinished []ChatOut `json:"unfinished_chats"`
	Gaps       []GapOut  `json:"gaps"`
}

type GapOut struct {
	ChatID   int64  `json:"chat_id"`
	Chat     string `json:"chat"`
	AfterID  int    `json:"after_id"`
	BeforeID int    `json:"before_id"`
	Missing  int    `json:"missing"`
}

func (s *Server) checkArchive(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, CheckOut, error) {
	unfinished, err := s.st.Unfinished()
	if err != nil {
		return nil, CheckOut{}, err
	}
	gaps, err := s.st.Gaps(50)
	if err != nil {
		return nil, CheckOut{}, err
	}
	var out CheckOut
	var b strings.Builder
	for _, c := range unfinished {
		out.Unfinished = append(out.Unfinished, ChatOut{ID: c.ID, Kind: c.Kind, Title: c.Title, Messages: c.Count})
	}
	fmt.Fprintf(&b, "%d chat(s) not walked back to the beginning\n", len(unfinished))
	for i, g := range gaps {
		if i < 20 {
			fmt.Fprintf(&b, "  %s: #%d → #%d (~%d missing)\n", g.Title, g.AfterID, g.BeforeID, g.Missing)
		}
		out.Gaps = append(out.Gaps, GapOut{ChatID: g.ChatID, Chat: g.Title,
			AfterID: g.AfterID, BeforeID: g.BeforeID, Missing: g.Missing})
	}
	fmt.Fprintf(&b, "%d gap(s) in supergroups/channels. Private chats cannot be checked this "+
		"way: Telegram numbers their messages per account, so id jumps there are normal.\n", len(gaps))
	return text(b.String()), out, nil
}

// ---------------------------------------------------------------- helpers

// connect opens the Telegram connection on first need and reuses it afterwards.
func (s *Server) connect(ctx context.Context) (*tgclient.Session, error) {
	if s.sess != nil {
		return s.sess, nil
	}
	sess, err := s.client.Serve(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	s.sess = sess
	return sess, nil
}

// resolve turns an id, @username or title fragment into exactly one chat.
func (s *Server) resolve(q string) (store.Chat, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return store.Chat{}, fmt.Errorf("chat is required")
	}
	if id, err := strconv.ParseInt(q, 10, 64); err == nil {
		return s.st.Chat(id)
	}
	found, err := s.st.SearchChats(strings.TrimPrefix(q, "@"))
	if err != nil {
		return store.Chat{}, err
	}
	switch len(found) {
	case 0:
		return store.Chat{}, fmt.Errorf("no chat matches %q — try list_chats", q)
	case 1:
		return found[0], nil
	}
	var b strings.Builder
	for _, c := range found {
		fmt.Fprintf(&b, "\n  %d  %s", c.ID, c.Title)
	}
	return store.Chat{}, fmt.Errorf("%q matches %d chats, pass an id:%s", q, len(found), b.String())
}

func (s *Server) msgOut(m store.Message) MessageOut {
	return MessageOut{
		ID: m.ID, Date: s.local(m.Date), Sender: m.Sender, Mine: m.Out, Text: m.Text,
		Media: m.Media, ReplyTo: m.ReplyTo, Fwd: m.Fwd, Edited: m.Edited != "", Deleted: m.Deleted,
		Reactions: m.Reactions, File: m.File,
	}
}

func (s *Server) time(iso string) time.Time {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return time.Time{}
	}
	return t.In(s.cfg.Location())
}

func (s *Server) local(iso string) string {
	if iso == "" {
		return ""
	}
	return s.time(iso).Format("2006-01-02 15:04")
}

func describeBody(m store.Message) string {
	var parts []string
	if m.Fwd != "" {
		parts = append(parts, "fwd from "+m.Fwd)
	}
	if m.ReplyTo != 0 {
		parts = append(parts, fmt.Sprintf("re #%d", m.ReplyTo))
	}
	if m.Media != "" {
		parts = append(parts, "["+m.Media+"]")
	}
	if m.Reactions != "" {
		parts = append(parts, m.Reactions)
	}
	if m.Edited != "" {
		parts = append(parts, "(edited)")
	}
	if m.Deleted {
		parts = append(parts, "(DELETED)")
	}
	head := ""
	if len(parts) > 0 {
		head = " " + strings.Join(parts, " ")
	}
	if m.Text == "" {
		return head
	}
	return head + ": " + strings.ReplaceAll(m.Text, "\n", "\n    ")
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// checkDates rejects a malformed date early, with a message that says what was expected.
func checkDates(dates ...string) error {
	for _, d := range dates {
		if d == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return fmt.Errorf("date %q must look like 2026-08-19", d)
		}
	}
	return nil
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
