package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/stripe/stripe-go/v86"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
)

// Every call to Stripe has this long, and the SDK tries a call that failed
// on the way twice more.
const (
	callTimeout = 10 * time.Second
	callRetries = 2
)

// KeyMode is the mode ("test" or "live") a Stripe secret or restricted key
// is for, from its prefix. The key is never put in an error.
func KeyMode(key string) (string, error) {
	for prefix, mode := range map[string]string{
		"sk_test_": "test", "rk_test_": "test", "sk_live_": "live", "rk_live_": "live",
	} {
		if strings.HasPrefix(key, prefix) {
			return mode, nil
		}
	}
	return "", errors.New("not a Stripe secret key or restricted key (sk_test_, rk_test_, sk_live_, rk_live_)")
}

func newSDK(key, baseURL string) *sdk.Client {
	cfg := &sdk.BackendConfig{
		HTTPClient:        &http.Client{Timeout: callTimeout},
		MaxNetworkRetries: sdk.Int64(callRetries),
		EnableTelemetry:   sdk.Bool(false),
		// The SDK logs nothing: what fails is returned, and logged by
		// whoever called, without the request.
		LeveledLogger: &sdk.LeveledLogger{Level: sdk.LevelNull},
	}
	if baseURL != "" {
		cfg.URL = sdk.String(baseURL)
	}
	return sdk.NewClient(key, sdk.WithBackends(sdk.NewBackendsWithConfig(cfg)))
}

// API is billing.Stripe over Stripe's API, with the SDK. It is also a
// PeriodFinder.
type API struct {
	sc *sdk.Client
	// currency is what a setup Checkout is in: it charges nothing, and
	// Stripe still wants to know.
	currency string

	idsPath, mode string // IDsFile

	mu     sync.Mutex
	portal string // the portal configuration's ID, once found at Stripe
}

var (
	_ billing.Stripe = (*API)(nil)
	_ PeriodFinder   = (*API)(nil)
	_ PlanChanger    = (*API)(nil)
)

// NewAPI is the client for a key. currency is the catalogue's. baseURL is
// Stripe's, "" for the real one.
func NewAPI(key, currency, baseURL string) *API {
	return &API{sc: newSDK(key, baseURL), currency: strings.ToLower(currency)}
}

// notFound turns Stripe's "no such object" into billing.ErrNotFound.
func notFound(err error) error {
	var se *sdk.Error
	if errors.As(err, &se) && (se.Code == sdk.ErrorCodeResourceMissing || se.HTTPStatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w: %s", billing.ErrNotFound, se.Msg)
	}
	return err
}

func unix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func idOf[T any](obj *T, id func(*T) string) string {
	if obj == nil {
		return ""
	}
	return id(obj)
}

func customerID(c *sdk.Customer) string {
	return idOf(c, func(c *sdk.Customer) string { return c.ID })
}

func (a *API) CreateCustomer(ctx context.Context, p billing.CustomerParams) (string, error) {
	params := &sdk.CustomerCreateParams{
		Email:    sdk.String(p.Email),
		Metadata: map[string]string{"account": p.Account, "owner_hash": p.OwnerHash},
	}
	params.SetIdempotencyKey("customer-" + p.OwnerHash)
	c, err := a.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (a *API) CreateCheckout(ctx context.Context, p billing.CheckoutParams) (billing.CheckoutSession, error) {
	md := map[string]string{"account": p.Account, "kind": p.Kind}
	if p.Item != "" {
		md["item"] = p.Item
	}
	params := &sdk.CheckoutSessionCreateParams{
		Mode:              sdk.String(p.Mode),
		Customer:          sdk.String(p.Customer),
		ClientReferenceID: sdk.String(p.Account),
		Metadata:          md,
		SuccessURL:        sdk.String(p.SuccessURL),
		CancelURL:         sdk.String(p.CancelURL),
	}
	line := []*sdk.CheckoutSessionCreateLineItemParams{{Price: sdk.String(p.Price), Quantity: sdk.Int64(1)}}
	switch p.Mode {
	case billing.ModeSetup:
		// Nothing is charged. Stripe may place a temporary authorisation
		// to check the card.
		params.Currency = sdk.String(a.currency)
		params.SetupIntentData = &sdk.CheckoutSessionCreateSetupIntentDataParams{Metadata: md}
	case billing.ModeSubscription:
		// Subscription mode saves the payment method.
		params.LineItems = line
		params.SubscriptionData = &sdk.CheckoutSessionCreateSubscriptionDataParams{Metadata: md}
	case billing.ModePayment:
		params.LineItems = line
		params.PaymentIntentData = &sdk.CheckoutSessionCreatePaymentIntentDataParams{
			// The card is saved to the customer.
			SetupFutureUsage: sdk.String("off_session"),
			Metadata:         md,
		}
	default:
		return billing.CheckoutSession{}, fmt.Errorf("checkout mode %q", p.Mode)
	}
	cs, err := a.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return billing.CheckoutSession{}, err
	}
	return checkoutSession(cs), nil
}

func checkoutSession(cs *sdk.CheckoutSession) billing.CheckoutSession {
	return billing.CheckoutSession{
		ID: cs.ID, URL: cs.URL, Mode: string(cs.Mode), Status: string(cs.Status), PaymentStatus: string(cs.PaymentStatus),
		Customer: customerID(cs.Customer), ClientReferenceID: cs.ClientReferenceID, Metadata: cs.Metadata,
		Subscription:  idOf(cs.Subscription, func(s *sdk.Subscription) string { return s.ID }),
		PaymentIntent: idOf(cs.PaymentIntent, func(p *sdk.PaymentIntent) string { return p.ID }),
		Created:       unix(cs.Created),
	}
}

// PortalManagedBy marks the Customer Portal configuration infra/billing
// makes (metadata.managed_by). A configuration made through the API is
// never the account's default, so a portal session has to name it.
const PortalManagedBy = "stripe-setup"

// IDsFile says where infra/billing's IDs are (the ConfigMap billing-iac,
// key ids.json, mounted): the portal configuration's is taken from there.
// With no file, or none that is of this mode and names one, it is found at
// Stripe by its metadata.
func (a *API) IDsFile(path, mode string) *API {
	a.idsPath, a.mode = path, mode
	return a
}

// portalFromFile is ids.json's stripe_portal_configuration_id, "" when the
// file is not there, is of the other mode, or names none. Read each time:
// an apply that replaces the configuration rewrites the file.
func (a *API) portalFromFile() string {
	if a.idsPath == "" {
		return ""
	}
	data, err := os.ReadFile(a.idsPath)
	if err != nil {
		return ""
	}
	var ids struct {
		Mode   string `json:"mode"`
		Portal string `json:"stripe_portal_configuration_id"`
	}
	if json.Unmarshal(data, &ids) != nil || (ids.Mode != "" && ids.Mode != a.mode) {
		return ""
	}
	return ids.Portal
}

// portalConfiguration is the ID of the portal configuration infra/billing
// made, "" when there is none: the portal is then the Dashboard's default.
// From the IDs file; failing that, found at Stripe once and remembered,
// and looked for again while there is none.
func (a *API) portalConfiguration(ctx context.Context) (string, error) {
	if id := a.portalFromFile(); id != "" {
		return id, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.portal != "" {
		return a.portal, nil
	}
	params := &sdk.BillingPortalConfigurationListParams{Active: sdk.Bool(true)}
	params.Limit = sdk.Int64(100)
	for cfg, err := range a.sc.V1BillingPortalConfigurations.List(ctx, params).All(ctx) {
		if err != nil {
			return "", err
		}
		if cfg.Metadata["managed_by"] == PortalManagedBy {
			a.portal = cfg.ID
			return a.portal, nil
		}
	}
	return "", nil
}

func (a *API) CreatePortal(ctx context.Context, customer, returnURL string) (string, error) {
	params := &sdk.BillingPortalSessionCreateParams{Customer: sdk.String(customer), ReturnURL: sdk.String(returnURL)}
	configuration, err := a.portalConfiguration(ctx)
	if err != nil {
		return "", err
	}
	if configuration != "" {
		params.Configuration = sdk.String(configuration)
	}
	s, err := a.sc.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return s.URL, nil
}

func (a *API) CreateRecharge(ctx context.Context, p billing.RechargeParams) (billing.PaymentIntent, error) {
	params := &sdk.PaymentIntentCreateParams{
		Amount:        sdk.Int64(p.AmountCents),
		Currency:      sdk.String(p.Currency),
		Customer:      sdk.String(p.Customer),
		PaymentMethod: sdk.String(p.PaymentMethod),
		OffSession:    sdk.Bool(true),
		Confirm:       sdk.Bool(true),
		Metadata:      map[string]string{"account": p.Account, "kind": billing.KindRecharge, "item": p.Item},
	}
	// Which attempt of the account's it is, for its outcome to be written
	// to that attempt and no other.
	if month, seq, ok := attemptOf(p.IdempotencyKey); ok {
		params.Metadata["month"], params.Metadata["seq"] = month, strconv.Itoa(seq)
	}
	params.SetIdempotencyKey(p.IdempotencyKey)
	pi, err := a.sc.V1PaymentIntents.Create(ctx, params)
	// A card that refuses, or wants its owner, is an error that carries the
	// PaymentIntent it failed.
	var se *sdk.Error
	if errors.As(err, &se) && se.PaymentIntent != nil {
		failed := paymentIntent(se.PaymentIntent)
		if failed.DeclineCode == "" {
			failed.DeclineCode = declineCode(se)
		}
		return failed, nil
	}
	if err != nil {
		return billing.PaymentIntent{}, err
	}
	return paymentIntent(pi), nil
}

// declineCode is why a payment failed: "authentication_required" when the
// card wants its owner present, else the issuer's decline code, else
// Stripe's error code.
func declineCode(e *sdk.Error) string {
	switch {
	case e == nil:
		return ""
	case e.Code == sdk.ErrorCodeAuthenticationRequired || e.DeclineCode == "authentication_required":
		return "authentication_required"
	case e.DeclineCode != "":
		return string(e.DeclineCode)
	}
	return string(e.Code)
}

func paymentIntent(pi *sdk.PaymentIntent) billing.PaymentIntent {
	return billing.PaymentIntent{
		ID: pi.ID, Customer: customerID(pi.Customer), Status: string(pi.Status), AmountCents: pi.Amount,
		Metadata: pi.Metadata, DeclineCode: declineCode(pi.LastPaymentError), Created: unix(pi.Created),
	}
}

func (a *API) Checkout(ctx context.Context, id string) (billing.CheckoutSession, error) {
	cs, err := a.sc.V1CheckoutSessions.Retrieve(ctx, id, nil)
	if err != nil {
		return billing.CheckoutSession{}, notFound(err)
	}
	return checkoutSession(cs), nil
}

func (a *API) PaymentMethods(ctx context.Context, customer string) ([]billing.PaymentMethod, string, error) {
	c, err := a.sc.V1Customers.Retrieve(ctx, customer, nil)
	if err != nil {
		return nil, "", notFound(err)
	}
	if c.Deleted {
		return nil, "", billing.ErrNotFound
	}
	def := ""
	if c.InvoiceSettings != nil {
		def = idOf(c.InvoiceSettings.DefaultPaymentMethod, func(m *sdk.PaymentMethod) string { return m.ID })
	}
	// Every type, all pages.
	params := &sdk.CustomerListPaymentMethodsParams{Customer: sdk.String(customer)}
	params.Limit = sdk.Int64(100)
	var out []billing.PaymentMethod
	for m, err := range a.sc.V1Customers.ListPaymentMethods(ctx, params).All(ctx) {
		if err != nil {
			return nil, "", notFound(err)
		}
		pm := billing.PaymentMethod{ID: m.ID, Customer: customer, Type: string(m.Type), Created: unix(m.Created)}
		if card := m.Card; card != nil {
			pm.Fingerprint, pm.Funding, pm.Brand, pm.Last4 = card.Fingerprint, string(card.Funding), string(card.Brand), card.Last4
			pm.ExpMonth, pm.ExpYear = int(card.ExpMonth), int(card.ExpYear)
			if card.Wallet != nil {
				pm.Wallet = string(card.Wallet.Type)
			}
		}
		out = append(out, pm)
	}
	return out, def, nil
}

func subscription(s *sdk.Subscription) billing.Subscription {
	out := billing.Subscription{
		ID: s.ID, Customer: customerID(s.Customer), Status: string(s.Status), Metadata: s.Metadata, Created: unix(s.Created),
	}
	// The period is on the item since API version 2025-03-31.
	if s.Items != nil && len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		out.CurrentPeriodStart, out.CurrentPeriodEnd = unix(item.CurrentPeriodStart), unix(item.CurrentPeriodEnd)
		if item.Price != nil {
			out.PriceLookupKey = item.Price.LookupKey
		}
	}
	switch {
	case s.CancelAt != 0:
		at := unix(s.CancelAt)
		out.CancelAt = &at
	case s.CancelAtPeriodEnd && !out.CurrentPeriodEnd.IsZero():
		at := out.CurrentPeriodEnd
		out.CancelAt = &at
	}
	if inv := s.LatestInvoice; inv != nil {
		out.LatestInvoice, out.LatestInvoiceStatus = inv.ID, string(inv.Status)
	}
	return out
}

func (a *API) Subscription(ctx context.Context, id string) (billing.Subscription, error) {
	params := &sdk.SubscriptionRetrieveParams{}
	params.AddExpand("latest_invoice")
	s, err := a.sc.V1Subscriptions.Retrieve(ctx, id, params)
	if err != nil {
		return billing.Subscription{}, notFound(err)
	}
	return subscription(s), nil
}

func (a *API) PaymentIntent(ctx context.Context, id string) (billing.PaymentIntent, error) {
	pi, err := a.sc.V1PaymentIntents.Retrieve(ctx, id, nil)
	if err != nil {
		return billing.PaymentIntent{}, notFound(err)
	}
	return paymentIntent(pi), nil
}

func (a *API) Prices(ctx context.Context, lookupKeys []string) (map[string]string, error) {
	params := &sdk.PriceListParams{LookupKeys: sdk.StringSlice(lookupKeys), Active: sdk.Bool(true)}
	out := map[string]string{}
	for p, err := range a.sc.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, err
		}
		out[p.LookupKey] = p.ID
	}
	return out, nil
}

func (a *API) CancelSubscription(ctx context.Context, id string) error {
	_, err := a.sc.V1Subscriptions.Cancel(ctx, id, nil)
	return notFound(err)
}

func (a *API) DetachPaymentMethod(ctx context.Context, id string) error {
	_, err := a.sc.V1PaymentMethods.Detach(ctx, id, nil)
	return notFound(err)
}

func (a *API) Subscriptions(ctx context.Context, customer string) ([]billing.Subscription, error) {
	params := &sdk.SubscriptionListParams{Customer: sdk.String(customer), Status: sdk.String("all")}
	params.Limit = sdk.Int64(100)
	var out []billing.Subscription
	for s, err := range a.sc.V1Subscriptions.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, notFound(err)
		}
		out = append(out, subscription(s))
	}
	return out, nil
}

func (a *API) Checkouts(ctx context.Context, customer string, since time.Time) ([]billing.CheckoutSession, error) {
	params := &sdk.CheckoutSessionListParams{
		Customer:     sdk.String(customer),
		CreatedRange: &sdk.RangeQueryParams{GreaterThanOrEqual: since.Unix()},
	}
	params.Limit = sdk.Int64(100)
	var out []billing.CheckoutSession
	for cs, err := range a.sc.V1CheckoutSessions.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, notFound(err)
		}
		out = append(out, checkoutSession(cs))
	}
	return out, nil
}

func (a *API) PaymentIntents(ctx context.Context, customer string, since time.Time) ([]billing.PaymentIntent, error) {
	params := &sdk.PaymentIntentListParams{
		Customer:     sdk.String(customer),
		CreatedRange: &sdk.RangeQueryParams{GreaterThanOrEqual: since.Unix()},
	}
	params.Limit = sdk.Int64(100)
	var out []billing.PaymentIntent
	for pi, err := range a.sc.V1PaymentIntents.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, notFound(err)
		}
		out = append(out, paymentIntent(pi))
	}
	return out, nil
}

func (a *API) SubscriptionPeriodOf(ctx context.Context, paymentIntent string) (PaidPeriod, bool, error) {
	params := &sdk.InvoicePaymentListParams{
		Payment: &sdk.InvoicePaymentListPaymentParams{Type: sdk.String("payment_intent"), PaymentIntent: sdk.String(paymentIntent)},
	}
	params.AddExpand("data.invoice")
	for ip, err := range a.sc.V1InvoicePayments.List(ctx, params).All(ctx) {
		if err != nil {
			return PaidPeriod{}, false, notFound(err)
		}
		inv := ip.Invoice
		if inv == nil || inv.Parent == nil || inv.Parent.SubscriptionDetails == nil || inv.Parent.SubscriptionDetails.Subscription == nil {
			continue
		}
		period := PaidPeriod{Subscription: inv.Parent.SubscriptionDetails.Subscription.ID, Invoice: inv.ID}
		// The period paid for is the latest its lines start: an upgrade's
		// invoice also has a line giving back the old period's unused time.
		if inv.Lines != nil {
			for _, line := range inv.Lines.Data {
				if line.Period != nil && unix(line.Period.Start).After(period.Start) {
					period.Start = unix(line.Period.Start)
				}
			}
		}
		if period.Start.IsZero() {
			continue
		}
		return period, true, nil
	}
	return PaidPeriod{}, false, nil
}

// scheduleOf is the ID of the subscription's schedule, "" for none.
func scheduleOf(s *sdk.Subscription) string {
	return idOf(s.Schedule, func(sc *sdk.SubscriptionSchedule) string { return sc.ID })
}

func (a *API) UpgradeSubscription(ctx context.Context, id, price string) error {
	s, err := a.sc.V1Subscriptions.Retrieve(ctx, id, nil)
	if err != nil {
		return notFound(err)
	}
	if s.Items == nil || len(s.Items.Data) != 1 {
		return fmt.Errorf("subscription %s does not have one item", id)
	}
	item := s.Items.Data[0]
	// A change waiting for the period's end is dropped: the subscription is
	// its own again.
	if schedule := scheduleOf(s); schedule != "" {
		if _, err := a.sc.V1SubscriptionSchedules.Release(ctx, schedule, nil); err != nil {
			return err
		}
	}
	params := &sdk.SubscriptionUpdateParams{
		Items: []*sdk.SubscriptionUpdateItemParams{{ID: sdk.String(item.ID), Price: sdk.String(price)}},
		// A new period from now, invoiced and paid at once, less the unused
		// time of the old one; if the card refuses, nothing is changed.
		ProrationBehavior:     sdk.String("always_invoice"),
		BillingCycleAnchorNow: sdk.Bool(true),
		PaymentBehavior:       sdk.String("error_if_incomplete"),
	}
	// The same upgrade asked twice in one period is one upgrade.
	params.SetIdempotencyKey(fmt.Sprintf("upgrade-%s-%s-%d", id, price, item.CurrentPeriodStart))
	_, err = a.sc.V1Subscriptions.Update(ctx, id, params)
	var se *sdk.Error
	if errors.As(err, &se) && se.Type == sdk.ErrorTypeCard {
		return fmt.Errorf("%w: %s", ErrPaymentFailed, declineCode(se))
	}
	return err
}

func (a *API) SchedulePrice(ctx context.Context, id, price string) error {
	s, err := a.sc.V1Subscriptions.Retrieve(ctx, id, nil)
	if err != nil {
		return notFound(err)
	}
	schedule := scheduleOf(s)
	if price == "" {
		if schedule == "" {
			return nil
		}
		_, err := a.sc.V1SubscriptionSchedules.Release(ctx, schedule, nil)
		return err
	}
	if s.Items == nil || len(s.Items.Data) != 1 || s.Items.Data[0].Price == nil {
		return fmt.Errorf("subscription %s does not have one item with a price", id)
	}
	item := s.Items.Data[0]
	if schedule == "" {
		made, err := a.sc.V1SubscriptionSchedules.Create(ctx, &sdk.SubscriptionScheduleCreateParams{FromSubscription: sdk.String(id)})
		if err != nil {
			return err
		}
		schedule = made.ID
	}
	one := func(price string) []*sdk.SubscriptionScheduleUpdatePhaseItemParams {
		return []*sdk.SubscriptionScheduleUpdatePhaseItemParams{{Price: sdk.String(price), Quantity: sdk.Int64(1)}}
	}
	// Two phases: the period that is paid for, as it is; then the new
	// price, after which the subscription is its own again.
	_, err = a.sc.V1SubscriptionSchedules.Update(ctx, schedule, &sdk.SubscriptionScheduleUpdateParams{
		EndBehavior:       sdk.String("release"),
		ProrationBehavior: sdk.String("none"),
		Phases: []*sdk.SubscriptionScheduleUpdatePhaseParams{
			{Items: one(item.Price.ID), StartDate: sdk.Int64(item.CurrentPeriodStart), EndDate: sdk.Int64(item.CurrentPeriodEnd)},
			{Items: one(price)},
		},
	})
	return err
}
