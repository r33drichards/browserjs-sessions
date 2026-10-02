// Package tokens is the API tokens: credentials a user makes for clients
// that cannot sign in through Pomerium (the Terraform provider, scripts).
//
// A token is "bjs_<id>_<secret>". The id is public and names the record; the
// secret is 32 random bytes. Only the SHA-256 of the whole string is kept,
// in an APIToken custom resource (deploy/base/crd-apitoken.yaml), so a token
// can be shown exactly once, when it is made.
package tokens

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
)

const (
	prefix    = "bjs_" // what a secret scanner looks for
	idLen     = 12
	secretLen = 43 // 32 bytes in unpadded base64url
	tokenLen  = len(prefix) + idLen + 1 + secretLen
)

const (
	idAlphabet     = "abcdefghijklmnopqrstuvwxyz234567"
	secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)

var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

// generate makes a new token from 40 bytes of random.
func generate(random io.Reader) (id, token string, err error) {
	var b [8 + 32]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", "", err
	}
	id = idEncoding.EncodeToString(b[:8])[:idLen]
	return id, prefix + id + "_" + base64.RawURLEncoding.EncodeToString(b[8:]), nil
}

func only(s, alphabet string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// ValidID reports whether id could be a token's id.
func ValidID(id string) bool { return len(id) == idLen && only(id, idAlphabet) }

// parse takes the id out of a string of a token's form.
func parse(token string) (id string, ok bool) {
	if len(token) != tokenLen || !strings.HasPrefix(token, prefix) {
		return "", false
	}
	id, secret := token[len(prefix):len(prefix)+idLen], token[len(prefix)+idLen+1:]
	if !ValidID(id) || token[len(prefix)+idLen] != '_' || !only(secret, secretAlphabet) {
		return "", false
	}
	return id, true
}

// digest is what is stored of a token: the SHA-256 of all of it, in hex.
func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
