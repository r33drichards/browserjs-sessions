package fakeapi

import (
	"context"
	"net/http"
)

type bodyKey struct{}

func withBody(ctx context.Context, b []byte) context.Context {
	return context.WithValue(ctx, bodyKey{}, b)
}

// bodyOf is the request body, read once by Handler so it can be recorded.
func bodyOf(r *http.Request) []byte {
	b, _ := r.Context().Value(bodyKey{}).([]byte)
	return b
}
