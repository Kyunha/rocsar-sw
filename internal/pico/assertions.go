package pico

import "github.com/rocsar/obc/internal/domain"

// Compile-time proof that these types satisfy the ports they claim to.
//
// The compiler checks these; test/layering_test.go then verifies that every port
// has BOTH a real implementation and a mock, by finding these assertions. That
// is cheaper and more trustworthy than inferring satisfaction from type names --
// domain.Pico is implemented by Link, and a name-based check would miss it.
//
// If you add a third implementation, add the assertion here too. The test fails
// otherwise, which is the point: an unasserted implementation is an
// implementation nobody has checked.
var (
	_ domain.Pico = (*Link)(nil)
	_ domain.Pico = (*Mock)(nil)
)

// Link is also a Shutdown, which is how the composition root unwinds without
// type-switching on every subsystem it happens to have built.
var _ domain.Shutdown = (*Link)(nil)
