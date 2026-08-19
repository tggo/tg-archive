package tgclient

import (
	"context"
	"fmt"
	"os"

	"github.com/gotd/td/session"
)

// ImportTelethonSession переносить авторизацію з Telethon StringSession,
// щоб при міграції з python-версії не логінитися вдруге.
func ImportTelethonSession(cfg sessionPathProvider, stringSession string) error {
	data, err := session.TelethonSession(stringSession)
	if err != nil {
		return fmt.Errorf("не схоже на Telethon StringSession: %w", err)
	}
	var buf []byte
	loader := session.Loader{Storage: &memStorage{out: &buf}}
	if err := loader.Save(context.Background(), data); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.SessionPath(), buf, 0o600); err != nil {
		return err
	}
	fmt.Printf("Сесію імпортовано у %s (DC %d)\n", cfg.SessionPath(), data.DC)
	return nil
}

type sessionPathProvider interface{ SessionPath() string }

// memStorage ловить у пам'ять те, що session.Loader хоче записати.
type memStorage struct{ out *[]byte }

func (m *memStorage) LoadSession(_ context.Context) ([]byte, error) { return *m.out, nil }
func (m *memStorage) StoreSession(_ context.Context, data []byte) error {
	*m.out = append([]byte(nil), data...)
	return nil
}
