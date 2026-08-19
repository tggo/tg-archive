// Package config holds tg-archive settings: paths, api credentials, chat filters.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	APIID   int    `json:"api_id"`
	APIHash string `json:"api_hash"`

	OutDir   string `json:"out_dir"`
	Timezone string `json:"timezone"`
	// DBPathOverride keeps the database next to the archive instead of in the config dir.
	DBPathOverride string `json:"db_path,omitempty"`

	Private  bool `json:"private"`
	Groups   bool `json:"groups"`
	Saved    bool `json:"saved"`
	Channels bool `json:"channels"`
	Bots     bool `json:"bots"`

	SkipIDs []int64 `json:"skip_ids,omitempty"`
	OnlyIDs []int64 `json:"only_ids,omitempty"`

	dir string
	loc *time.Location
}

// Dir is ~/.config/tg-archive (or $XDG_CONFIG_HOME / $TG_ARCHIVE_HOME).
// XDG is used on macOS too: CLI tools live in ~/.config, not "Application Support".
func Dir() string {
	if d := os.Getenv("TG_ARCHIVE_HOME"); d != "" {
		return d
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "tg-archive"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tg-archive")
}

func Path() string        { return filepath.Join(Dir(), "config.json") }
func (c *Config) SessionPath() string { return filepath.Join(c.dir, "session.json") }
func (c *Config) DBPath() string {
	if c.DBPathOverride != "" {
		return expandHome(c.DBPathOverride)
	}
	return filepath.Join(c.dir, "state.db")
}
func (c *Config) Location() *time.Location { return c.loc }

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		OutDir:   filepath.Join(home, "TelegramArchive"),
		Timezone: "Local",
		Private:  true,
		Groups:   true,
		Saved:    true,
		Channels: false,
		Bots:     false,
		dir:      Dir(),
	}
}

func Load() (*Config, error) {
	c := Default()
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config at %s — run `tg-archive setup`", Path())
		}
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	c.dir = Dir()
	c.applyEnv()
	if c.APIID == 0 || c.APIHash == "" {
		return nil, fmt.Errorf("empty api_id/api_hash in %s — see `tg-archive setup`", Path())
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", c.Timezone, err)
	}
	c.loc = loc
	c.OutDir = expandHome(c.OutDir)
	return c, nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// applyEnv lets environment variables override key fields (handy for CI and tests).
func (c *Config) applyEnv() {
	if v := os.Getenv("TG_API_ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.APIID = n
		}
	}
	if v := os.Getenv("TG_API_HASH"); v != "" {
		c.APIHash = v
	}
	if v := os.Getenv("TG_OUT_DIR"); v != "" {
		c.OutDir = v
	}
	if v := os.Getenv("TG_DB"); v != "" {
		c.DBPathOverride = v
	}
}

func (c *Config) Save() error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(b, '\n'), 0o600)
}

// Allowed reports whether this chat is archived, honouring the skip/only lists.
func (c *Config) Allowed(id int64, kind string) bool {
	for _, s := range c.SkipIDs {
		if s == id {
			return false
		}
	}
	if len(c.OnlyIDs) > 0 {
		found := false
		for _, o := range c.OnlyIDs {
			if o == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	switch kind {
	case "private":
		return c.Private
	case "group":
		return c.Groups
	case "channel":
		return c.Channels
	case "saved":
		return c.Saved
	case "bot":
		return c.Bots
	}
	return false
}
