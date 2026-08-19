package tgclient

import "testing"

func TestSlugify(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"Тарас Шевченко", 123, "taras-shevchenko-123"},
		{"Family 👨‍👩‍👧‍👧", -321784895, "family-321784895"},
		{"", 5, "chat-5"},
		{"!!!", -1001349669071, "chat-1001349669071"},
		{"Genesis Network (ex-Барахолка)", -100, "genesis-network-ex-barakholka-100"},
	} {
		if got := slugify(tc.name, tc.id); got != tc.want {
			t.Errorf("slugify(%q, %d) = %q, want %q", tc.name, tc.id, got, tc.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{512, "512B"}, {2048, "2.0KB"}, {5 * 1024 * 1024, "5.0MB"}} {
		if got := humanSize(tc.in); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
