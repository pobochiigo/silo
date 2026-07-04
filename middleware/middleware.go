package middleware

// Middleware defines a generic wrapper type for decorating service or repository interfaces.
type Middleware[T any] func(T) T
