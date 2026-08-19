package tgclient

import (
	"context"
	"fmt"
	"os"

	"github.com/gotd/td/session"
)

// ImportTelethonSession carries authorization over from a Telethon StringSession so a
// migration does not require signing in again.
func ImportTelethonSession(cfg sessionPathProvider, stringSession string) error {
	data, err := session.TelethonSession(stringSession)
	if err != nil {
		return fmt.Errorf("does not look like a Telethon StringSession: %w", err)
	}
	var buf []byte
	loader := session.Loader{Storage: &memStorage{out: &buf}}
	if err := loader.Save(context.Background(), data); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.SessionPath(), buf, 0o600); err != nil {
		return err
	}
	fmt.Printf("Session imported into %s (DC %d)\n", cfg.SessionPath(), data.DC)
	return nil
}

type sessionPathProvider interface{ SessionPath() string }

// memStorage captures whatever session.Loader wants to write.
type memStorage struct{ out *[]byte }

func (m *memStorage) LoadSession(_ context.Context) ([]byte, error) { return *m.out, nil }
func (m *memStorage) StoreSession(_ context.Context, data []byte) error {
	*m.out = append([]byte(nil), data...)
	return nil
}
