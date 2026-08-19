package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resourceLimit caps how many chats are listed as resources. A client shows them in a
// picker, and 350 entries there is noise; the rest stay reachable through the template
// URI and the tools.
const resourceLimit = 40

// addResources exposes chats as MCP resources, so they can be attached by name in a client
// (typing @) instead of only through a tool call.
func (s *Server) addResources(srv *mcp.Server) error {
	rows, err := s.st.Summary()
	if err != nil {
		return err
	}
	for i, c := range rows {
		if i >= resourceLimit {
			break
		}
		srv.AddResource(&mcp.Resource{
			URI:      fmt.Sprintf("tg-archive://chat/%d", c.ID),
			Name:     c.Title,
			Title:    c.Title,
			MIMEType: "text/markdown",
			Description: fmt.Sprintf("%s chat, %d messages, last activity %s",
				c.Kind, c.Count, s.local(c.Last)),
		}, s.readResource)
	}

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "tg-archive://chat/{chat_id}",
		Name:        "Telegram chat",
		MIMEType:    "text/markdown",
		Description: "Recent messages of any archived chat, by id (see list_chats).",
	}, s.readResource)

	return nil
}

// readResource renders the tail of a chat as Markdown, the same shape the archive uses.
func (s *Server) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	idPart := strings.TrimPrefix(uri, "tg-archive://chat/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unknown resource %q", uri)
	}
	chat, err := s.st.Chat(id)
	if err != nil {
		return nil, fmt.Errorf("no archived chat with id %d", id)
	}
	msgs, err := s.st.Tail(id, 200)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_%s chat, id %d, showing the %d most recent messages_\n\n",
		chat.Title, chat.Kind, chat.ID, len(msgs))
	day := ""
	for _, m := range msgs {
		t := s.time(m.Date)
		if d := t.Format("2006-01-02"); d != day {
			day = d
			fmt.Fprintf(&b, "\n## %s\n\n", d)
		}
		fmt.Fprintf(&b, "**%s** · **%s**%s\n\n", t.Format("15:04"), m.Sender, describeBody(m))
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "text/markdown",
			Text:     b.String(),
		}},
	}, nil
}
