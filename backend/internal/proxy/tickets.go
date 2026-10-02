package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ticketTTL is how long a VNC ticket can be redeemed. The UI redeems it in
// the moment it is issued.
const ticketTTL = 10 * time.Second

// tickets issues and checks the VNC tickets. A ticket is a statement signed
// with a key every replica has: "the screen of this session may be opened,
// until then, on behalf of this user". No replica keeps the tickets it
// issued, so one issues and any other redeems.
//
// Within its few seconds a ticket is redeemed once per replica: each
// remembers the tickets it has redeemed until they expire. That is all the
// "one-time" there is without a store the replicas share, and it is what a
// single replica had before. It is enough because a ticket is not a secret
// that outlives its use: it authorises opening one session's screen, it is
// issued only to a user who may (the route checks), and whoever could read
// it off that user's browser in those seconds could ask for one of their
// own with the same sign-in.
type tickets struct {
	key []byte
	now func() time.Time

	mu       sync.Mutex
	redeemed map[string]time.Time // by nonce: when the ticket expires
}

// ticket is what a redeemed ticket says.
type ticket struct {
	session string
	// user is sessions.OwnerLabel of who it was issued to: the user is
	// bound into the ticket without their address being in a URL.
	user  string
	admin bool
}

// TicketKey derives the key VNC tickets are signed with from the backend's
// signing key, so that a ticket is never a valid signature of anything else
// the key signs. It is nil for no key: the Proxy then makes one of its own,
// which no other replica has.
func TicketKey(signingKey []byte) []byte {
	if len(signingKey) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte("browserjs.dev/vnc-ticket/v1"))
	return mac.Sum(nil)
}

func newTickets(key []byte, now func() time.Time) *tickets {
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic(err)
		}
	}
	return &tickets{key: key, now: now, redeemed: map[string]time.Time{}}
}

var ticketEncoding = base64.RawURLEncoding

func (t *tickets) sign(payload string) string {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(payload))
	return ticketEncoding.EncodeToString(mac.Sum(nil))
}

// Issue makes a ticket for the session, on behalf of user (an OwnerLabel).
func (t *tickets) Issue(session, user string, admin bool) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	role := "u"
	if admin {
		role = "a"
	}
	// No field can hold a ".": a session ID, a hex digest, a letter, a
	// number, hex.
	payload := strings.Join([]string{
		session, user, role,
		strconv.FormatInt(t.now().Add(ticketTTL).UnixMilli(), 10),
		hex.EncodeToString(nonce),
	}, ".")
	return ticketEncoding.EncodeToString([]byte(payload)) + "." + t.sign(payload)
}

// Redeem checks a ticket for the session and uses it up, as far as this
// replica is concerned.
func (t *tickets) Redeem(raw, session string) (ticket, bool) {
	encoded, signature, ok := strings.Cut(raw, ".")
	if !ok {
		return ticket{}, false
	}
	payload, err := ticketEncoding.DecodeString(encoded)
	if err != nil || !hmac.Equal([]byte(signature), []byte(t.sign(string(payload)))) {
		return ticket{}, false
	}
	// Signed, so it is of the form Issue gave it.
	fields := strings.Split(string(payload), ".")
	if len(fields) != 5 {
		return ticket{}, false
	}
	ms, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return ticket{}, false
	}
	now, expires, nonce := t.now(), time.UnixMilli(ms), fields[4]
	if fields[0] != session || !expires.After(now) {
		return ticket{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, until := range t.redeemed { // tickets are short-lived; sweep on redeem
		if !until.After(now) {
			delete(t.redeemed, k)
		}
	}
	if _, used := t.redeemed[nonce]; used {
		return ticket{}, false
	}
	t.redeemed[nonce] = expires
	return ticket{session: fields[0], user: fields[1], admin: fields[2] == "a"}, true
}
