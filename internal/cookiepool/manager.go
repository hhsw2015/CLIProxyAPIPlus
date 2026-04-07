package cookiepool

import (
	"sync"

	log "github.com/sirupsen/logrus"
)

var (
	globalMu    sync.RWMutex
	globalPools = make(map[string]*Pool) // key: config entry name → pool
)

// Get returns the pool for the given config entry name, or nil.
func Get(name string) *Pool {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalPools[name]
}

// Register loads a cookie pool file and associates it with the config entry name.
// healthCheckURL is the base URL for zero-token cookie validation (optional, pass "" to skip).
func Register(name, filePath, healthCheckURL string) {
	if name == "" || filePath == "" {
		return
	}

	globalMu.Lock()
	defer globalMu.Unlock()

	if existing, ok := globalPools[name]; ok {
		existing.Stop()
		delete(globalPools, name)
	}

	pool, err := Load(filePath, healthCheckURL)
	if err != nil {
		log.Errorf("cookie pool: failed to load %s for %s: %v", filePath, name, err)
		return
	}
	globalPools[name] = pool
}

// StopAll stops all background watchers.
func StopAll() {
	globalMu.Lock()
	defer globalMu.Unlock()
	for name, pool := range globalPools {
		pool.Stop()
		delete(globalPools, name)
	}
}
