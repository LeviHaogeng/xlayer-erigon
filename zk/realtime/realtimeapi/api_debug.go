package realtimeapi

import (
	"context"
	"fmt"

	"github.com/ledgerwatch/erigon/turbo/jsonrpc"
	realtimeCache "github.com/ledgerwatch/erigon/zk/realtime/cache"
	"github.com/ledgerwatch/log/v3"
)

type RealtimeDebugApiImpl struct {
	*jsonrpc.PrivateDebugAPIImpl
	ethApi  *jsonrpc.APIImpl
	cacheDB *realtimeCache.RealtimeCache
}

func NewRealtimeDebugApiImpl(debugApi *jsonrpc.PrivateDebugAPIImpl, ethApi *jsonrpc.APIImpl, cacheDB *realtimeCache.RealtimeCache) *RealtimeDebugApiImpl {
	return &RealtimeDebugApiImpl{
		PrivateDebugAPIImpl: debugApi,
		ethApi:              ethApi,
		cacheDB:             cacheDB,
	}
}

func NewRealtimeDebugApi(debugApi *jsonrpc.PrivateDebugAPIImpl, ethApi *jsonrpc.APIImpl, cacheDB *realtimeCache.RealtimeCache) interface{} {
	return NewRealtimeDebugApiImpl(debugApi, ethApi, cacheDB)
}

func (api *RealtimeDebugApiImpl) RealtimeDumpCache(ctx context.Context) error {
	if api.cacheDB == nil || !api.cacheDB.ReadyFlag.Load() {
		// Custom for realtime
		return ErrRealtimeNotEnabled
	}

	if err := api.cacheDB.DebugDumpToFile(); err != nil {
		log.Error("[Realtime] Failed to dump state cache", "error", err)
		return fmt.Errorf("failed to dump state cache: %v", err)
	}

	return nil
}

func (api *RealtimeDebugApiImpl) RealtimeCompareStateCache(ctx context.Context) (*RealtimeDebugResult, error) {
	if api.cacheDB == nil || !api.cacheDB.ReadyFlag.Load() {
		// Custom for realtime
		return nil, ErrRealtimeNotEnabled
	}

	mismatches, err := api.cacheDB.State.DebugCompare()
	if err != nil {
		return nil, fmt.Errorf("compareStateCache cannot compare state cache: %w", err)
	}

	return &RealtimeDebugResult{
		ConfirmHeight:   api.cacheDB.GetHighestConfirmHeight(),
		ExecutionHeight: api.cacheDB.GetExecutionHeight(),
		Mismatches:      mismatches,
	}, nil
}
