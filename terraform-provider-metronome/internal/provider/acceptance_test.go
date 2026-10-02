package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/fakeapi"
)

// Acceptance tests run real plans, applies and imports with a terraform or
// tofu binary. They are skipped unless TF_ACC=1.
//
// TestAcc... run against the fake, always: whatever the environment holds,
// they never reach Metronome.
//
// TestAccLive... run against Metronome itself, and only when
// METRONOME_ACC_BEARER_TOKEN is set. The name is not the one the provider
// reads, so a token that is in the environment for another reason cannot
// start them. Use a token of a SANDBOX environment: what they create is
// archived at the end, and archived objects stay in the account for good.
//
// With OpenTofu:
//
//	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$(command -v tofu)" \
//	TF_ACC_PROVIDER_NAMESPACE=hashicorp TF_ACC_PROVIDER_HOST=registry.opentofu.org \
//	go test ./internal/provider -run TestAcc -v

const envAccToken = "METRONOME_ACC_BEARER_TOKEN"

var accProviders = map[string]func() (tfprotov6.ProviderServer, error){
	"metronome": providerserver.NewProtocol6WithError(New("acc")()),
}

// accFake points the provider at a new fake.
func accFake(t *testing.T) *fakeapi.Server {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skip("acceptance tests are skipped unless TF_ACC=1")
	}
	fake := fakeapi.New(testToken)
	fake.Milliseconds = true
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv(envEndpoint, ts.URL)
	t.Setenv(envToken, testToken)
	return fake
}

// accLive points the provider at Metronome, or skips.
func accLive(t *testing.T) {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skip("acceptance tests are skipped unless TF_ACC=1")
	}
	token := os.Getenv(envAccToken)
	if token == "" {
		t.Skip("live acceptance tests are skipped unless " + envAccToken + " holds a sandbox token")
	}
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, token)
}

func accName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tf-acc-" + hex.EncodeToString(b)
}

// accConfig is the shape of this repository's own pricing: a metric, a
// usage product that converts it, a product for credit, a rate card with a
// rate that charges only against credit, a low-balance notification and a
// custom field key.
func accConfig(name, productName string, price float64, startingAt string) string {
	return fmt.Sprintf(`
data "metronome_pricing_unit" "usd" {
  name = "USD (cents)"
}

resource "metronome_billable_metric" "awake" {
  name             = "%[1]s awake seconds"
  aggregation_type = "SUM"
  aggregation_key  = "seconds"
  event_type_filter = {
    in_values = ["%[1]s.awake"]
  }
  property_filters = [{ name = "seconds", exists = true }]
  group_keys       = [["session_id"]]
}

resource "metronome_product" "awake" {
  name               = %[2]q
  type               = "USAGE"
  billable_metric_id = metronome_billable_metric.awake.id
  tags               = ["usage"]
  quantity_conversion = {
    conversion_factor = 3600
    operation         = "DIVIDE"
  }
}

resource "metronome_product" "credit" {
  name = "%[1]s credit"
  type = "FIXED"
}

resource "metronome_rate_card" "standard" {
  name                = "%[1]s standard"
  description         = "acceptance test"
  fiat_credit_type_id = data.metronome_pricing_unit.usd.id
  aliases             = [{ name = "%[1]s-standard" }]
}

resource "metronome_rate" "awake" {
  rate_card_id = metronome_rate_card.standard.id
  product_id   = metronome_product.awake.id
  starting_at  = %[4]q
  entitled     = true
  rate_type    = "FLAT"
  price        = 0
  commit_rate = {
    rate_type = "FLAT"
    price     = %[3]v
  }
}

resource "metronome_custom_field_key" "grant_key" {
  entity             = "contract_credit"
  key                = "%[1]s_grant_key"
  enforce_uniqueness = false
}

resource "metronome_alert" "low" {
  name           = "%[1]s credit low"
  alert_type     = "low_remaining_contract_credit_and_commit_balance_reached"
  threshold      = 200
  uniqueness_key = "%[1]s-credit-low"
}
`, name, productName, price, startingAt)
}

const (
	accMetric   = "metronome_billable_metric.awake"
	accProduct  = "metronome_product.awake"
	accCredit   = "metronome_product.credit"
	accCard     = "metronome_rate_card.standard"
	accRate     = "metronome_rate.awake"
	accAlert    = "metronome_alert.low"
	accFieldKey = "metronome_custom_field_key.grant_key"
)

// accPricing takes the configuration through create, an empty plan, imports,
// an update in place and a new rate. Destroy, at the end, archives.
func accPricing(t *testing.T, check func(step string) resource.TestCheckFunc) {
	name := accName(t)
	first := accConfig(name, name+" awake time", 20, "2026-10-01T00:00:00Z")
	noop := func(names ...string) []plancheck.PlanCheck {
		var out []plancheck.PlanCheck
		for _, n := range names {
			out = append(out, plancheck.ExpectResourceAction(n, plancheck.ResourceActionNoop))
		}
		return out
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProviders,
		Steps: []resource.TestStep{
			{
				Config: first,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(accMetric, "id", uuidPattern),
					resource.TestCheckResourceAttrPair(accProduct, "billable_metric_id", accMetric, "id"),
					resource.TestCheckResourceAttr(accCard, "fiat_credit_type_id", client.USDCreditTypeID),
					resource.TestCheckResourceAttr(accRate, "credit_type_id", client.USDCreditTypeID),
					resource.TestCheckResourceAttr(accRate, "price", "0"),
					resource.TestCheckResourceAttr(accRate, "commit_rate.price", "20"),
					resource.TestMatchResourceAttr(accAlert, "id", uuidPattern),
					check("created"),
				),
			},
			{
				Config:           first,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{ResourceName: accMetric, ImportState: true, ImportStateVerify: true},
			{ResourceName: accProduct, ImportState: true, ImportStateVerify: true},
			{ResourceName: accCredit, ImportState: true, ImportStateVerify: true},
			{ResourceName: accCard, ImportState: true, ImportStateVerify: true},
			{ResourceName: accRate, ImportState: true, ImportStateVerify: true},
			{ResourceName: accFieldKey, ImportState: true, ImportStateVerify: true},
			{
				// A new name for the product: in place, nothing else touched.
				Config: accConfig(name, name+" awake hours", 20, "2026-10-01T00:00:00Z"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(
					noop(accMetric, accCredit, accCard, accRate, accAlert, accFieldKey),
					plancheck.ExpectResourceAction(accProduct, plancheck.ResourceActionUpdate),
				)},
				Check: resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttr(accProduct, "name", name+" awake hours"), check("renamed")),
			},
			{
				// A new price from a later moment: a new rate, the product
				// and the rate card as they were.
				Config: accConfig(name, name+" awake hours", 25, "2026-11-01T00:00:00Z"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: append(
					noop(accMetric, accProduct, accCredit, accCard, accAlert, accFieldKey),
					plancheck.ExpectResourceAction(accRate, plancheck.ResourceActionReplace),
				)},
				Check: resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttr(accRate, "commit_rate.price", "25"), check("repriced")),
			},
		},
	})
}

func TestAccPricing(t *testing.T) {
	fake := accFake(t)
	ids := map[string]string{}
	accPricing(t, func(step string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttrWith(accCard, "id", func(id string) error {
				ids["card"] = id
				want := map[string]int{"created": 1, "renamed": 1, "repriced": 2}[step]
				if n := len(fake.Rates(id)); n != want {
					return fmt.Errorf("%s: %d rates on the schedule, want %d", step, n, want)
				}
				return nil
			}),
			resource.TestCheckResourceAttrWith(accProduct, "id", func(id string) error { ids["product"] = id; return nil }),
			resource.TestCheckResourceAttrWith(accMetric, "id", func(id string) error { ids["metric"] = id; return nil }),
			resource.TestCheckResourceAttrWith(accAlert, "id", func(id string) error { ids["alert"] = id; return nil }),
		)
	})
	// After the destroy everything is archived, and nothing is gone.
	if m, ok := fake.Metric(ids["metric"]); !ok || m.ArchivedAt == "" {
		t.Errorf("the metric after destroy: %+v, %v", m, ok)
	}
	if p, ok := fake.Product(ids["product"]); !ok || p.ArchivedAt == "" {
		t.Errorf("the product after destroy: %+v, %v", p, ok)
	}
	if archived, ok := fake.RateCardArchived(ids["card"]); !ok || !archived {
		t.Errorf("the rate card after destroy: archived %v, exists %v", archived, ok)
	}
	if got := fake.AlertStatus(ids["alert"]); got != "archived" {
		t.Errorf("the notification after destroy: %q", got)
	}
	if n := len(fake.Rates(ids["card"])); n != 2 {
		t.Errorf("%d rates after destroy, want the 2 that were added", n)
	}
}

// TestAccLivePricing is the same journey against Metronome. It leaves five
// archived objects (and two rates on an archived rate card) in the
// environment the token belongs to.
func TestAccLivePricing(t *testing.T) {
	accLive(t)
	accPricing(t, func(string) resource.TestCheckFunc { return func(*terraform.State) error { return nil } })
}
