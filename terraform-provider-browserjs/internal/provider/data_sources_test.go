package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/fakeapi"
)

func TestSessionDataSource(t *testing.T) {
	h := newHarness(t)
	a := h.fake.AddSession("research")
	h.fake.AddSession("twin")
	h.fake.AddSession("twin")

	byID, diags := h.data("browserjs_session", cfg{"id": a})
	noErrors(t, "by id", diags)
	byName, diags := h.data("browserjs_session", cfg{"name": "research"})
	noErrors(t, "by name", diags)
	want := map[string]any{
		"id": a, "name": "research", "state": "running", "owner": "dev@example.com",
		"mcp_url": "https://sessions.example.test/" + a + "/mcp",
	}
	if !reflect.DeepEqual(byID, want) || !reflect.DeepEqual(byName, want) {
		t.Errorf("by id %v\nby name %v\nwant %v", byID, byName, want)
	}

	_, diags = h.data("browserjs_session", cfg{"name": "twin"})
	wantError(t, diags, "name", "More than one session", "Use id instead")
	_, diags = h.data("browserjs_session", cfg{"name": "nobody"})
	wantError(t, diags, "name", "No such session")
	_, diags = h.data("browserjs_session", cfg{"id": "s-zzzzz"})
	wantError(t, diags, "id", "No such session")
	_, diags = h.data("browserjs_session", cfg{})
	wantError(t, diags, "id", "name")
	_, diags = h.data("browserjs_session", cfg{"id": a, "name": "research"})
	wantError(t, diags, "id", "name")
}

func TestSessionsDataSource(t *testing.T) {
	h := newHarness(t)
	got, diags := h.data("browserjs_sessions", cfg{})
	noErrors(t, "empty", diags)
	if l := got["sessions"].([]any); len(l) != 0 {
		t.Errorf("sessions %v", l)
	}
	a, b := h.fake.AddSession("one"), h.fake.AddSession("two")
	got, diags = h.data("browserjs_sessions", cfg{})
	noErrors(t, "two", diags)
	l := got["sessions"].([]any)
	if len(l) != 2 || l[0].(map[string]any)["id"] != a || l[1].(map[string]any)["id"] != b || l[1].(map[string]any)["name"] != "two" {
		t.Errorf("sessions %v", l)
	}
}

// examples are the five policies of docs/contracts/policy/examples, written
// as browserjs_policy_document configurations.
var examples = map[string]cfg{
	"unrestricted": {
		"description":      "No restrictions: every browser operation, with any parameters.",
		"allow_operations": []any{"*"},
	},
	"no-scripting": {
		"description":      "Everything except running script in the page or replacing its content.",
		"allow_operations": []any{"*"},
		"deny_operations":  []any{"setContent", "evaluate"},
	},
	"observe-only": {
		"description":      "Look, do not touch: open https pages, wait, and take screenshots.",
		"allow_operations": []any{"screenshot", "url", "wait", "setViewport"},
		"rule": []any{
			cfg{"operation": "navigate", "constraint": []any{cfg{"parameter": "url", "schemes": []any{"https"}}}},
		},
	},
	"one-site": {
		"description":      "The agent may be sent only to example.com and its subdomains, and may type only short text.",
		"allow_operations": []any{"click", "press", "select", "wait", "screenshot", "url", "setViewport"},
		"rule": []any{
			cfg{"operation": "navigate", "constraint": []any{
				cfg{"parameter": "url", "schemes": []any{"https"}, "hosts": []any{"example.com", "*.example.com"}},
			}},
			cfg{"operation": "type", "constraint": []any{cfg{"parameter": "text", "max_length": 500}}},
		},
	},
	"form-filling": {
		"description":      "Fill in forms on two sites: printable text only, a few keys, sane viewport sizes, no script.",
		"allow_operations": []any{"click", "select", "wait", "screenshot", "url"},
		"deny_operations":  []any{"evaluate", "setContent"},
		"rule": []any{
			cfg{"operation": "navigate", "constraint": []any{
				cfg{"parameter": "url", "hosts": []any{"forms.example.org", "*.intranet.example.org"}},
			}},
			cfg{"operation": "type", "constraint": []any{
				cfg{"parameter": "text", "max_length": 200, "pattern": `^[\x20-\x7E]*$`},
			}},
			cfg{"operation": "press", "constraint": []any{
				cfg{"parameter": "key", "allowed": []any{"Enter", "Tab", "Escape"}},
			}},
			cfg{"operation": "setViewport", "constraint": []any{
				cfg{"parameter": "width", "min": 320, "max": 1920},
				cfg{"parameter": "height", "min": 200, "max": 1200},
			}},
		},
	},
}

// sets are the arrays of the JSON format whose order means nothing.
func canonical(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = canonical(e, k)
		}
	case []any:
		for i, e := range x {
			x[i] = canonical(e, "")
		}
		if key == "operations" || key == "hosts" || key == "schemes" {
			seen := map[any]bool{}
			for _, e := range x {
				seen[e] = true
			}
			return seen
		}
	}
	return v
}

func TestPolicyDocumentRendersTheContractExamples(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join("..", "..", "..", "docs", "contracts", "policy", "examples")
	files, err := filepath.Glob(filepath.Join(dir, "*.policy.json"))
	if err != nil || len(files) != len(examples) {
		t.Fatalf("%d example policies in %s, %d in this test (%v)", len(files), dir, len(examples), err)
	}
	for name, c := range examples {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, name+".policy.json"))
			if err != nil {
				t.Fatal(err)
			}
			got, diags := h.data("browserjs_policy_document", c)
			noErrors(t, "read", diags)
			rendered := got["json"].(string)

			var want, have any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(rendered), &have); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, rendered)
			}
			if !reflect.DeepEqual(canonical(have, ""), canonical(want, "")) {
				t.Errorf("rendered\n%s\nwant\n%s", rendered, raw)
			}
			// What the data source renders, the API accepts.
			if v := fakeapi.Validate("json", rendered); !v.OK {
				t.Errorf("the rendered policy does not validate: %v", v.Errors)
			}
		})
	}
}

func TestPolicyDocumentIsStable(t *testing.T) {
	h := newHarness(t)
	got, diags := h.data("browserjs_policy_document", cfg{
		"allow_operations": []any{"wait", "click"},
		"deny_operations":  []any{"setContent", "evaluate"},
		"rule": []any{
			cfg{"operation": "setViewport", "constraint": []any{
				cfg{"parameter": "width", "min": 320, "max": 1920.5},
				cfg{"parameter": "height", "allowed_numbers": []any{600, 768}},
				cfg{"parameter": "mobile", "allowed_booleans": []any{false}},
			}},
			cfg{"operation": "navigate", "constraint": []any{
				cfg{"parameter": "url", "hosts": []any{"b.example.com", "a.example.com"}, "pattern": "^https://[a-z.]+/<&>$"},
			}},
			cfg{"operation": "press"},
		},
	})
	noErrors(t, "read", diags)
	const want = `{
  "version": 1,
  "allow": {
    "operations": [
      "click",
      "wait"
    ],
    "rules": [
      {
        "operation": "setViewport",
        "constraints": {
          "height": {
            "allowed": [
              600,
              768
            ]
          },
          "mobile": {
            "allowed": [
              false
            ]
          },
          "width": {
            "min": 320,
            "max": 1920.5
          }
        }
      },
      {
        "operation": "navigate",
        "constraints": {
          "url": {
            "pattern": "^https://[a-z.]+/<&>$",
            "hosts": [
              "a.example.com",
              "b.example.com"
            ]
          }
        }
      },
      {
        "operation": "press"
      }
    ]
  },
  "deny": {
    "operations": [
      "evaluate",
      "setContent"
    ]
  }
}`
	if got["json"] != want {
		t.Errorf("rendered\n%s\nwant\n%s", got["json"], want)
	}
}

func TestPolicyDocumentEmpty(t *testing.T) {
	h := newHarness(t)
	got, diags := h.data("browserjs_policy_document", cfg{})
	noErrors(t, "read", diags)
	// Deny by default: a policy that allows nothing.
	if want := "{\n  \"version\": 1\n}"; got["json"] != want {
		t.Errorf("rendered %q", got["json"])
	}
}

func TestPolicyDocumentRejects(t *testing.T) {
	h := newHarness(t)
	for name, tc := range map[string]struct {
		c    cfg
		want []string
	}{
		"unknown operation":   {cfg{"allow_operations": []any{"clik"}}, []string{"allow_operations", "clik"}},
		"star in deny":        {cfg{"deny_operations": []any{"*"}}, []string{"deny_operations"}},
		"rule operation":      {cfg{"rule": []any{cfg{"operation": "*"}}}, []string{"operation"}},
		"empty constraint":    {cfg{"rule": []any{cfg{"operation": "type", "constraint": []any{cfg{"parameter": "text"}}}}}, []string{"Empty constraint", `"text"`}},
		"parameter twice":     {cfg{"rule": []any{cfg{"operation": "type", "constraint": []any{cfg{"parameter": "text", "max_length": 1}, cfg{"parameter": "text", "pattern": "a"}}}}}, []string{"Parameter constrained twice"}},
		"parameter name":      {cfg{"rule": []any{cfg{"operation": "type", "constraint": []any{cfg{"parameter": "te xt", "max_length": 1}}}}}, []string{"parameter"}},
		"negative max_length": {cfg{"rule": []any{cfg{"operation": "type", "constraint": []any{cfg{"parameter": "text", "max_length": -1}}}}}, []string{"max_length"}},
		"upper-case host":     {cfg{"rule": []any{cfg{"operation": "navigate", "constraint": []any{cfg{"parameter": "url", "hosts": []any{"Example.com"}}}}}}, []string{"hosts"}},
		"ftp":                 {cfg{"rule": []any{cfg{"operation": "navigate", "constraint": []any{cfg{"parameter": "url", "schemes": []any{"ftp"}}}}}}, []string{"schemes"}},
		"long description":    {cfg{"description": string(make([]byte, 1025))}, []string{"description"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := h.data("browserjs_policy_document", tc.c)
			wantError(t, diags, tc.want...)
		})
	}
}
