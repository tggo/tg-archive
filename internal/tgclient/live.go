package tgclient

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

const (
	flushDebounce = 3 * time.Second  // quiet period before writing .md
	resyncEvery   = 10 * time.Minute // backstop for updates that never arrive
)

// updatesEngine turns incoming updates into database rows.
type updatesEngine struct {
	c    *Client
	gaps *updates.Manager
	disp tg.UpdateDispatcher

	mu      sync.Mutex
	pending chan struct{}
}

func newUpdates(c *Client) *updatesEngine {
	u := &updatesEngine{c: c, pending: make(chan struct{}, 1)}
	u.disp = tg.NewUpdateDispatcher()
	u.disp.OnNewMessage(u.onNew)
	u.disp.OnNewChannelMessage(u.onNewChannel)
	u.disp.OnEditMessage(u.onEdit)
	u.disp.OnEditChannelMessage(u.onEditChannel)
	u.disp.OnDeleteMessages(u.onDelete)
	u.disp.OnDeleteChannelMessages(u.onDeleteChannel)
	u.gaps = updates.New(updates.Config{Handler: u.disp})
	return u
}

func (u *updatesEngine) handler() telegram.UpdateHandler { return u.gaps }

func (u *updatesEngine) touch() {
	select {
	case u.pending <- struct{}{}:
	default: // already flagged, a second signal adds nothing
	}
}

func (u *updatesEngine) store(ents peer.Entities, msg tg.MessageClass) {
	m, ok := msg.(*tg.Message)
	if !ok {
		return // service messages (MessageService) are not archived
	}
	chatID := markedID(m.PeerID)
	if chatID == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	// the chat may be new: work out its kind and whether we want it at all
	if _, err := u.c.st.Chat(chatID); err != nil {
		kind, title, username := chatFromEntities(ents, m.PeerID, u.c.selfID)
		if kind == "" || !u.c.cfg.Allowed(chatID, kind) {
			return
		}
		if err := u.c.st.UpsertChat(store_Chat(chatID, kind, title, username)); err != nil {
			fmt.Fprintln(os.Stderr, "chat:", err)
			return
		}
	}
	row := describe(m, chatID, ents, u.c.selfID, u.c.cfg.Location())
	if err := u.c.st.SaveMessage(row); err != nil {
		fmt.Fprintln(os.Stderr, "save:", err)
		return
	}
	if err := u.c.st.BumpState(chatID, m.ID); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
	}
	u.touch()
}

func (u *updatesEngine) onNew(ctx context.Context, e tg.Entities, up *tg.UpdateNewMessage) error {
	u.store(peer.EntitiesFromUpdate(e), up.Message)
	return nil
}

func (u *updatesEngine) onNewChannel(ctx context.Context, e tg.Entities, up *tg.UpdateNewChannelMessage) error {
	u.store(peer.EntitiesFromUpdate(e), up.Message)
	return nil
}

func (u *updatesEngine) onEdit(ctx context.Context, e tg.Entities, up *tg.UpdateEditMessage) error {
	u.store(peer.EntitiesFromUpdate(e), up.Message)
	return nil
}

func (u *updatesEngine) onEditChannel(ctx context.Context, e tg.Entities, up *tg.UpdateEditChannelMessage) error {
	u.store(peer.EntitiesFromUpdate(e), up.Message)
	return nil
}

// onDelete: in private chats the update does not name the chat, so we look it up by id.
func (u *updatesEngine) onDelete(ctx context.Context, e tg.Entities, up *tg.UpdateDeleteMessages) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, id := range up.Messages {
		chatID, ok, err := u.c.st.FindChatByMessage(id)
		if err != nil || !ok {
			continue
		}
		if err := u.c.st.MarkDeleted(chatID, id); err != nil {
			fmt.Fprintln(os.Stderr, "delete:", err)
		}
	}
	u.touch()
	return nil
}

func (u *updatesEngine) onDeleteChannel(ctx context.Context, e tg.Entities, up *tg.UpdateDeleteChannelMessages) error {
	chatID := -(1000000000000 + up.ChannelID)
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, id := range up.Messages {
		if err := u.c.st.MarkDeleted(chatID, id); err != nil {
			fmt.Fprintln(os.Stderr, "delete:", err)
		}
	}
	u.touch()
	return nil
}

func store_Chat(id int64, kind, title, username string) storeChat {
	return storeChat{ID: id, Kind: kind, Title: title, Username: username, Slug: slugify(title, id)}
}

func chatFromEntities(ents peer.Entities, p tg.PeerClass, selfID int64) (kind, title, username string) {
	switch v := p.(type) {
	case *tg.PeerUser:
		u, ok := ents.User(v.UserID)
		if !ok {
			return "", "", ""
		}
		un, _ := u.GetUsername()
		switch {
		case u.ID == selfID:
			return "saved", "Saved Messages", un
		case u.Bot:
			return "bot", userName(u), un
		}
		return "private", userName(u), un
	case *tg.PeerChat:
		if c, ok := ents.Chat(v.ChatID); ok {
			return "group", c.Title, ""
		}
	case *tg.PeerChannel:
		if c, ok := ents.Channel(v.ChannelID); ok {
			un, _ := c.GetUsername()
			if c.Megagroup || c.Gigagroup {
				return "group", c.Title, un
			}
			return "channel", c.Title, un
		}
	}
	return "", "", ""
}

// Live holds the connection, writes changes to .md, and periodically picks up what it missed.
func (c *Client) Live(ctx context.Context) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		dialogs, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		self, err := c.tg.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("live: %s · chats watched: %d\n", userName(self), len(dialogs))

		go c.writer(ctx)
		go c.resyncLoop(ctx, dialogs)

		return c.updates.gaps.Run(ctx, c.api, self.ID, updates.AuthOptions{
			OnStart: func(ctx context.Context) {},
		})
	})
}

// writer batches bursts of updates so .md is redrawn once, not per message.
func (c *Client) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.updates.pending:
			select {
			case <-ctx.Done():
				return
			case <-time.After(flushDebounce):
			}
			n, err := c.rd.Flush()
			if err != nil {
				fmt.Fprintln(os.Stderr, "render:", err)
				continue
			}
			if n > 0 {
				fmt.Printf("%s files written: %d\n", time.Now().Format("15:04:05"), n)
			}
		}
	}
}

// resyncLoop pulls anything newer than max_id, in case an update was lost.
func (c *Client) resyncLoop(ctx context.Context, dialogs []dialog) {
	t := time.NewTicker(resyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, d := range dialogs {
				st, err := c.st.GetState(d.id)
				if err != nil || st.MaxID == 0 {
					continue
				}
				if _, err := c.fetch(ctx, d, fetchOpts{MinID: st.MaxID}); err != nil {
					fmt.Fprintln(os.Stderr, "resync:", d.title, err)
				}
			}
			if _, err := c.rd.Flush(); err != nil {
				fmt.Fprintln(os.Stderr, "render:", err)
			}
		}
	}
}

func promptSecret(label string) (string, error) {
	fmt.Print(label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(b), err
}
