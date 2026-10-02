package tokens

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/browserjs-sessions/backend/internal/auth"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// GVR is the APIToken custom resource.
var GVR = schema.GroupVersionResource{Group: "browserjs.dev", Version: "v1alpha1", Resource: "apitokens"}

// How often, at most, a token's last use is written down.
const lastUsedEvery = time.Hour

// Token is a token's record. The token itself is not in it.
type Token struct {
	ID       string     `json:"id"`
	Owner    string     `json:"owner"`
	Name     string     `json:"name"`
	Scopes   []string   `json:"scopes"`
	Created  time.Time  `json:"created"`
	Expires  time.Time  `json:"expires"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	// Session is the one session the token is for, "" for all its owner's.
	Session string `json:"session_id,omitempty"`

	sha256 string
}

// Store keeps tokens as APIToken custom resources. It holds no token and no
// state of its own that matters: a restart loses nothing.
type Store struct {
	client dynamic.ResourceInterface
	now    func() time.Time
	random io.Reader

	// signer makes and checks the access tokens API tokens are exchanged
	// for; nil for none (oauth.go).
	signer *Signer

	mu      sync.Mutex
	touched map[string]time.Time // when each token's last use was last written
}

func NewStore(client dynamic.Interface, namespace string) *Store {
	return &Store{
		client:  client.Resource(GVR).Namespace(namespace),
		now:     time.Now,
		random:  rand.Reader,
		touched: map[string]time.Time{},
	}
}

func objectName(id string) string { return "tok-" + id }

func normalOwner(owner string) string { return strings.ToLower(strings.TrimSpace(owner)) }

func fromObject(obj *unstructured.Unstructured) (Token, error) {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	t := Token{Created: obj.GetCreationTimestamp().Time}
	t.ID, _ = spec["id"].(string)
	t.Owner, _ = spec["owner"].(string)
	t.Name, _ = spec["name"].(string)
	t.sha256, _ = spec["sha256"].(string)
	t.Session, _ = spec["session"].(string)
	t.Scopes, _, _ = unstructured.NestedStringSlice(obj.Object, "spec", "scopes")
	expires, _ := spec["expiresAt"].(string)
	var err error
	if t.Expires, err = time.Parse(time.RFC3339, expires); err != nil {
		return Token{}, fmt.Errorf("%s: expiresAt: %w", obj.GetName(), err)
	}
	if !ValidID(t.ID) || obj.GetName() != objectName(t.ID) || t.Owner == "" || len(t.sha256) != 64 ||
		(t.Session != "" && !sessions.ValidID(t.Session)) {
		return Token{}, fmt.Errorf("%s: not a well-formed APIToken", obj.GetName())
	}
	if used, _, _ := unstructured.NestedString(obj.Object, "status", "lastUsedTime"); used != "" {
		if at, err := time.Parse(time.RFC3339, used); err == nil {
			t.LastUsed = &at
		}
	}
	return t, nil
}

// Create makes a token for owner and returns it: the only time it exists
// outside its owner's hands. session is the one session it is for, "" for
// all of owner's.
func (s *Store) Create(ctx context.Context, owner, name string, scopes []string, session string, life time.Duration) (Token, string, error) {
	owner = normalOwner(owner)
	list := make([]any, len(scopes))
	for i, scope := range scopes {
		list[i] = scope
	}
	expires := s.now().Add(life).UTC().Truncate(time.Second)
	for range 3 { // an id already taken: 60 random bits, so never twice
		id, token, err := generate(s.random)
		if err != nil {
			return Token{}, "", err
		}
		spec := map[string]any{
			"id": id, "owner": owner, "name": name, "scopes": list,
			"expiresAt": expires.Format(time.RFC3339), "sha256": digest(token),
		}
		if session != "" {
			spec["session"] = session
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GVR.Group + "/" + GVR.Version,
			"kind":       "APIToken",
			"metadata": map[string]any{
				"name":   objectName(id),
				"labels": map[string]any{sessions.LabelOwner: sessions.OwnerLabel(owner)},
			},
			"spec": spec,
		}}
		created, err := s.client.Create(ctx, obj, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return Token{}, "", err
		}
		t, err := fromObject(created)
		if err != nil {
			return Token{}, "", err
		}
		if t.Created.IsZero() {
			t.Created = s.now().UTC().Truncate(time.Second)
		}
		return t, token, nil
	}
	return Token{}, "", errors.New("could not find an unused token id")
}

func (s *Store) list(ctx context.Context, selector string) ([]Token, error) {
	objs, err := s.client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	tokens := make([]Token, 0, len(objs.Items))
	for i := range objs.Items {
		t, err := fromObject(&objs.Items[i])
		if err != nil {
			slog.Warn("skipping an APIToken", "err", err)
			continue
		}
		tokens = append(tokens, t)
	}
	// Newest first.
	sort.Slice(tokens, func(i, j int) bool {
		if !tokens[i].Created.Equal(tokens[j].Created) {
			return tokens[i].Created.After(tokens[j].Created)
		}
		return tokens[i].ID < tokens[j].ID
	})
	return tokens, nil
}

// List returns owner's tokens, expired ones included.
func (s *Store) List(ctx context.Context, owner string) ([]Token, error) {
	owner = normalOwner(owner)
	all, err := s.list(ctx, labels.SelectorFromSet(labels.Set{sessions.LabelOwner: sessions.OwnerLabel(owner)}).String())
	if err != nil {
		return nil, err
	}
	// The label is a hash, cut short; the owner is what counts.
	mine := all[:0]
	for _, t := range all {
		if t.Owner == owner {
			mine = append(mine, t)
		}
	}
	return mine, nil
}

// ListAll returns everybody's tokens.
func (s *Store) ListAll(ctx context.Context) ([]Token, error) { return s.list(ctx, "") }

// Get returns the token with id; found is false when there is none.
func (s *Store) Get(ctx context.Context, id string) (t Token, found bool, err error) {
	if !ValidID(id) {
		return Token{}, false, nil
	}
	obj, err := s.client.Get(ctx, objectName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, err
	}
	if t, err = fromObject(obj); err != nil {
		return Token{}, false, err
	}
	return t, true, nil
}

// Delete revokes the token with id: the next request made with it is
// refused. Deleting one that is not there is not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	if !ValidID(id) {
		return nil
	}
	if err := s.client.Delete(ctx, objectName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	s.mu.Lock()
	delete(s.touched, id)
	s.mu.Unlock()
	return nil
}

// noDigest stands in for the hash of a token that does not exist, so that
// refusing one costs the same comparison as refusing a wrong secret.
var noDigest = digest("")

// check is the whole of what makes an API token good: it is of a token's
// form, its record exists, the hash of all of it is the record's, and it
// has not expired. Every request reads the record from the cluster, so a
// revoked or expired token stops working at once.
func (s *Store) check(ctx context.Context, token string) (Token, error) {
	id, ok := parse(token)
	if !ok {
		return Token{}, auth.ErrInvalidToken
	}
	t, err := s.record(ctx, id)
	stored := noDigest
	if err == nil {
		stored = t.sha256
	}
	// Hash against hash, in constant time: how long this takes says nothing
	// about how much of a guess was right.
	same := subtle.ConstantTimeCompare([]byte(digest(token)), []byte(stored)) == 1
	if err != nil {
		return Token{}, err
	}
	if !same {
		return Token{}, auth.ErrInvalidToken
	}
	return t, nil
}

// record reads the record of the token with id: ErrInvalidToken if there is
// none, or it is malformed, or it has expired.
func (s *Store) record(ctx context.Context, id string) (Token, error) {
	obj, err := s.client.Get(ctx, objectName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Token{}, auth.ErrInvalidToken
	}
	if err != nil {
		return Token{}, err
	}
	t, err := fromObject(obj)
	if err != nil {
		slog.Warn("refusing an APIToken", "err", err)
		return Token{}, auth.ErrInvalidToken
	}
	if !s.now().Before(t.Expires) {
		return Token{}, auth.ErrInvalidToken
	}
	return t, nil
}

// VerifyToken is auth.TokenVerifier: token is an API token, or an access
// token one was exchanged for (oauth.go).
func (s *Store) VerifyToken(ctx context.Context, token string) (string, auth.TokenInfo, error) {
	if !strings.HasPrefix(token, prefix) {
		return s.verifyAccessToken(ctx, token)
	}
	t, err := s.check(ctx, token)
	if err != nil {
		return "", auth.TokenInfo{}, err
	}
	s.touch(ctx, t)
	return t.Owner, auth.TokenInfo{Name: t.Name, Scopes: t.Scopes, Session: t.Session}, nil
}

// touch writes down that t was used, unless that was done within the hour.
// It is for show (the token page), so a failure is logged and nothing more.
func (s *Store) touch(ctx context.Context, t Token) {
	now := s.now()
	if t.LastUsed != nil && now.Sub(*t.LastUsed) < lastUsedEvery {
		return
	}
	s.mu.Lock()
	at, tried := s.touched[t.ID]
	if tried && now.Sub(at) < lastUsedEvery {
		s.mu.Unlock()
		return
	}
	s.touched[t.ID] = now
	s.mu.Unlock()

	patch, _ := json.Marshal(map[string]any{"status": map[string]any{
		"lastUsedTime": now.UTC().Truncate(time.Second).Format(time.RFC3339),
	}})
	// The caller hanging up must not stop it half-way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.client.Patch(ctx, objectName(t.ID), types.MergePatchType, patch, metav1.PatchOptions{}, "status"); err != nil {
		slog.Warn("could not record an API token's last use", "token", t.ID, "err", err)
	}
}
