// fakeapi serves the in-memory fake of Metronome's API on a local port, to
// try the provider with tofu or terraform without a Metronome account.
//
//	go run ./cmd/fakeapi -listen 127.0.0.1:18090 -token fake-token
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/fakeapi"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18090", "address to listen on")
	token := flag.String("token", "", "the API token to accept")
	flag.Parse()
	if *token == "" {
		log.Fatal("a token is required: -token")
	}
	s := fakeapi.New(*token)
	s.Milliseconds = true
	h := s.Handler()
	log.Printf("fake Metronome API on http://%s (in memory; nothing is real)", *listen)
	log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		h.ServeHTTP(w, r)
	})))
}
