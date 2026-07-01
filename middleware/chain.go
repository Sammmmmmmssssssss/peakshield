package middleware

import "net/http"

// Middleware defines a standard idiomatic Go middleware signature.
type Middleware func(http.Handler) http.Handler

// Chain builds an http.Handler by wrapping a target handler with a sequence of Middlewares.
// The middlewares are applied in reverse order so that the first middleware in the slice
// is the first one executed when an HTTP request arrives.
//
// Execution flow for Chain(target, m1, m2, m3):
// Request -> m1 -> m2 -> m3 -> target -> m3 (response) -> m2 -> m1 -> Client
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}
