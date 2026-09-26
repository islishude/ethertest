package ethertest

// Existing protocol fixtures explicitly retain their Osaka/Fulu rules. Amsterdam
// tests use DefaultConfig directly, so new default behavior is exercised too.
func osakaTestConfig() Config {
	cfg := DefaultConfig()
	cfg.Chain.Forks.AmsterdamEpoch = -1
	return cfg
}
