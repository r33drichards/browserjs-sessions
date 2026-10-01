package hosts

import "testing"

func TestSplit(t *testing.T) {
	for host, want := range map[string][2]string{
		"app.example.com":      {"app.example.com", ""},
		"App.Example.COM":      {"app.example.com", ""},
		"app.example.com:8443": {"app.example.com", "8443"},
		"app.example.com.":     {"app.example.com", ""},
		"app.example.com.:443": {"app.example.com", "443"},
		"[::1]:8080":           {"::1", "8080"},
		"10.0.0.7:8080":        {"10.0.0.7", "8080"},
		"":                     {"", ""},
	} {
		if name, port := Split(host); name != want[0] || port != want[1] {
			t.Errorf("Split(%q) = %q, %q; want %q, %q", host, name, port, want[0], want[1])
		}
	}
}
