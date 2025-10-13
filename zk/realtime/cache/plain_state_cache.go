package cache

import (
	"github.com/holiman/uint256"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/core/types/accounts"
)

type plainStateCache struct {
	accountCache         map[libcommon.Address]*accounts.Account
	deletedAccountsCache map[libcommon.Address]struct{}
	storageCache         map[string]*uint256.Int
	codeCache            map[libcommon.Hash][]byte
	incarnationMapCache  map[libcommon.Address]uint64
}

func newPlainStateCache(size int) *plainStateCache {
	return &plainStateCache{
		accountCache:         make(map[libcommon.Address]*accounts.Account, size),
		deletedAccountsCache: make(map[libcommon.Address]struct{}, size),
		storageCache:         make(map[string]*uint256.Int, size),
		codeCache:            make(map[libcommon.Hash][]byte, size),
		incarnationMapCache:  make(map[libcommon.Address]uint64, size),
	}
}

func (cache *plainStateCache) Clear() {
	for k := range cache.accountCache {
		delete(cache.accountCache, k)
	}
	for k := range cache.deletedAccountsCache {
		delete(cache.deletedAccountsCache, k)
	}
	for k := range cache.storageCache {
		delete(cache.storageCache, k)
	}
	for k := range cache.codeCache {
		delete(cache.codeCache, k)
	}
	for k := range cache.incarnationMapCache {
		delete(cache.incarnationMapCache, k)
	}
}

func (cache *plainStateCache) Flatten(incoming *plainStateCache) {
	// Apply account changes
	for address, account := range incoming.accountCache {
		delete(cache.accountCache, address)
		cache.accountCache[address] = account
		// Delete updated accounts from deleted accounts cache
		delete(cache.deletedAccountsCache, address)
	}

	for address := range incoming.deletedAccountsCache {
		cache.deletedAccountsCache[address] = struct{}{}
		delete(cache.accountCache, address)
	}

	// Apply code changes
	for codeHash, code := range incoming.codeCache {
		delete(cache.codeCache, codeHash)
		cache.codeCache[codeHash] = code
	}

	// Apply storage changes
	for key, value := range incoming.storageCache {
		delete(cache.storageCache, key)
		cache.storageCache[key] = value
	}

	// Apply incarnation map changes
	for address, incarnation := range incoming.incarnationMapCache {
		delete(cache.incarnationMapCache, address)
		cache.incarnationMapCache[address] = incarnation
	}
}
