package tgclient

import (
	"context"
	"fmt"
)

// Session keeps one authorized connection open and runs jobs on it. The MCP server needs
// this: opening a fresh MTProto connection per tool call would cost seconds and burn
// Telegram's auth rate limits.
type Session struct {
	jobs chan job
	done chan struct{}
	err  error
}

type job struct {
	fn    func(context.Context) error
	reply chan error
}

// Serve dials Telegram, authorizes, and returns once the connection is ready to take jobs.
// The connection lives until ctx is cancelled.
func (c *Client) Serve(ctx context.Context) (*Session, error) {
	s := &Session{jobs: make(chan job), done: make(chan struct{})}
	ready := make(chan error, 1)

	go func() {
		defer close(s.done)
		s.err = c.Authed(ctx, func(ctx context.Context) error {
			ready <- nil
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case j := <-s.jobs:
					j.reply <- j.fn(ctx)
				}
			}
		})
		select {
		case ready <- s.err:
		default:
		}
	}()

	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Do runs fn on the live connection.
func (s *Session) Do(ctx context.Context, fn func(context.Context) error) error {
	reply := make(chan error, 1)
	select {
	case s.jobs <- job{fn: fn, reply: reply}:
	case <-s.done:
		return fmt.Errorf("telegram connection closed: %w", s.err)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SyncChat pulls anything newer than what the archive already has for one chat.
func (c *Client) SyncChat(ctx context.Context, chatID int64) (int, error) {
	p, err := c.inputPeer(chatID)
	if err != nil {
		return 0, err
	}
	chat, err := c.st.Chat(chatID)
	if err != nil {
		return 0, err
	}
	st, err := c.st.GetState(chatID)
	if err != nil {
		return 0, err
	}
	n, err := c.fetch(ctx, dialog{id: chatID, title: chat.Title, peer: p}, fetchOpts{MinID: st.MaxID})
	if err != nil {
		return n, err
	}
	if _, err := c.rd.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

// SendTo posts a message on an already-open connection.
func (c *Client) SendTo(ctx context.Context, chatID int64, text string, replyTo int) (int, error) {
	p, err := c.inputPeer(chatID)
	if err != nil {
		return 0, err
	}
	return c.sendOn(ctx, p, chatID, text, replyTo)
}
