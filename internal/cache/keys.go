package cache

// Cache-key prefix constants. The exact literal values are part of the
// on-disk state.json schema (legacy cache.json carries the same prefixes):
// do NOT change them without a migration.
const (
	KeyPrefixTimeline  = "timeline:"
	KeyPrefixScheduler = "scheduler:"
	KeyPrefixStreams   = "streams:"
)
