package coordinator

import "github.com/cosmoformer/cosmoformer/internal/runs"

func verifyProviderLock(m *runs.Manifest, b []byte) error { return runs.VerifyProviderLock(m, b) }
