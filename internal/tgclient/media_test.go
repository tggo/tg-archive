package tgclient

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// A sender controls the mime type, so it must never turn into a path.
func TestDocumentNameIgnoresHostileMime(t *testing.T) {
	for _, mime := range []string{"image/../../../evil", "image/a/b", "image/"} {
		name := documentName(&tg.Document{MimeType: mime}, 7)
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Errorf("mime %q gave unsafe name %q", mime, name)
		}
	}
	if got := documentName(&tg.Document{MimeType: "image/png"}, 7); got != "7.png" {
		t.Errorf("image/png gave %q, want 7.png", got)
	}
}

func TestDocumentNameTrimsByRune(t *testing.T) {
	long := strings.Repeat("Ж", 100) + ".pdf"
	doc := &tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: long}}}
	if name := documentName(doc, 1); !utf8.ValidString(name) {
		t.Errorf("trimmed name is not valid UTF-8: %q", name)
	}
}
