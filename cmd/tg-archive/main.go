// Command tg-archive archives your own Telegram into Markdown, near real-time.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tggo/tg-archive/internal/config"
	"github.com/tggo/tg-archive/internal/mcpserver"
	"github.com/tggo/tg-archive/internal/render"
	"github.com/tggo/tg-archive/internal/store"
	"github.com/tggo/tg-archive/internal/tgclient"
)

var version = "dev" // set by the linker at release time

const usage = `tg-archive %s — Markdown archive of your own Telegram

  tg-archive setup              wizard: api_id/api_hash, output dir, which chats
  tg-archive login [--phone N]  sign in (the code arrives inside Telegram)
  tg-archive chats              list dialogs and their ids
  tg-archive backfill [--limit N] [--force]
                                full history; interrupting it loses no progress
  tg-archive live               daemon: new/edited/deleted → .md within ~3s
  tg-archive send --chat X --text "..." [--reply-to N]
  tg-archive search "words" [--chat X] [--from D] [--to D]
                                full-text search over the archive
  tg-archive media [--chat X] [--limit N]
                                download attachments for messages that have none yet
  tg-archive doctor [--fix]     find holes in the archived history (and fill them)
  tg-archive rerender           rebuild every .md from the database
  tg-archive status             what the archive holds right now
  tg-archive mcp [--allow-send] serve the archive to an MCP client over stdio
                                (read-only unless --allow-send is passed)
  tg-archive import-telethon <StringSession>
                                carry over a Telethon session instead of logging in again
  tg-archive version

Global: --profile <name> before the command uses a separate account and archive.

Config: %s
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, version, config.Path())
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --profile comes before the command: tg-archive --profile work backfill
	if os.Args[1] == "--profile" || strings.HasPrefix(os.Args[1], "--profile=") {
		name, rest := "", os.Args[2:]
		if v, ok := strings.CutPrefix(os.Args[1], "--profile="); ok {
			name = v
		} else if len(os.Args) > 2 {
			name, rest = os.Args[2], os.Args[3:]
		}
		if name == "" || len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "usage: tg-archive --profile <name> <command>")
			os.Exit(2)
		}
		config.SetProfile(name)
		os.Args = append([]string{os.Args[0]}, rest...)
	}

	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup()
	case "login":
		err = cmdLogin(ctx)
	case "chats":
		err = cmdChats(ctx)
	case "backfill":
		err = cmdBackfill(ctx)
	case "live":
		err = cmdLive(ctx)
	case "send":
		err = cmdSend(ctx)
	case "rerender":
		err = cmdRerender()
	case "status":
		err = cmdStatus()
	case "doctor":
		err = cmdDoctor(ctx)
	case "media":
		err = cmdMedia(ctx)
	case "search":
		err = cmdSearch()
	case "mcp":
		err = cmdMCP(ctx)
	case "import-telethon":
		err = cmdImportTelethon()
	case "version", "--version", "-v":
		fmt.Println("tg-archive", version)
	default:
		fmt.Printf(usage, version, config.Path())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// open is the shared startup for commands that need both config and database.
func open() (*config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return cfg, st, nil
}

func cmdSetup() error {
	in := bufio.NewReader(os.Stdin)
	cfg := config.Default()
	if existing, err := config.Load(); err == nil {
		cfg = existing
		fmt.Println("Found an existing config — press Enter to keep the current value.")
	}

	fmt.Print(`
You need your own api_id / api_hash (Telegram issues them per account):
  1. https://my.telegram.org → sign in with your phone number
  2. API development tools → create an application (any name)
  3. copy api_id and api_hash

`)
	if v := ask(in, "api_id", fmt.Sprint(cfg.APIID)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("api_id must be a number: %w", err)
		}
		cfg.APIID = n
	}
	if v := ask(in, "api_hash", cfg.APIHash); v != "" {
		cfg.APIHash = v
	}
	if v := ask(in, "where to write the archive", cfg.OutDir); v != "" {
		cfg.OutDir = v
	}
	if v := ask(in, "timezone (e.g. Europe/Kyiv or Local)", cfg.Timezone); v != "" {
		cfg.Timezone = v
	}
	cfg.Private = askBool(in, "archive private chats", cfg.Private)
	cfg.Groups = askBool(in, "archive groups", cfg.Groups)
	cfg.Saved = askBool(in, "archive Saved Messages", cfg.Saved)
	cfg.Channels = askBool(in, "archive channels you follow", cfg.Channels)
	cfg.Bots = askBool(in, "archive bot chats", cfg.Bots)

	fmt.Print("\ndownload media? none = keep markers like [voice 12s] (smallest archive),\n" +
		"small = photos and voice notes, all = everything including video\n")
	if v := ask(in, "media (none/small/all)", cfg.Media); v != "" {
		switch v {
		case "none", "small", "all":
			cfg.Media = v
		default:
			return fmt.Errorf("media must be none, small or all")
		}
	}
	if cfg.Media == "small" {
		if v := ask(in, "size limit in MB", strconv.Itoa(cfg.MediaMaxMB)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("size limit must be a number: %w", err)
			}
			cfg.MediaMaxMB = n
		}
	}

	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("\nSaved: %s\nNext: tg-archive login\n", config.Path())
	return nil
}

func cmdLogin(ctx context.Context) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	phone := fs.String("phone", "", "phone number, e.g. +12025550123")
	_ = fs.Parse(os.Args[2:])

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	return tgclient.New(cfg, st).Login(ctx, *phone)
}

func cmdChats(ctx context.Context) error {
	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	c := tgclient.New(cfg, st)
	return c.WithDialogs(ctx, func(ds []tgclient.DialogInfo) error {
		for _, d := range ds {
			fmt.Printf("%16d  %-8s %s\n", d.ID, d.Kind, d.Title)
		}
		fmt.Printf("\ntotal: %d\n", len(ds))
		return nil
	})
}

func cmdBackfill(ctx context.Context) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max messages per chat in this pass (0 = no limit)")
	force := fs.Bool("force", false, "re-walk chats already marked complete")
	_ = fs.Parse(os.Args[2:])

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	return tgclient.New(cfg, st).Backfill(ctx, *limit, *force)
}

func cmdLive(ctx context.Context) error {
	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	return tgclient.New(cfg, st).Live(ctx)
}

func cmdSend(ctx context.Context) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	chat := fs.String("chat", "", "id, @username, or part of the chat title")
	text := fs.String("text", "", "message text")
	replyTo := fs.Int("reply-to", 0, "id of the message to reply to")
	_ = fs.Parse(os.Args[2:])
	if *chat == "" || *text == "" {
		return fmt.Errorf("--chat and --text are both required")
	}

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	id, err := resolveChat(st, *chat)
	if err != nil {
		return err
	}
	return tgclient.New(cfg, st).Send(ctx, id, *text, *replyTo)
}

// resolveChat accepts an id or a title fragment and insists on an unambiguous match.
func resolveChat(st *store.Store, q string) (int64, error) {
	if id, err := strconv.ParseInt(q, 10, 64); err == nil {
		return id, nil
	}
	found, err := st.SearchChats(strings.TrimPrefix(q, "@"))
	if err != nil {
		return 0, err
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("no chat matches %q — run `tg-archive chats` first", q)
	case 1:
		return found[0].ID, nil
	}
	fmt.Fprintln(os.Stderr, "ambiguous, be more specific:")
	for _, c := range found {
		fmt.Fprintf(os.Stderr, "  %16d  %s\n", c.ID, c.Title)
	}
	return 0, fmt.Errorf("%d chats matched", len(found))
}

func cmdDoctor(ctx context.Context) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fix := fs.Bool("fix", false, "fetch the missing messages")
	minJump := fs.Int("min-gap", 50, "ignore id jumps smaller than this")
	_ = fs.Parse(os.Args[2:])

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	unfinished, err := st.Unfinished()
	if err != nil {
		return err
	}
	if len(unfinished) > 0 {
		fmt.Printf("%d chat(s) never walked back to the beginning:\n", len(unfinished))
		for i, c := range unfinished {
			if i == 10 {
				fmt.Printf("  … and %d more\n", len(unfinished)-10)
				break
			}
			fmt.Printf("  %-40s %7d archived, oldest %s\n", trunc(c.Title, 40), c.Count, shortDate(c.Last))
		}
		fmt.Print("Run `tg-archive backfill` to continue from where it stopped.\n\n")
	}

	gaps, err := st.Gaps(*minJump)
	if err != nil {
		return err
	}
	if len(gaps) == 0 {
		fmt.Println("No holes found in supergroups or channels.")
		fmt.Println("(Private chats cannot be checked this way: Telegram numbers their")
		fmt.Println("messages per account, so id jumps there are normal, not missing data.)")
		return nil
	}
	total := 0
	for _, g := range gaps {
		total += g.Missing
	}
	fmt.Printf("%d suspicious gap(s), up to %d messages missing:\n\n", len(gaps), total)
	for i, g := range gaps {
		if i == 20 && !*fix {
			fmt.Printf("  … and %d more\n", len(gaps)-20)
			break
		}
		fmt.Printf("  %-40s #%d → #%d  (~%d missing, %s → %s)\n",
			trunc(g.Title, 40), g.AfterID, g.BeforeID, g.Missing,
			shortDate(g.AfterDate), shortDate(g.BeforeDate))
	}
	if !*fix {
		fmt.Println("\nRun `tg-archive doctor --fix` to fetch them.")
		return nil
	}
	fmt.Println("\nFilling gaps…")
	return tgclient.New(cfg, st).FillGaps(ctx, gaps)
}

func cmdMedia(ctx context.Context) error {
	fs := flag.NewFlagSet("media", flag.ExitOnError)
	chat := fs.String("chat", "", "only this chat")
	limit := fs.Int("limit", 500, "max files in this pass")
	_ = fs.Parse(os.Args[2:])

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	var chatID int64
	if *chat != "" {
		if chatID, err = resolveChat(st, *chat); err != nil {
			return err
		}
	}
	return tgclient.New(cfg, st).DownloadMediaPass(ctx, chatID, *limit)
}

// reorderFlags moves flags ahead of positional arguments, because Go's flag package stops
// parsing at the first non-flag: `search "words" --limit 5` would otherwise treat
// "--limit 5" as part of the search text. valued lists flags that take a value.
func reorderFlags(args []string, valued ...string) []string {
	takesValue := map[string]bool{}
	for _, v := range valued {
		takesValue[v] = true
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if takesValue[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func cmdSearch() error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	chat := fs.String("chat", "", "limit to one chat")
	sender := fs.String("sender", "", "limit to a sender")
	from := fs.String("from", "", "on or after YYYY-MM-DD")
	to := fs.String("to", "", "on or before YYYY-MM-DD")
	limit := fs.Int("limit", 40, "max hits")
	_ = fs.Parse(reorderFlags(os.Args[2:], "chat", "sender", "from", "to", "limit"))
	if fs.NArg() == 0 {
		return fmt.Errorf(`usage: tg-archive search "words" [--chat X] [--from 2026-01-01]`)
	}

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()

	opts := store.SearchOpts{Sender: *sender, From: *from, To: *to, Limit: *limit}
	if *chat != "" {
		if opts.ChatID, err = resolveChat(st, *chat); err != nil {
			return err
		}
	}
	hits, err := st.Search(strings.Join(fs.Args(), " "), opts)
	if err != nil {
		return err
	}
	titles := map[int64]string{}
	for _, m := range hits {
		title, ok := titles[m.ChatID]
		if !ok {
			if c, err := st.Chat(m.ChatID); err == nil {
				title = c.Title
			}
			titles[m.ChatID] = title
		}
		when := m.Date
		if t, err := time.Parse(time.RFC3339, m.Date); err == nil {
			when = t.In(cfg.Location()).Format("2006-01-02 15:04")
		}
		body := strings.ReplaceAll(m.Text, "\n", " ")
		fmt.Printf("%s  %s  %s: %s\n", when, trunc(title, 24), trunc(m.Sender, 16), trunc(body, 90))
	}
	fmt.Printf("\n%d hit(s)\n", len(hits))
	return nil
}

func shortDate(iso string) string {
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.Format("2006-01-02")
	}
	return iso
}

func cmdMCP(ctx context.Context) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	allowSend := fs.Bool("allow-send", false, "expose the send_message tool (off by default)")
	_ = fs.Parse(os.Args[2:])

	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	return mcpserver.New(cfg, st, *allowSend).Run(ctx, version)
}

func cmdImportTelethon() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("pass the StringSession: tg-archive import-telethon 1BVtsO...")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return tgclient.ImportTelethonSession(cfg, os.Args[2])
}

func cmdRerender() error {
	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.MarkAllDirty(); err != nil {
		return err
	}
	n, err := render.New(st, cfg.OutDir, cfg.Location()).Flush()
	if err != nil {
		return err
	}
	fmt.Printf("files rebuilt: %d\n", n)
	return nil
}

func cmdStatus() error {
	cfg, st, err := open()
	if err != nil {
		return err
	}
	defer st.Close()
	msgs, err := st.Count("messages")
	if err != nil {
		return err
	}
	rows, err := st.Summary()
	if err != nil {
		return err
	}
	fmt.Printf("archive:   %s\n", cfg.OutDir)
	fmt.Printf("database:  %s\n", cfg.DBPath())
	fmt.Printf("messages:  %d\n", msgs)
	fmt.Printf("chats:     %d\n", len(rows))
	if len(rows) > 0 {
		fmt.Println("\nmost recent:")
		for i, c := range rows {
			if i == 10 {
				break
			}
			fmt.Printf("  %-40s %6d  %s\n", trunc(c.Title, 40), c.Count, c.Last)
		}
	}
	return nil
}

func ask(in *bufio.Reader, label, def string) string {
	if def != "" && def != "0" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func askBool(in *bufio.Reader, label string, def bool) bool {
	d := "y"
	if !def {
		d = "n"
	}
	fmt.Printf("%s? [%s]: ", label, d)
	line, _ := in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes", "т", "так", "1":
		return true
	}
	return false
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
