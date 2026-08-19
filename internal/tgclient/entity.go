package tgclient

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/tg"
	"github.com/tggo/tg-archive/internal/store"
)

// markedID — та сама схема, що в Telethon: user > 0, chat < 0, channel -100…,
// щоб бази обох реалізацій були взаємозамінні.
func markedID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return -v.ChatID
	case *tg.PeerChannel:
		return -(1000000000000 + v.ChannelID)
	}
	return 0
}

func markedFromInput(p tg.InputPeerClass) int64 {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		return v.UserID
	case *tg.InputPeerSelf:
		return 0 // підставляється викликачем
	case *tg.InputPeerChat:
		return -v.ChatID
	case *tg.InputPeerChannel:
		return -(1000000000000 + v.ChannelID)
	}
	return 0
}

var transliteration = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e", 'є': "ie",
	'ж': "zh", 'з': "z", 'и': "y", 'і': "i", 'ї': "i", 'й': "i", 'к': "k", 'л': "l",
	'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch", 'ь': "",
	'ю': "iu", 'я': "ia", 'ы': "y", 'э': "e", 'ё': "e", 'ъ': "",
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify робить ASCII-ім'я теки: кирилиця транслітерується, id тримає унікальність.
func slugify(name string, id int64) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if t, ok := transliteration[r]; ok {
			b.WriteString(t)
			continue
		}
		if r < unicode.MaxASCII {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "chat"
	}
	neg := id
	if neg < 0 {
		neg = -neg
	}
	return fmt.Sprintf("%s-%d", strings.Trim(s, "-"), neg)
}

func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	if un, ok := u.GetUsername(); ok && un != "" {
		return "@" + un
	}
	return fmt.Sprint(u.ID)
}

// peerName — людське ім'я за peer, наскільки дозволяють підвантажені сутності.
func peerName(ents peer.Entities, p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerUser:
		if u, ok := ents.User(v.UserID); ok {
			return userName(u)
		}
		return fmt.Sprint(v.UserID)
	case *tg.PeerChat:
		if c, ok := ents.Chat(v.ChatID); ok {
			return c.Title
		}
	case *tg.PeerChannel:
		if c, ok := ents.Channel(v.ChannelID); ok {
			return c.Title
		}
	}
	return "?"
}

// describe перетворює tg.Message на рядок бази (без медіа-файлів — тільки маркер).
func describe(m *tg.Message, chatID int64, ents peer.Entities, selfID int64, loc *time.Location) store.Message {
	date := time.Unix(int64(m.Date), 0).UTC()
	row := store.Message{
		ChatID:  chatID,
		ID:      m.ID,
		Date:    date.Format(time.RFC3339),
		Month:   date.In(loc).Format("2006-01"),
		Out:     m.Out,
		Text:    m.Message,
		ReplyTo: replyTo(m),
		Media:   mediaDesc(m),
	}
	if from, ok := m.GetFromID(); ok {
		row.SenderID = markedID(from)
		row.Sender = peerName(ents, from)
	} else {
		// у приватних чатах from_id часто відсутній: автор — або я, або співрозмовник
		if m.Out {
			row.SenderID = selfID
			row.Sender = peerName(ents, &tg.PeerUser{UserID: selfID})
		} else {
			row.SenderID = chatID
			row.Sender = peerName(ents, m.PeerID)
		}
	}
	if ed, ok := m.GetEditDate(); ok && ed > 0 {
		row.Edited = time.Unix(int64(ed), 0).UTC().Format(time.RFC3339)
	}
	if fwd, ok := m.GetFwdFrom(); ok {
		if from, ok := fwd.GetFromID(); ok {
			row.Fwd = peerName(ents, from)
		} else if n, ok := fwd.GetFromName(); ok {
			row.Fwd = n
		} else {
			row.Fwd = "unknown"
		}
	}
	return row
}

func replyTo(m *tg.Message) int {
	if r, ok := m.GetReplyTo(); ok {
		if h, ok := r.(*tg.MessageReplyHeader); ok {
			if id, ok := h.GetReplyToMsgID(); ok {
				return id
			}
		}
	}
	return 0
}

func mediaDesc(m *tg.Message) string {
	media, ok := m.GetMedia()
	if !ok {
		return ""
	}
	switch v := media.(type) {
	case *tg.MessageMediaWebPage:
		return "" // прев'ю посилання — саме посилання вже є в тексті
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive:
		return "location"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaPoll:
		return "poll"
	case *tg.MessageMediaDice:
		return "dice"
	case *tg.MessageMediaDocument:
		doc, ok := v.Document.AsNotEmpty()
		if !ok {
			return "file"
		}
		return documentDesc(doc)
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", media), "*tg.MessageMedia")
}

func documentDesc(doc *tg.Document) string {
	var name string
	var duration float64
	voice, video, sticker, animated := false, false, false, false
	for _, a := range doc.Attributes {
		switch at := a.(type) {
		case *tg.DocumentAttributeFilename:
			name = at.FileName
		case *tg.DocumentAttributeAudio:
			duration = float64(at.Duration)
			voice = at.Voice
		case *tg.DocumentAttributeVideo:
			duration = at.Duration
			video = true
		case *tg.DocumentAttributeSticker:
			sticker = true
			name = at.Alt
		case *tg.DocumentAttributeAnimated:
			animated = true
		}
	}
	switch {
	case voice:
		return fmt.Sprintf("voice %.0fs", duration)
	case sticker:
		return strings.TrimSpace("sticker " + name)
	case animated:
		return "gif"
	case video:
		return fmt.Sprintf("video %.0fs %s", duration, humanSize(doc.Size))
	case name != "":
		return fmt.Sprintf("file %s %s", name, humanSize(doc.Size))
	}
	return "file " + humanSize(doc.Size)
}

func humanSize(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 || u == "GB" {
			if u == "B" {
				return fmt.Sprintf("%.0fB", f)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return ""
}
