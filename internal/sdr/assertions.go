package sdr

import "github.com/rocsar/obc/internal/domain"

var (
	_ domain.Sdr = (*Service)(nil)
	_ domain.Sdr = (*Mock)(nil)
)
