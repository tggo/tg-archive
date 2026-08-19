package tgclient

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/tg"

	"github.com/tggo/tg-archive/internal/store"
)

type storeChat = store.Chat

type fetchOpts struct {
	OffsetID int // fetch messages older than this id
	MinID    int // stop once we reach this id (fetch only newer)
	Limit    int // 0 means no limit
}

// fetch pulls one chat's history into the database and returns how many messages landed.
func (c *Client) fetch(ctx context.Context, d dialog, opt fetchOpts) (int, error) {
	b := query.Messages(c.api).GetHistory(d.peer).BatchSize(100)
	if opt.OffsetID > 0 {
		b = b.OffsetID(opt.OffsetID)
	}
	iter := b.Iter()
	n := 0
	for iter.Next(ctx) {
		e := iter.Value()
		msg, ok := e.Msg.(*tg.Message)
		if !ok {
			continue
		}
		// GetHistory only understands offset_id (going back); "newer than X" is done by
		// stopping early, since history always arrives newest-first.
		if opt.MinID > 0 && msg.ID <= opt.MinID {
			break
		}
		row := describe(msg, d.id, e.Entities, c.selfID, c.cfg.Location())
		if err := c.st.SaveMessage(row); err != nil {
			return n, err
		}
		if err := c.st.BumpState(d.id, msg.ID); err != nil {
			return n, err
		}
		n++
		if opt.Limit > 0 && n >= opt.Limit {
			break
		}
		if n%500 == 0 {
			fmt.Printf("  %s: %d…\n", d.title, n)
		}
	}
	return n, iter.Err()
}

// Backfill walks every allowed chat from the oldest message it already has, further back.
// An interrupted run resumes from the same place.
func (c *Client) Backfill(ctx context.Context, limit int, force bool) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		dialogs, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Dialogs to archive: %d\n", len(dialogs))
		for _, d := range dialogs {
			st, err := c.st.GetState(d.id)
			if err != nil {
				return err
			}
			if st.BackfillDone && !force {
				continue
			}
			n, err := c.fetch(ctx, d, fetchOpts{OffsetID: st.MinID, Limit: limit})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", d.title, err)
			}
			if limit == 0 && err == nil {
				if err := c.st.SetBackfillDone(d.id); err != nil {
					return err
				}
			}
			fmt.Printf("✓ %s: +%d\n", d.title, n)
			if _, err := c.rd.Flush(); err != nil {
				return err
			}
		}
		return c.rd.Index()
	})
}

// Send posts a message and files it into the archive right away.
func (c *Client) Send(ctx context.Context, chatID int64, text string, replyTo int) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		p, err := c.inputPeer(chatID)
		if err != nil {
			return err
		}
		if _, err := c.sendOn(ctx, p, chatID, text, replyTo); err != nil {
			return err
		}
		fmt.Println("Sent.")
		return nil
	})
}

// sendOn does the actual send on an open connection and returns the new message id.
func (c *Client) sendOn(ctx context.Context, p tg.InputPeerClass, chatID int64, text string, replyTo int) (int, error) {
	req := &tg.MessagesSendMessageRequest{
		Peer:     p,
		Message:  text,
		RandomID: time.Now().UnixNano(),
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	upd, err := c.api.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, err
	}
	id := c.absorb(upd, chatID)
	if _, err := c.rd.Flush(); err != nil {
		return id, err
	}
	return id, nil
}

// absorb takes the just-sent message out of the server response and archives it,
// returning the new message id.
func (c *Client) absorb(upd tg.UpdatesClass, chatID int64) int {
	switch u := upd.(type) {
	case *tg.Updates:
		ents := peer.NewEntities(usersMap(u.Users), chatsMap(u.Chats), channelsMap(u.Chats))
		id := 0
		for _, up := range u.Updates {
			switch v := up.(type) {
			case *tg.UpdateNewMessage:
				id = c.absorbMsg(v.Message, chatID, ents)
			case *tg.UpdateNewChannelMessage:
				id = c.absorbMsg(v.Message, chatID, ents)
			}
		}
		return id
	case *tg.UpdateShortSentMessage:
		// Telegram economises here: no full Message comes back, so we assemble one.
		row := store.Message{
			ChatID: chatID, ID: u.ID,
			Date:  time.Unix(int64(u.Date), 0).UTC().Format(time.RFC3339),
			Month: time.Unix(int64(u.Date), 0).In(c.cfg.Location()).Format("2006-01"),
			Out:   true, SenderID: c.selfID, Sender: "me",
		}
		_ = c.st.SaveMessage(row)
		_ = c.st.BumpState(chatID, u.ID)
		return u.ID
	}
	return 0
}

func (c *Client) absorbMsg(msg tg.MessageClass, chatID int64, ents peer.Entities) int {
	m, ok := msg.(*tg.Message)
	if !ok {
		return 0
	}
	id := markedID(m.PeerID)
	if id == 0 {
		id = chatID
	}
	_ = c.st.SaveMessage(describe(m, id, ents, c.selfID, c.cfg.Location()))
	_ = c.st.BumpState(id, m.ID)
	return m.ID
}

func usersMap(us []tg.UserClass) map[int64]*tg.User {
	out := map[int64]*tg.User{}
	for _, u := range us {
		if uu, ok := u.(*tg.User); ok {
			out[uu.ID] = uu
		}
	}
	return out
}

func chatsMap(cs []tg.ChatClass) map[int64]*tg.Chat {
	out := map[int64]*tg.Chat{}
	for _, c := range cs {
		if cc, ok := c.(*tg.Chat); ok {
			out[cc.ID] = cc
		}
	}
	return out
}

func channelsMap(cs []tg.ChatClass) map[int64]*tg.Channel {
	out := map[int64]*tg.Channel{}
	for _, c := range cs {
		if cc, ok := c.(*tg.Channel); ok {
			out[cc.ID] = cc
		}
	}
	return out
}

// DialogInfo is the public slice of a dialog for the CLI.
type DialogInfo struct {
	ID    int64
	Kind  string
	Title string
}

// WithDialogs refreshes the chat list in the database and hands it to the caller.
func (c *Client) WithDialogs(ctx context.Context, fn func([]DialogInfo) error) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		ds, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		out := make([]DialogInfo, 0, len(ds))
		for _, d := range ds {
			out = append(out, DialogInfo{ID: d.id, Kind: d.kind, Title: d.title})
		}
		return fn(out)
	})
}
