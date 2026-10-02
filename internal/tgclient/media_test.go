package tgclient

import (
	"strings"
	"testing"

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
