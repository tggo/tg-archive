// Package tgclient — усе, що говорить з Telegram: авторизація, backfill, live, надсилання.
package tgclient

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"golang.org/x/time/rate"

	"github.com/tggo/tg-archive/internal/config"
	"github.com/tggo/tg-archive/internal/render"
	"github.com/tggo/tg-archive/internal/store"
)

type Client struct {
	cfg *config.Config
	st  *store.Store
	rd  *render.Renderer

	tg     *telegram.Client
	api    *tg.Client
	selfID int64
	waiter *floodwait.Waiter

	updates *updatesEngine
}

func New(cfg *config.Config, st *store.Store) *Client {
	c := &Client{cfg: cfg, st: st, rd: render.New(st, cfg.OutDir, cfg.Location())}
	// floodwait сам пересиджує FLOOD_WAIT; ratelimit тримає темп нижче межі Telegram.
	c.waiter = floodwait.NewWaiter().WithMaxRetries(6).WithMaxWait(10 * time.Minute)
	opts := telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: cfg.SessionPath()},
		Middlewares: []telegram.Middleware{
			c.waiter,
			ratelimit.New(rate.Every(100*time.Millisecond), 5),
		},
	}
	c.updates = newUpdates(c)
	opts.UpdateHandler = c.updates.handler()
	c.tg = telegram.NewClient(cfg.APIID, cfg.APIHash, opts)
	return c
}

// Run піднімає з'єднання і викликає fn; floodwait крутиться поруч.
func (c *Client) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.waiter.Run(ctx, func(ctx context.Context) error {
		return c.tg.Run(ctx, func(ctx context.Context) error {
			c.api = c.tg.API()
			return fn(ctx)
		})
	})
}

// Authed виконує fn лише після успішної авторизації.
func (c *Client) Authed(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.Run(ctx, func(ctx context.Context) error {
		status, err := c.tg.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized {
			return fmt.Errorf("не авторизовано — спершу `tg-archive login`")
		}
		self, err := c.tg.Self(ctx)
		if err != nil {
			return err
		}
		c.selfID = self.ID
		if err := c.st.SavePeer(self.ID, "user", self.AccessHash, self.Username); err != nil {
			return err
		}
		return fn(ctx)
	})
}

func (c *Client) SelfID() int64 { return c.selfID }

// Login проводить інтерактивну авторизацію (телефон → код → 2FA).
func (c *Client) Login(ctx context.Context, phone string) error {
	return c.Run(ctx, func(ctx context.Context) error {
		status, err := c.tg.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if status.Authorized {
			self, err := c.tg.Self(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("Уже авторизовано: %s (id %d)\n", userName(self), self.ID)
			return nil
		}
		flow := auth.NewFlow(termAuth{phone: phone}, auth.SendCodeOptions{})
		if err := c.tg.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}
		self, err := c.tg.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Авторизовано: %s (id %d)\n", userName(self), self.ID)
		return nil
	})
}

// dialog — один чат, який ми архівуємо.
type dialog struct {
	id    int64
	kind  string
	title string
	peer  tg.InputPeerClass
}

// Dialogs проходить усі діалоги, зберігає їх у базу і повертає ті, що підпадають під конфіг.
func (c *Client) Dialogs(ctx context.Context) ([]dialog, error) {
	var out []dialog
	iter := query.GetDialogs(c.api).BatchSize(100).Iter()
	for iter.Next(ctx) {
		e := iter.Value()
		id := markedFromInput(e.Peer)
		kind, title, username, accessHash := c.describePeer(e, id)
		if kind == "" {
			continue
		}
		if id == 0 {
			continue
		}
		if err := c.st.SavePeer(id, peerType(e.Peer), accessHash, username); err != nil {
			return nil, err
		}
		if !c.cfg.Allowed(id, kind) {
			continue
		}
		if err := c.st.UpsertChat(store.Chat{
			ID: id, Kind: kind, Title: title, Username: username, Slug: slugify(title, id),
		}); err != nil {
			return nil, err
		}
		out = append(out, dialog{id: id, kind: kind, title: title, peer: e.Peer})
	}
	return out, iter.Err()
}

// describePeer витягує тип/назву/access_hash із сутностей, доданих до діалогу.
func (c *Client) describePeer(e dialogs.Elem, id int64) (kind, title, username string, accessHash int64) {
	switch p := e.Peer.(type) {
	case *tg.InputPeerUser:
		u, ok := e.Entities.User(p.UserID)
		if !ok {
			return "", "", "", 0
		}
		un, _ := u.GetUsername()
		if u.ID == c.selfID {
			return "saved", "Saved Messages", un, u.AccessHash
		}
		if u.Bot {
			return "bot", userName(u), un, u.AccessHash
		}
		return "private", userName(u), un, u.AccessHash
	case *tg.InputPeerChat:
		ch, ok := e.Entities.Chat(p.ChatID)
		if !ok {
			return "", "", "", 0
		}
		return "group", ch.Title, "", 0
	case *tg.InputPeerChannel:
		ch, ok := e.Entities.Channel(p.ChannelID)
		if !ok {
			return "", "", "", 0
		}
		un, _ := ch.GetUsername()
		if ch.Megagroup || ch.Gigagroup {
			return "group", ch.Title, un, ch.AccessHash
		}
		return "channel", ch.Title, un, ch.AccessHash
	case *tg.InputPeerSelf:
		return "saved", "Saved Messages", "", 0
	}
	return "", "", "", 0
}

func peerType(p tg.InputPeerClass) string {
	switch p.(type) {
	case *tg.InputPeerUser, *tg.InputPeerSelf:
		return "user"
	case *tg.InputPeerChat:
		return "chat"
	case *tg.InputPeerChannel:
		return "channel"
	}
	return "user"
}

// inputPeer відновлює InputPeer із збереженого access_hash.
func (c *Client) inputPeer(id int64) (tg.InputPeerClass, error) {
	p, ok, err := c.st.Peer(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("невідомий чат %d — запусти `tg-archive chats`, щоб оновити список", id)
	}
	switch p.Type {
	case "user":
		if id == c.selfID {
			return &tg.InputPeerSelf{}, nil
		}
		return &tg.InputPeerUser{UserID: id, AccessHash: p.AccessHash}, nil
	case "chat":
		return &tg.InputPeerChat{ChatID: -id}, nil
	case "channel":
		return &tg.InputPeerChannel{ChannelID: -id - 1000000000000, AccessHash: p.AccessHash}, nil
	}
	return nil, fmt.Errorf("невідомий тип peer %q", p.Type)
}

// termAuth питає телефон/код/пароль у терміналі.
type termAuth struct{ phone string }

func (a termAuth) Phone(_ context.Context) (string, error) {
	if a.phone != "" {
		return a.phone, nil
	}
	return prompt("Номер телефону (+380…): ")
}

func (a termAuth) Password(_ context.Context) (string, error) {
	return promptSecret("Хмарний пароль (2FA): ")
}

func (a termAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	return prompt("Код із Telegram: ")
}

func (a termAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Println(tos.Text)
	return nil
}

func (a termAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("реєстрація нового акаунта не підтримується")
}

func prompt(label string) (string, error) {
	fmt.Print(label)
	var s string
	if _, err := fmt.Fscanln(os.Stdin, &s); err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
