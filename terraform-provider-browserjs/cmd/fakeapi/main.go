// fakeapi serves the in-memory fake of the browserjs API on a local port, to
// try the provider with tofu or terraform before the real API exists.
//
//	go run ./cmd/fakeapi -listen 127.0.0.1:18080 -token bjs_fake_token
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/fakeapi"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "address to listen on")
	token := flag.String("token", os.Getenv("BROWSERJS_TOKEN"), "the API token to accept (default $BROWSERJS_TOKEN)")
	starting := flag.Int("session-starting-reads", 1, "reads of a new session that report it starting")
	loading := flag.Int("policy-loading-reads", 1, "reads of a written policy that report it loading (0: writes answer 200)")
	flag.Parse()
	if *token == "" {
		log.Fatal("a token is required: -token or BROWSERJS_TOKEN")
	}
	s := fakeapi.New(*token)
	s.SessionStartingReads = *starting
	s.PolicyLoadingReads = *loading
	h := s.Handler()
	log.Printf("fake browserjs API on http://%s (in memory; nothing is real)", *listen)
	log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})))
}
