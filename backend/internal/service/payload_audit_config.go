package service

// ConfigSnapshot holds a point-in-time snapshot of payload audit configuration.
// T8 task will expand this file with the full implementation.
type ConfigSnapshot struct {
	Enabled       bool
	AllGroups     bool
	GroupIDs      map[int64]struct{}
	InputMaxBytes  int
	OutputMaxBytes int
	ExcerptBytes   int
	Generation     uint64
}

// GroupInScope reports whether the given group ID falls within the audit scope.
func (s *ConfigSnapshot) GroupInScope(gid *int64) bool {
	if s == nil || !s.Enabled {
		return false
	}
	if s.AllGroups {
		return true
	}
	if gid == nil {
		return false
	}
	_, ok := s.GroupIDs[*gid]
	return ok
}
