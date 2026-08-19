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
	OffsetID int // качати старіші за цей id
	MinID    int // качати новіші за цей id
	Limit    int // 0 = без обмеження
}

// fetch тягне історію одного чату й пише її в базу. Повертає кількість повідомлень.
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
		// GetHistory уміє тільки offset_id (углиб); «новіші за X» робимо зупинкою ітерації,
		// бо історія завжди йде від найновішого до найстарішого.
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

// Backfill проходить усі дозволені чати від найстарішого завантаженого вглиб історії.
// Перерваний прохід продовжується з того ж місця.
func (c *Client) Backfill(ctx context.Context, limit int, force bool) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		dialogs, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Діалогів до архівації: %d\n", len(dialogs))
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

// Send надсилає повідомлення і одразу кладе його в архів.
func (c *Client) Send(ctx context.Context, chatID int64, text string, replyTo int) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		p, err := c.inputPeer(chatID)
		if err != nil {
			return err
		}
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
			return err
		}
		c.absorb(upd, chatID)
		if _, err := c.rd.Flush(); err != nil {
			return err
		}
		fmt.Println("Надіслано.")
		return nil
	})
}

// absorb дістає з відповіді сервера щойно надіслане повідомлення й архівує його.
func (c *Client) absorb(upd tg.UpdatesClass, chatID int64) {
	switch u := upd.(type) {
	case *tg.Updates:
		ents := peer.NewEntities(usersMap(u.Users), chatsMap(u.Chats), channelsMap(u.Chats))
		for _, up := range u.Updates {
			switch v := up.(type) {
			case *tg.UpdateNewMessage:
				c.absorbMsg(v.Message, chatID, ents)
			case *tg.UpdateNewChannelMessage:
				c.absorbMsg(v.Message, chatID, ents)
			}
		}
	case *tg.UpdateShortSentMessage:
		// Telegram economises: повний Message не приходить, збираємо його самі.
		row := store.Message{
			ChatID: chatID, ID: u.ID,
			Date:  time.Unix(int64(u.Date), 0).UTC().Format(time.RFC3339),
			Month: time.Unix(int64(u.Date), 0).In(c.cfg.Location()).Format("2006-01"),
			Out:   true, SenderID: c.selfID, Sender: "я",
		}
		_ = c.st.SaveMessage(row)
		_ = c.st.BumpState(chatID, u.ID)
	}
}

func (c *Client) absorbMsg(msg tg.MessageClass, chatID int64, ents peer.Entities) {
	m, ok := msg.(*tg.Message)
	if !ok {
		return
	}
	id := markedID(m.PeerID)
	if id == 0 {
		id = chatID
	}
	_ = c.st.SaveMessage(describe(m, id, ents, c.selfID, c.cfg.Location()))
	_ = c.st.BumpState(id, m.ID)
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

// DialogInfo — публічний зріз діалогу для CLI.
type DialogInfo struct {
	ID    int64
	Kind  string
	Title string
}

// WithDialogs оновлює список чатів у базі й віддає його викликачу.
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
