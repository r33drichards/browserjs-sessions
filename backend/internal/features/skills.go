// Package features evaluates server-side OpenFeature flags.
package features

import (
	"context"
	"strings"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/isolated"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
)

const SkillsFlag = "mcp-skills"

type Skills struct {
	client *openfeature.Client
	digest string
}

// NewSkills uses OpenFeature's in-memory provider for a deployment-configured
// allowlist. No remote service or credentials are needed for the first rollout.
func NewSkills(ctx context.Context, digest string, emails []string) (*Skills, error) {
	allowed := map[string]bool{}
	for _, email := range emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			allowed[email] = true
		}
	}
	evaluate := func(_ memprovider.InMemoryFlag, values openfeature.FlattenedContext) (any, openfeature.ProviderResolutionDetail) {
		email, _ := values[openfeature.TargetingKey].(string)
		enabled := email != "" && allowed[email]
		reason := openfeature.DefaultReason
		if enabled {
			reason = openfeature.TargetingMatchReason
		}
		return enabled, openfeature.ProviderResolutionDetail{Reason: reason}
	}
	provider := memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{
		SkillsFlag: {
			Key: SkillsFlag, State: memprovider.Enabled, DefaultVariant: "off",
			Variants: map[string]any{"off": false, "on": true}, ContextEvaluator: &evaluate,
		},
	})
	return NewSkillsWithProvider(ctx, digest, provider)
}

// NewSkillsWithProvider allows an external OpenFeature provider to replace the
// initial allowlist without changing the session-creation integration.
func NewSkillsWithProvider(ctx context.Context, digest string, provider openfeature.FeatureProvider) (*Skills, error) {
	api := isolated.NewAPI()
	if err := api.SetProviderAndWait(ctx, provider); err != nil {
		return nil, err
	}
	return &Skills{client: api.NewClient(), digest: digest}, nil
}

// ImageDigest defaults off on provider errors, an empty user, or missing image.
// Only authenticated identity belongs here; never pass caller-provided attributes.
func (s *Skills) ImageDigest(ctx context.Context, email string) string {
	if s == nil || s.digest == "" || email == "" {
		return ""
	}
	if s.client.Boolean(ctx, SkillsFlag, false, openfeature.NewEvaluationContext(email, nil)) {
		return s.digest
	}
	return ""
}
