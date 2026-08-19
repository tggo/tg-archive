// tg-archive — архівує твій власний Telegram у Markdown, майже в реальному часі.
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

var version = "dev" // підставляється лінкером при релізі

const usage = `tg-archive %s — Markdown-архів твого Telegram

  tg-archive setup              майстер: api_id/api_hash, куди писати, що архівувати
  tg-archive login [--phone N]  вхід у Telegram (код приходить у застосунок)
  tg-archive chats              список діалогів та їх id
  tg-archive backfill [--limit N] [--force]
                                вся історія; переривання не втрачає прогрес
  tg-archive live               демон: нове/редаговане/видалене → .md за ~3с
  tg-archive send --chat X --text "..." [--reply-to N]
  tg-archive rerender           перебудувати всі .md з бази
  tg-archive status             що вже в архіві
  tg-archive import-telethon <StringSession>
                                перенести авторизацію з Telethon (без повторного логіну)
  tg-archive version

Конфіг: %s
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
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

// open — спільний старт для команд, яким потрібні і конфіг, і база.
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
		fmt.Println("Знайдено наявний конфіг — Enter лишає поточне значення.")
	}

	fmt.Print(`
Потрібні власні api_id / api_hash (у Telegram вони видаються на акаунт):
  1. https://my.telegram.org → увійти за номером
  2. API development tools → створити застосунок (будь-яка назва)
  3. скопіювати api_id і api_hash

`)
	if v := ask(in, "api_id", fmt.Sprint(cfg.APIID)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("api_id має бути числом: %w", err)
		}
		cfg.APIID = n
	}
	if v := ask(in, "api_hash", cfg.APIHash); v != "" {
		cfg.APIHash = v
	}
	if v := ask(in, "куди писати архів", cfg.OutDir); v != "" {
		cfg.OutDir = v
	}
	if v := ask(in, "часова зона (напр. Europe/Kyiv або Local)", cfg.Timezone); v != "" {
		cfg.Timezone = v
	}
	cfg.Private = askBool(in, "архівувати приватні чати", cfg.Private)
	cfg.Groups = askBool(in, "архівувати групи", cfg.Groups)
	cfg.Saved = askBool(in, "архівувати Saved Messages", cfg.Saved)
	cfg.Channels = askBool(in, "архівувати канали (підписки)", cfg.Channels)
	cfg.Bots = askBool(in, "архівувати чати з ботами", cfg.Bots)

	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("\nЗбережено: %s\nДалі: tg-archive login\n", config.Path())
	return nil
}

func cmdLogin(ctx context.Context) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	phone := fs.String("phone", "", "номер телефону, напр. +380671234567")
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
		fmt.Printf("\nусього: %d\n", len(ds))
		return nil
	})
}

func cmdBackfill(ctx context.Context) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	limit := fs.Int("limit", 0, "макс. повідомлень на чат за прохід (0 = без межі)")
	force := fs.Bool("force", false, "перепройти навіть завершені чати")
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
	chat := fs.String("chat", "", "id, @username або частина назви чату")
	text := fs.String("text", "", "текст повідомлення")
	replyTo := fs.Int("reply-to", 0, "id повідомлення, на яке відповідаємо")
	_ = fs.Parse(os.Args[2:])
	if *chat == "" || *text == "" {
		return fmt.Errorf("потрібні --chat і --text")
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

// resolveChat приймає id або шматок назви й вимагає однозначності.
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
		return 0, fmt.Errorf("чат %q не знайдено — спершу `tg-archive chats`", q)
	case 1:
		return found[0].ID, nil
	}
	fmt.Fprintln(os.Stderr, "неоднозначно, уточни:")
	for _, c := range found {
		fmt.Fprintf(os.Stderr, "  %16d  %s\n", c.ID, c.Title)
	}
	return 0, fmt.Errorf("знайдено %d чатів", len(found))
}

func cmdImportTelethon() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("вкажи StringSession: tg-archive import-telethon 1BVtsO...")
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
	fmt.Printf("перебудовано файлів: %d\n", n)
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
	fmt.Printf("архів:        %s\n", cfg.OutDir)
	fmt.Printf("база:         %s\n", cfg.DBPath())
	fmt.Printf("повідомлень:  %d\n", msgs)
	fmt.Printf("чатів:        %d\n", len(rows))
	if len(rows) > 0 {
		fmt.Println("\nостанні:")
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
