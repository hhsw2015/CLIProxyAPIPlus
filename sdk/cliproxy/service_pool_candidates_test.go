package cliproxy

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestServicePoolCandidatesConcurrentAccess(t *testing.T) {
	cfg := config.PoolManagerConfig{Size: 2, Provider: "codex"}
	service := &Service{
		poolManager:    NewPoolManager(cfg),
		poolMetrics:    NewPoolMetrics(cfg),
		poolCandidates: map[string]*coreauth.Auth{},
	}

	service.storePoolCandidate(testCodexAuthWithRemaining("seed", 80))

	start := make(chan struct{})
	var wg sync.WaitGroup

	writer := func(prefix string) {
		defer wg.Done()
		<-start
		for i := 0; i < 2_000; i++ {
			auth := testCodexAuthWithRemaining(fmt.Sprintf("%s-%d", prefix, i), i%100)
			service.storePoolCandidate(auth)
			runtime.Gosched()
		}
	}

	reader := func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2_000; i++ {
			service.poolCandidateCounts()
			service.poolMetricsSnapshotCurrent()
			service.poolObservedState("seed")
			runtime.Gosched()
		}
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go writer(fmt.Sprintf("writer-%d", i))
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go reader()
	}

	close(start)
	wg.Wait()
}
