package gnss

import "github.com/rocsar/obc/internal/domain"

var (
	_ domain.GnssReceiver = (*Receiver)(nil)
	_ domain.GnssReceiver = (*Mock)(nil)
)

// Both are Shutdown: a receiver releases its socket and a bank releases its
// receivers.
var (
	_ domain.Shutdown = (*Receiver)(nil)
	_ domain.Shutdown = (*Bank)(nil)
)
