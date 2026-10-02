package policy

import (
	"embed"
	"sort"
	"strings"

	"github.com/r33drichards/computer-use/backend/internal/sessions"
)

// The ready-made policies: copies of docs/contracts/policy/examples/*.rego,
// here because a Go binary can only embed files below its package and the
// image is built from backend/ alone. A test keeps them the same as the
// contract's.
//
//go:embed presets/*.rego
var presetFiles embed.FS

const (
	presetDir    = "presets"
	presetSuffix = ".rego"
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
		id := strings.TrimSuffix(entry.Name(), presetSuffix)
		out = append(out, Preset{ID: id, Title: presetTitle(id), Description: presetDescription(string(source)), Kind: KindRego, Source: string(source)})
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

// presetDescription is the comment a preset begins with, as one line.
func presetDescription(source string) string {
	var words []string
	for _, line := range strings.Split(source, "\n") {
		text, ok := strings.CutPrefix(line, "#")
		if !ok {
			break
		}
		words = append(words, strings.Fields(text)...)
	}
	if len(words) == 0 {
		panic("a policy preset must begin with a comment that says what it allows")
	}
	return strings.Join(words, " ")
}

// presetTitles are the titles that are not their ID with spaces.
var presetTitles = map[string]string{"read-only-shell": "Read-only shell"}

// presetTitle makes "No scripting" of "no-scripting".
func presetTitle(id string) string {
	if title, ok := presetTitles[id]; ok {
		return title
	}
	words := strings.ReplaceAll(id, "-", " ")
	return strings.ToUpper(words[:1]) + words[1:]
}

// Unrestricted is the policy of a session that was asked to have none:
// every operation in the browser, full control of the desktop and any shell
// command, which is what a session without policies does. It is the
// contract's own example and is not sent to the operator to be checked.
func Unrestricted() sessions.PolicySpec {
	return sessions.PolicySpec{Kind: KindRego, Source: presets[0].Source, Mode: sessions.PolicyModeEditor}
}
