package sessions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A canary session: one made from the blueprint with other image digests
// than the ones it names, to try a new build of the session images on one
// session while every other session, and the warm pool, keep the old ones
// (docs/releases.md). It is always started cold: a warm pod already runs
// the old images.

// AnnCanary marks a canary session's Sandbox, with the digests it was given.
const AnnCanary = "browserjs.dev/canary"

// ErrCanary is a canary request that cannot be honoured as it is written.
var ErrCanary = errors.New("canary")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type canaryKey struct{}

// WithImageDigests asks the Create made with the context for a canary
// session: digests is container name (of the blueprint's pod) to image
// digest ("sha256:..."). Only the digest of an image the blueprint already
// names by digest is replaced; its repository is the blueprint's.
func WithImageDigests(ctx context.Context, digests map[string]string) context.Context {
	return context.WithValue(ctx, canaryKey{}, digests)
}

func imageDigests(ctx context.Context) map[string]string {
	digests, _ := ctx.Value(canaryKey{}).(map[string]string)
	return digests
}

// CheckImageDigests says whether digests is something WithImageDigests can
// be given, before anything is created.
func CheckImageDigests(digests map[string]string) error {
	if len(digests) == 0 {
		return fmt.Errorf("%w: name at least one container and its digest", ErrCanary)
	}
	for container, digest := range digests {
		if container == "" || !digestPattern.MatchString(digest) {
			return fmt.Errorf("%w: %q is not a digest (sha256: and 64 hex digits)", ErrCanary, digest)
		}
	}
	return nil
}

// applyImageDigests replaces, in a rendered Sandbox spec, the digest of each
// named container's image.
func applyImageDigests(spec map[string]any, digests map[string]string) error {
	containers, _, _ := unstructured.NestedSlice(spec, "podTemplate", "spec", "containers")
	done := map[string]bool{}
	for i, c := range containers {
		container, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := container["name"].(string)
		digest, wanted := digests[name]
		if !wanted {
			continue
		}
		image, _ := container["image"].(string)
		repository, _, pinned := strings.Cut(image, "@")
		if !pinned {
			return fmt.Errorf("%w: the blueprint's %s image is not named by digest", ErrCanary, name)
		}
		container["image"] = repository + "@" + digest
		containers[i] = container
		done[name] = true
	}
	for name := range digests {
		if !done[name] {
			return fmt.Errorf("%w: the blueprint has no container %q", ErrCanary, name)
		}
	}
	return unstructured.SetNestedSlice(spec, containers, "podTemplate", "spec", "containers")
}

// canaryAnnotation is the digests as "browser=sha256:...,mcp-js=sha256:...".
func canaryAnnotation(digests map[string]string) string {
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	// Two or three entries: sorted by hand to keep the imports small.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+digests[name])
	}
	return strings.Join(parts, ",")
}
