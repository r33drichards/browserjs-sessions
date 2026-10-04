package config

import (
	"slices"
	"strings"
	"testing"
)

func TestSkillsRolloutConfiguration(t *testing.T) {
	m := valid()
	c, err := FromEnv(env(m))
	if err != nil || c.SkillsImageDigest != "" || len(c.SkillsEmails) != 0 {
		t.Fatalf("default: %+v %v", c, err)
	}
	m["MCP_SKILLS_IMAGE_DIGEST"] = "sha256:" + strings.Repeat("a", 64)
	m["MCP_SKILLS_EMAILS"] = " Alice@Example.com , ,bob@example.com"
	c, err = FromEnv(env(m))
	if err != nil || !slices.Equal(c.SkillsEmails, []string{"alice@example.com", "bob@example.com"}) {
		t.Fatalf("configured: %+v %v", c, err)
	}
	m["MCP_SKILLS_IMAGE_DIGEST"] = "latest"
	if _, err := FromEnv(env(m)); err == nil || !strings.Contains(err.Error(), "MCP_SKILLS_IMAGE_DIGEST") {
		t.Fatalf("invalid digest: %v", err)
	}
}
