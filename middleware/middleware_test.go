package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type greeter func() string

func tag(name string) Middleware[greeter] {
	return func(next greeter) greeter {
		return func() string { return name + "(" + next() + ")" }
	}
}

func TestChain(t *testing.T) {
	base := greeter(func() string { return "x" })

	assert.Equal(t, "a(b(c(x)))", Chain(tag("a"), tag("b"), tag("c"))(base)(), "the first middleware is the outermost")
	assert.Equal(t, "a(x)", Chain(tag("a"))(base)())
	assert.Equal(t, "x", Chain[greeter]()(base)(), "no middlewares leaves next unchanged")
}
