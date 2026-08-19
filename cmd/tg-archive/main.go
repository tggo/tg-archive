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

	"github.com/tggo/tg-archive/internal/config"
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
  tg-archive rerender           rebuild every .md from the database
  tg-archive status             what the archive holds right now
  tg-archive import-telethon <StringSession>
                                carry over a Telethon session instead of logging in again
  tg-archive version

Config: %s
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, version, config.Path())
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
