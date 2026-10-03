package features

import (
	"strings"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
)

func TestSkillsTargetsOnlyConfiguredIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	skills, err := NewSkills(t.Context(), digest, []string{" Alice@Example.com ", ""})
	if err != nil {
		t.Fatal(err)
	}
	for email, want := range map[string]string{"alice@example.com": digest, "bob@example.com": "", "": ""} {
		if got := skills.ImageDigest(t.Context(), email); got != want {
			t.Errorf("%q: %q, want %q", email, got, want)
		}
	}
	off, err := NewSkills(t.Context(), "", []string{"alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := off.ImageDigest(t.Context(), "alice@example.com"); got != "" {
		t.Errorf("missing digest enabled %q", got)
	}
}

func TestProviderFailureDefaultsOff(t *testing.T) {
	skills, err := NewSkillsWithProvider(t.Context(), "sha256:"+strings.Repeat("a", 64), openfeature.NoopProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if got := skills.ImageDigest(t.Context(), "alice@example.com"); got != "" {
		t.Errorf("no provider enabled %q", got)
	}
}
