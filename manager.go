// Manager: the thread-safe multi-version snapshot registry (the Go port of
// the RustyChickpeas manager layer). Snapshots are immutable, so the
// registry is a mutex-guarded map of shared pointers.

package chickpeas

import "sync"

// LatestVersion is the registry key AddSnapshot uses for a snapshot that
// carries no version string.
const LatestVersion = "latest"

// Manager holds named immutable snapshots.
type Manager struct {
	mu        sync.RWMutex
	snapshots map[string]*Snapshot
}

// NewManager returns an empty registry.
func NewManager() *Manager {
	return &Manager{snapshots: map[string]*Snapshot{}}
}

// AddSnapshot registers g under its own version string, or LatestVersion
// when it has none, replacing any previous snapshot under that key.
func (m *Manager) AddSnapshot(g *Snapshot) {
	version, ok := g.Version()
	if !ok || version == "" {
		version = LatestVersion
	}
	m.AddSnapshotWithVersion(version, g)
}

// AddSnapshotWithVersion registers g under an explicit version key.
func (m *Manager) AddSnapshotWithVersion(version string, g *Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[version] = g
}

// Snapshot returns the snapshot registered under version; ok is false when
// absent.
func (m *Manager) Snapshot(version string) (*Snapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.snapshots[version]
	return g, ok
}

// Versions lists the registered version keys (unordered).
func (m *Manager) Versions() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.snapshots))
	for v := range m.snapshots {
		out = append(out, v)
	}
	return out
}

// RemoveSnapshot drops the snapshot under version, reporting whether one
// was present.
func (m *Manager) RemoveSnapshot(version string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.snapshots[version]
	delete(m.snapshots, version)
	return ok
}

// Len is the number of registered snapshots.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.snapshots)
}

// Clear drops every registered snapshot.
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.snapshots)
}
