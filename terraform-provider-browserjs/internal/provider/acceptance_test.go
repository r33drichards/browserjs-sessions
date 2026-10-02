package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/fakeapi"
)

// Acceptance tests run real plans and applies with a terraform or tofu
// binary. They are skipped unless TF_ACC=1.
//
// With BROWSERJS_ENDPOINT and BROWSERJS_TOKEN set they run against that API
// (a local deployment: they create and delete sessions, so never production).
// Without them they run against the fake.
//
// With OpenTofu:
//
//	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$(command -v tofu)" \
//	TF_ACC_PROVIDER_NAMESPACE=hashicorp TF_ACC_PROVIDER_HOST=registry.opentofu.org \
//	go test ./internal/provider -run TestAcc -v

var accProviders = map[string]func() (tfprotov6.ProviderServer, error){
	"browserjs": providerserver.NewProtocol6WithError(New("acc")()),
}

// accAPI is the API the acceptance tests use: nil when it is a real one.
func accAPI(t *testing.T) *fakeapi.Server {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skip("acceptance tests are skipped unless TF_ACC=1")
	}
	if os.Getenv(envEndpoint) != "" && os.Getenv(envToken) != "" {
		t.Logf("acceptance tests against %s", os.Getenv(envEndpoint))
		return nil
	}
	fake := fakeapi.New(testToken)
	fake.SessionStartingReads = 1
	fake.PolicyLoadingReads = 1
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv(envEndpoint, ts.URL)
	t.Setenv(envToken, testToken)
	return fake
}

func accName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tf-acc-" + hex.EncodeToString(b)
}

func accConfig(name, policy string) string {
	return fmt.Sprintf(`
resource "browserjs_session" "test" {
  name = %q
}

resource "browserjs_session_policy" "test" {
  session_id  = browserjs_session.test.id
  managed_url = %q
%s
}
`, name, managedURL, policy)
}

// accRego is a rego argument: source as an indented heredoc.
func accRego(source string) string {
	return "  rego = <<-EOT\n    " + strings.ReplaceAll(strings.TrimRight(source, "\n"), "\n", "\n    ") + "\n  EOT"
}

var (
	accNoScripting = accRego(noScripting)
	accObserveOnly = accRego(observeOnly)
	accBrowserOnly = accRego(browserOnly)
	accBypassable  = accRego(bypassable)
	accBrokenRego  = accRego("package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call if {")
)

func TestAccSessionAndPolicy(t *testing.T) {
	accAPI(t)
	name := accName(t)
	const session, policy = "browserjs_session.test", "browserjs_session_policy.test"
	var sessionIDs []string
	recordSession := func(s *terraform.State) error {
		sessionIDs = append(sessionIDs, s.RootModule().Resources[session].Primary.ID)
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProviders,
		Steps: []resource.TestStep{
			{
				Config: accConfig(name, accNoScripting),
				Check: resource.ComposeAggregateTestCheckFunc(
					recordSession,
					resource.TestMatchResourceAttr(session, "id", sessionID),
					resource.TestCheckResourceAttr(session, "name", name),
					resource.TestCheckResourceAttrSet(session, "mcp_url"),
					resource.TestCheckResourceAttrSet(session, "owner"),
					resource.TestCheckResourceAttrPair(policy, "id", session, "id"),
					resource.TestCheckResourceAttr(policy, "state", "ready"),
					resource.TestCheckResourceAttr(policy, "wait_for_ready", "true"),
					resource.TestCheckResourceAttrSet(policy, "version"),
					resource.TestCheckResourceAttrSet(policy, "hash"),
					resource.TestMatchResourceAttr(policy, "compiled_rego", regexp.MustCompile(`package browserjs\.policy`)),
				),
			},
			{
				// The same policy again is not a change.
				Config: accConfig(name, accNoScripting),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A policy edit updates the policy in place and leaves the
				// session alone.
				Config: accConfig(name, accObserveOnly),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(policy, plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction(session, plancheck.ResourceActionNoop),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(recordSession, resource.TestCheckResourceAttr(policy, "state", "ready")),
			},
			{ResourceName: policy, ImportState: true, ImportStateVerify: true},
			{ResourceName: session, ImportState: true, ImportStateVerify: true},
			{
				// Another edit, still in place.
				Config: accConfig(name, accBrowserOnly),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(policy, plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction(session, plancheck.ResourceActionNoop),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					recordSession,
					resource.TestCheckResourceAttrPair(policy, "compiled_rego", policy, "rego"),
				),
			},
			{ResourceName: policy, ImportState: true, ImportStateVerify: true},
			{
				// A renamed session is the same session.
				Config: accConfig(name+"-renamed", accBrowserOnly),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(session, plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction(policy, plancheck.ResourceActionNoop),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(recordSession, resource.TestCheckResourceAttr(session, "name", name+"-renamed")),
			},
			{
				// An invalid policy fails the plan, with a line and a column.
				Config:      accConfig(name+"-renamed", accBrokenRego),
				ExpectError: regexp.MustCompile(`(?s)Invalid policy.*rego line \d+, column \d+: `),
			},
			{
				// A policy that restricts the browser and leaves the desktop
				// and the shell open is applied; the API warns about it.
				Config: accConfig(name+"-renamed", accBypassable),
				Check:  resource.ComposeAggregateTestCheckFunc(recordSession, resource.TestCheckResourceAttr(policy, "state", "ready")),
			},
		},
	})

	for _, id := range sessionIDs {
		if id != sessionIDs[0] {
			t.Fatalf("the session was replaced along the way: %v", sessionIDs)
		}
	}
}

func TestAccDataSources(t *testing.T) {
	accAPI(t)
	name := accName(t)
	config := fmt.Sprintf(`
resource "browserjs_session" "test" {
  name = %q
}

data "browserjs_session" "by_id" {
  id = browserjs_session.test.id
}

data "browserjs_session" "by_name" {
  name       = %q
  depends_on = [browserjs_session.test]
}

# The policy of a session that is looked up, not managed here.
resource "browserjs_session_policy" "test" {
  session_id  = data.browserjs_session.by_name.id
  managed_url = %q
%s
}

data "browserjs_sessions" "all" {
  depends_on = [browserjs_session.test]
}
`, name, name, managedURL, accNoScripting)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProviders,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("browserjs_session_policy.test", "state", "ready"),
					resource.TestCheckResourceAttrPair("browserjs_session_policy.test", "id", "browserjs_session.test", "id"),
					resource.TestCheckResourceAttrPair("data.browserjs_session.by_id", "mcp_url", "browserjs_session.test", "mcp_url"),
					resource.TestCheckResourceAttrPair("data.browserjs_session.by_name", "id", "browserjs_session.test", "id"),
					resource.TestMatchResourceAttr("data.browserjs_sessions.all", "sessions.#", regexp.MustCompile(`^[1-9]\d*$`)),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// "Manage here instead" in the UI shows up as drift, and the next apply
// takes the policy back. Only the fake can press that button.
func TestAccPolicyDriftAgainstTheFake(t *testing.T) {
	fake := accAPI(t)
	if fake == nil {
		t.Skip("needs the fake API: a real one has no way to act as the UI")
	}
	name := accName(t)
	const policy = "browserjs_session_policy.test"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProviders,
		Steps: []resource.TestStep{
			{Config: accConfig(name, accNoScripting)},
			{
				PreConfig: func() { fake.UIManageHere(fake.IDByName(name)) },
				Config:    accConfig(name, accNoScripting),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(policy, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if m := fake.PolicyOf(fake.IDByName(name)).Management; m.Mode != "iac" || m.ManagedURL != managedURL {
						return fmt.Errorf("the apply did not take the policy back: %+v", m)
					}
					return nil
				},
			},
			{
				// An edit in the UI is undone too.
				PreConfig: func() {
					if err := fake.UIEdit(fake.IDByName(name), observeOnly); err != nil {
						t.Fatal(err)
					}
				},
				Config: accConfig(name, accNoScripting),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(policy, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if p := fake.PolicyOf(fake.IDByName(name)); p.Management.Mode != "iac" || p.Source == observeOnly {
						return fmt.Errorf("the apply did not put the policy back: %+v", p)
					}
					return nil
				},
			},
		},
	})
}

// A session that predates policies cannot be given one.
func TestAccPolicyUnsupportedAgainstTheFake(t *testing.T) {
	fake := accAPI(t)
	if fake == nil {
		t.Skip("needs the fake API: a real one cannot make a session that predates policies")
	}
	id := fake.AddLegacySession("old")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProviders,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "browserjs_session_policy" "test" {
  session_id  = %q
  managed_url = %q
%s
}`, id, managedURL, accNoScripting),
			ExpectError: regexp.MustCompile(`(?s)The session predates policies.*Recreate the session`),
		}},
	})
}
