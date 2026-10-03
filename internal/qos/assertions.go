package qos

import "github.com/rocsar/obc/internal/domain"

// Compile-time proof that these types satisfy the ports they claim to. The
// compiler checks these; test/layering_test.go then verifies that every port in
// internal/domain has BOTH a real implementation and a mock.
//
// Keeping them in a file called assertions.go is a convention, not a
// requirement: it means one `grep 'var _ domain\.'` answers "what implements
// what", instead of that question requiring a search of every package.
var (
	_ domain.LinkShaper = (*Shaper)(nil)
	_ domain.LinkShaper = (*NullShaper)(nil)
)
