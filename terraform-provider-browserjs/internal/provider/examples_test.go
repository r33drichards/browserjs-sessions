package provider

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/fakeapi"
)

// The .rego files under examples/ are copies of the contract's example
// policies, so that each example directory stands alone. A copy that has
// drifted from docs/contracts/policy/examples fails here.
func TestExamplePoliciesAreTheContracts(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	contract := filepath.Join("..", "..", "..", "docs", "contracts", "policy", "examples")
	copies := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".rego" {
			return err
		}
		copies++
		have, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(filepath.Join(contract, d.Name()))
		if err != nil {
			t.Errorf("%s is not a copy of a contract example: %v", path, err)
			return nil
		}
		if string(have) != string(want) {
			t.Errorf("%s differs from %s: copy it again", path, filepath.Join(contract, d.Name()))
		}
		if v := fakeapi.Validate(string(have)); !v.OK || len(v.Warnings) != 0 {
			t.Errorf("%s: errors %+v, warnings %+v", path, v.Errors, v.Warnings)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if copies < 3 {
		t.Errorf("%d .rego files under examples/, want at least 3", copies)
	}
}
