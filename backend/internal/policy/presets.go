package policy

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// The ready-made policies: copies of docs/contracts/policy/examples/*.policy.json,
// here because a Go binary can only embed files below its package and the
// image is built from backend/ alone. A test keeps them the same as the
// contract's.
//
//go:embed presets/*.policy.json
var presetFiles embed.FS

const (
	presetDir    = "presets"
	presetSuffix = ".policy.json"
	// unrestrictedID is the preset a session gets when it is asked to have
	// no policy, and the one a policy is reset to.
	unrestrictedID = "unrestricted"
)

// Preset is a ready-made policy the create page offers.
type Preset struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
}

var presets = mustLoadPresets()

// Presets returns the ready-made policies, the unrestricted one first.
func Presets() []Preset { return append([]Preset(nil), presets...) }

func mustLoadPresets() []Preset {
	entries, err := presetFiles.ReadDir(presetDir)
	if err != nil {
		panic(err)
	}
	var out []Preset
	for _, entry := range entries {
		source, err := presetFiles.ReadFile(presetDir + "/" + entry.Name())
		if err != nil {
			panic(err)
		}
		var doc struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(source, &doc); err != nil {
			panic(fmt.Sprintf("policy preset %s: %v", entry.Name(), err))
		}
		id := strings.TrimSuffix(entry.Name(), presetSuffix)
		out = append(out, Preset{ID: id, Title: presetTitle(id), Description: doc.Description, Kind: KindJSON, Source: string(source)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if first, other := out[i].ID == unrestrictedID, out[j].ID == unrestrictedID; first != other {
			return first
		}
		return out[i].ID < out[j].ID
	})
	if len(out) == 0 || out[0].ID != unrestrictedID {
		panic("policy presets: there is no " + unrestrictedID + presetSuffix)
	}
	return out
}

// presetTitle makes "No scripting" of "no-scripting".
func presetTitle(id string) string {
	words := strings.ReplaceAll(id, "-", " ")
	return strings.ToUpper(words[:1]) + words[1:]
}

// Unrestricted is the policy of a session that was asked to have none:
// every browser operation, which is what a session without policies does.
// It is the contract's own example and is not sent to the operator to be
// checked.
func Unrestricted() sessions.PolicySpec {
	return sessions.PolicySpec{Kind: KindJSON, Source: presets[0].Source, Mode: sessions.PolicyModeEditor}
}
