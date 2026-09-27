// Package svcmw holds the decorators of inventory.Service. They are generated
// into this package with -dir, which names the directory of the interface
// relative to the module root; the generated code imports the inventory
// package and refers to the interface as inventory.Service.
package svcmw

//go:generate go tool middlegen -type=Service -dir=middlegen/inventory -kinds=uow_service,logging,tracing -service=inventory
