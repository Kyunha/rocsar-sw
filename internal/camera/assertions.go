package camera

import "github.com/rocsar/obc/internal/domain"

var (
	_ domain.Camera = (*Capture)(nil)
	_ domain.Camera = (*Mock)(nil)
)
