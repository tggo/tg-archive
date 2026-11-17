package tgclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tgerr"
)

func TestSessionLost(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"key unregistered", tgerr.New(401, "AUTH_KEY_UNREGISTERED"), true},
		{"session revoked", tgerr.New(401, "SESSION_REVOKED"), true},
		{"wrapped 401", fmt.Errorf("rpcDoRequest: %w", tgerr.New(401, "AUTH_KEY_UNREGISTERED")), true},
		{"one private channel", tgerr.New(400, "CHANNEL_PRIVATE"), false},
		{"server hiccup", tgerr.New(500, "RPC_CALL_FAIL"), false},
		{"plain error", errors.New("context canceled"), false},
	}
	for _, c := range cases {
		if got := sessionLost(c.err); got != c.want {
			t.Errorf("%s: sessionLost = %v, want %v", c.name, got, c.want)
		}
	}
}
