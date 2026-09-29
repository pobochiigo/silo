// Package greeter is the domain of the connectrpc example, laid out the way an
// SDK built on silo's typed endpoints is: the domain types and the Service
// interface live here with the endpoints and the Connect handler, and the
// Connect client in client/greeter implements the same Service, so a caller
// cannot tell a remote greeter from a local one.
package greeter

import "errors"

// GreetRequest is the business request; it knows nothing about protobuf or
// Connect.
type GreetRequest struct {
	Name string
}

// GreetResponse is the business response.
type GreetResponse struct {
	Greeting string
}

// ErrUnknownPerson is returned for the one name the service refuses.
var ErrUnknownPerson = errors.New("greeter: unknown person")
