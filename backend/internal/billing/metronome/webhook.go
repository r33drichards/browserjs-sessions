package metronome

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/r33drichards/computer-use/backend/internal/billing"
)

// The alert at a zero balance.
const alertType = "alerts.low_remaining_contract_credit_and_commit_balance_reached"

// How old a notification may be.
const webhookTolerance = 5 * time.Minute

// Webhook is POST /metronome/webhook: Metronome's notifications. Nobody
// signs in; the signature is the credential.
type Webhook struct {
	Accounts billing.Accounts
	Ledger   billing.Ledger
	Clock    billing.Clock
	Secret   string // METRONOME_WEBHOOK_SECRET
}

// date reads the X-Metronome-Date header, an HTTP date or RFC 3339.
func date(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, http.TimeFormat, time.RFC1123Z, time.RFC1123} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// verified checks the signature: an HMAC-SHA256, keyed by the secret, of
// the date header, a newline and the raw body, in hex.
func (h *Webhook) verified(r *http.Request, body []byte) bool {
	stamp := r.Header.Get("X-Metronome-Date")
	sent, ok := date(stamp)
	if !ok || h.Secret == "" {
		return false
	}
	if age := h.Clock.Now().Sub(sent); age > webhookTolerance || age < -webhookTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.Secret))
	mac.Write([]byte(stamp + "\n"))
	mac.Write(body)
	got, err := hex.DecodeString(r.Header.Get("Metronome-Webhook-Signature"))
	return err == nil && hmac.Equal(got, mac.Sum(nil))
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !h.verified(r, body) {
		slog.Warn("metronome webhook: bad signature or too old", "from", r.RemoteAddr)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var event struct {
		Type       string `json:"type"`
		Properties struct {
			CustomerID string `json:"customer_id"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &event); err != nil || event.Type != alertType || event.Properties.CustomerID == "" {
		w.WriteHeader(http.StatusOK) // not ours to act on
		return
	}
	acc, err := h.Accounts.ByMetronomeCustomer(r.Context(), event.Properties.CustomerID)
	if errors.Is(err, billing.ErrNotFound) {
		slog.Info("metronome webhook: no account has this customer", "customer", event.Properties.CustomerID)
		w.WriteHeader(http.StatusOK)
		return
	}
	// The event is not believed: Metronome is read again, so a replay or a
	// stale event writes what is true now.
	if err == nil {
		_, err = h.Ledger.EnsureCredit(r.Context(), acc.Name)
	}
	if err != nil {
		slog.Error("metronome webhook: not handled; Metronome retries", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
