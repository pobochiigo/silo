// Package middleware defines the generic middleware type shared by the
// generated decorators and the typed endpoint middlewares, and Chain to
// compose them. It depends on nothing but the language.
package middleware

// Middleware defines a generic wrapper type for decorating service or repository interfaces.
type Middleware[T any] func(T) T

// Chain composes middlewares into one. The first is the outermost:
// Chain(a, b, c)(next) is a(b(c(next))), so a runs first on the way in and
// last on the way out. Chain with no middlewares returns next unchanged.
func Chain[T any](mws ...Middleware[T]) Middleware[T] {
	return func(next T) T {
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		return next
	}
}
