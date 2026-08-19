package tgclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"

	"github.com/tggo/tg-archive/internal/store"
)

// mediaLocation picks what to download and what to call it. It returns ok=false for media
// that is not a file (polls, locations, link previews) or that the size policy excludes.
func mediaLocation(m *tg.Message, maxBytes int64) (loc tg.InputFileLocationClass, name string, ok bool) {
	media, has := m.GetMedia()
	if !has {
		return nil, "", false
	}
	switch v := media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := v.Photo.AsNotEmpty()
		if !ok {
			return nil, "", false
		}
		size, bytes := largestPhotoSize(photo)
		if size == "" || (maxBytes > 0 && bytes > maxBytes) {
			return nil, "", false
		}
		return &tg.InputPhotoFileLocation{
			ID: photo.ID, AccessHash: photo.AccessHash,
			FileReference: photo.FileReference, ThumbSize: size,
		}, fmt.Sprintf("%d.jpg", m.ID), true

	case *tg.MessageMediaDocument:
		doc, ok := v.Document.AsNotEmpty()
		if !ok {
			return nil, "", false
		}
		if maxBytes > 0 && doc.Size > maxBytes {
			return nil, "", false
		}
		return &tg.InputDocumentFileLocation{
			ID: doc.ID, AccessHash: doc.AccessHash,
			FileReference: doc.FileReference,
		}, documentName(doc, m.ID), true
	}
	return nil, "", false
}

// largestPhotoSize returns the biggest available rendition and its byte size.
func largestPhotoSize(p *tg.Photo) (string, int64) {
	best, bestBytes := "", int64(0)
	for _, s := range p.Sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if int64(v.Size) > bestBytes {
				best, bestBytes = v.Type, int64(v.Size)
			}
		case *tg.PhotoSizeProgressive:
			if len(v.Sizes) > 0 {
				if n := int64(v.Sizes[len(v.Sizes)-1]); n > bestBytes {
					best, bestBytes = v.Type, n
				}
			}
		}
	}
	return best, bestBytes
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

func documentName(doc *tg.Document, msgID int) string {
	name, ext := "", ""
	for _, a := range doc.Attributes {
		switch at := a.(type) {
		case *tg.DocumentAttributeFilename:
			name = at.FileName
		case *tg.DocumentAttributeAudio:
			if at.Voice {
				ext = ".ogg"
			}
		case *tg.DocumentAttributeVideo:
			ext = ".mp4"
		case *tg.DocumentAttributeSticker:
			ext = ".webp"
		}
	}
	if name == "" {
		if ext == "" {
			ext = extFromMime(doc.MimeType)
		}
		return fmt.Sprintf("%d%s", msgID, ext)
	}
	name = unsafeName.ReplaceAllString(name, "_")
	if len(name) > 80 {
		name = name[len(name)-80:]
	}
	// The message id keeps two files of the same name from colliding.
	return fmt.Sprintf("%d-%s", msgID, name)
}

func extFromMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "." + strings.TrimPrefix(mime, "image/")
	case strings.HasPrefix(mime, "video/"):
		return ".mp4"
	case strings.HasPrefix(mime, "audio/"):
		return ".ogg"
	}
	return ".bin"
}

// DownloadMedia fetches attachments that the archive has a record of but no file for.
// It is deliberately a separate pass: history first (cheap, text), files later (expensive),
// so an interrupted download never costs you the messages.
func (c *Client) DownloadMedia(ctx context.Context, chatID int64, limit int) (got int, skipped int, err error) {
	maxBytes := c.cfg.MediaMaxBytes()
	if maxBytes < 0 {
		return 0, 0, fmt.Errorf(`media downloading is off — set "media" to "small" or "all" in the config`)
	}
	pending, err := c.st.PendingMedia(chatID, limit)
	if err != nil {
		return 0, 0, err
	}
	d := downloader.NewDownloader()
	for _, row := range pending {
		msg, err := c.fetchMessage(ctx, row.ChatID, row.ID)
		if err != nil || msg == nil {
			skipped++
			continue
		}
		loc, name, ok := mediaLocation(msg, maxBytes)
		if !ok {
			skipped++
			continue
		}
		chat, err := c.st.Chat(row.ChatID)
		if err != nil {
			return got, skipped, err
		}
		rel := filepath.Join("attachments", chat.Slug, name)
		abs := filepath.Join(c.cfg.OutDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return got, skipped, err
		}
		if _, err := os.Stat(abs); err == nil { // already on disk from an earlier run
			if err := c.st.SetFile(row.ChatID, row.ID, rel); err != nil {
				return got, skipped, err
			}
			got++
			continue
		}
		if _, err := d.Download(c.api, loc).ToPath(ctx, abs); err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s #%d: %v\n", chat.Title, row.ID, err)
			os.Remove(abs)
			skipped++
			continue
		}
		if err := c.st.SetFile(row.ChatID, row.ID, rel); err != nil {
			return got, skipped, err
		}
		got++
		if got%25 == 0 {
			fmt.Printf("  %d files…\n", got)
			if _, err := c.rd.Flush(); err != nil {
				return got, skipped, err
			}
		}
	}
	_, err = c.rd.Flush()
	return got, skipped, err
}

// fetchMessage re-reads one message, because downloading needs the file reference that
// only a fresh server response carries — stored rows hold text, not references.
func (c *Client) fetchMessage(ctx context.Context, chatID int64, msgID int) (*tg.Message, error) {
	p, err := c.inputPeer(chatID)
	if err != nil {
		return nil, err
	}
	var res tg.MessagesMessagesClass
	if ch, ok := p.(*tg.InputPeerChannel); ok {
		res, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
		})
	} else {
		res, err = c.api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}})
	}
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch v := res.(type) {
	case *tg.MessagesMessages:
		msgs = v.Messages
	case *tg.MessagesMessagesSlice:
		msgs = v.Messages
	case *tg.MessagesChannelMessages:
		msgs = v.Messages
	}
	for _, m := range msgs {
		if mm, ok := m.(*tg.Message); ok && mm.ID == msgID {
			return mm, nil
		}
	}
	return nil, nil
}

// FillGap fetches the messages missing between two archived ids.
func (c *Client) FillGap(ctx context.Context, g store.Gap) (int, error) {
	p, err := c.inputPeer(g.ChatID)
	if err != nil {
		return 0, err
	}
	return c.fetch(ctx, dialog{id: g.ChatID, title: g.Title, peer: p},
		fetchOpts{OffsetID: g.BeforeID, MinID: g.AfterID})
}

// DownloadMediaPass is the CLI entry point: connect, download, report.
func (c *Client) DownloadMediaPass(ctx context.Context, chatID int64, limit int) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		got, skipped, err := c.DownloadMedia(ctx, chatID, limit)
		fmt.Printf("downloaded %d file(s), skipped %d\n", got, skipped)
		if skipped > 0 {
			fmt.Println("(skipped ones are media with no file to fetch — polls, locations, " +
				"link previews — or files over the size limit)")
		}
		return err
	})
}

// FillGaps fetches the messages missing from each reported hole.
func (c *Client) FillGaps(ctx context.Context, gaps []store.Gap) error {
	return c.Authed(ctx, func(ctx context.Context) error {
		total := 0
		for _, g := range gaps {
			n, err := c.FillGap(ctx, g)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", g.Title, err)
				continue
			}
			total += n
			if n > 0 {
				fmt.Printf("  %s #%d→#%d: +%d\n", g.Title, g.AfterID, g.BeforeID, n)
			}
		}
		fmt.Printf("\nrecovered %d message(s)\n", total)
		if total == 0 {
			fmt.Println("Nothing came back, which usually means those ids belong to other " +
				"chats or to messages deleted long ago — Telegram numbers messages per " +
				"account, not per chat.")
		}
		_, err := c.rd.Flush()
		return err
	})
}
