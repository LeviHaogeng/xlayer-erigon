package txpool

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/common/fixedgas"
	"github.com/ledgerwatch/erigon-lib/common/u256"
	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/remote"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/erigon-lib/kv/temporal/temporaltest"
	"github.com/ledgerwatch/erigon-lib/txpool/txpoolcfg"
	types "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test okpay block priority txs limit - less OkPay transactions than the priority slots.
// Add 5 OkPay txs, 100 normal txs, with okpay priority limit set at 8 per block.
// Ensure that all 5 OkPay txs are obtained, and 5 normal txs are included.
func TestAddLocalTxsWithOkPayTxs1(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	_, coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	defer coreDB.Close()

	db := memdb.NewTestPoolDB(t)
	path := fmt.Sprintf("/tmp/db-test-%v", time.Now().UTC().Format(time.RFC3339Nano))
	txPoolDB := newTestTxPoolDB(t, path)
	defer txPoolDB.Close()
	aclsDB := newTestACLDB(t, path)
	defer aclsDB.Close()

	// Check if the dbs are created.
	require.NotNil(t, db)
	require.NotNil(t, txPoolDB)
	require.NotNil(t, aclsDB)

	cfg := txpoolcfg.DefaultConfig
	ethCfg := ethconfig.Defaults
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)

	// Create 5 OkPay addresses for testing
	var okPayAddresses []common.Address
	for i := 0; i < 5; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i))
		okPayAddresses = append(okPayAddresses, addr)
	}

	// Create 100 normal addresses for testing
	var normalAddresses []common.Address
	for i := 0; i < 100; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i+5))
		normalAddresses = append(normalAddresses, addr)
	}

	// Set OkPay addresses and yield gas limit configs
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList = *common.NewOrderedListOfAddresses(len(okPayAddresses))
	for _, addr := range okPayAddresses {
		ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Add(addr)
	}
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Sort()
	ethCfg.DeprecatedTxPool.OkPayBlockPriorityTxsLimit = 8

	// Create a new txpool
	pool, err := New(ch, coreDB, cfg, &ethCfg, sendersCache, *u256.N1, nil, nil, aclsDB)
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	h1 := gointerfaces.ConvertHashToH256([32]byte{})

	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}

	// Fund all addresses with 18 Ether for sending transactions
	v := make([]byte, types.EncodeSenderLengthForStorage(0, *uint256.NewInt(18 * common.Ether)))
	types.EncodeSender(0, *uint256.NewInt(18 * common.Ether), v)

	for _, addr := range okPayAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}

	for _, addr := range normalAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	// Spam the pool and add 100 normal transactions
	var normalTxSlots types.TxSlots
	for i := 0; i < 100; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i)
		normalTxSlots.Append(txSlot, normalAddresses[i][:], true)
	}
	reasons, err := pool.AddLocalTxs(ctx, normalTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	// Add 5 mock OkPay transactions to the pool
	var okPayTxSlots types.TxSlots
	for i := 0; i < 5; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i + 100)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i + 100)
		okPayTxSlots.Append(txSlot, okPayAddresses[i][:], true)
	}
	reasons, err = pool.AddLocalTxs(ctx, okPayTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	slots := types.TxsRlp{}
	// Limit to 10 yield txs
	allConditionsOk, count, err := pool.bestForXLayer(10, &slots, tx, 0, 1, 30_000_000, 0, mapset.NewSet[[32]byte]())
	assert.NoError(err)
	assert.True(allConditionsOk)

	// Check that only 10 transactions are yielded, and normal transactions are included as well
	assert.Equal(10, count)

	// Check only 8 OkPay transactions were included
	okPayCount := 0
	for _, rlpTx := range slots.Txs {
		for _, okPayTx := range okPayTxSlots.Txs {
			if bytes.Equal(rlpTx, okPayTx.Rlp) {
				okPayCount++
			}
		}
	}
	assert.Equal(5, okPayCount)
}

// Test okpay block priority txs limit - more OkPay transactions than the priority slots.
// Add 10 OkPay txs, 100 normal txs, with okpay priority limit set at 8 per block.
// Ensure that only 8 OkPay txs are obtained, and 2 normal txs are included.
func TestAddLocalTxsWithOkPayTxs2(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	_, coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	defer coreDB.Close()

	db := memdb.NewTestPoolDB(t)
	path := fmt.Sprintf("/tmp/db-test-%v", time.Now().UTC().Format(time.RFC3339Nano))
	txPoolDB := newTestTxPoolDB(t, path)
	defer txPoolDB.Close()
	aclsDB := newTestACLDB(t, path)
	defer aclsDB.Close()

	// Check if the dbs are created.
	require.NotNil(t, db)
	require.NotNil(t, txPoolDB)
	require.NotNil(t, aclsDB)

	cfg := txpoolcfg.DefaultConfig
	ethCfg := ethconfig.Defaults
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)

	// Create 10 OkPay addresses for testing
	var okPayAddresses []common.Address
	for i := 0; i < 10; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i))
		okPayAddresses = append(okPayAddresses, addr)
	}

	// Create 100 normal addresses for testing
	var normalAddresses []common.Address
	for i := 0; i < 100; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i+10))
		normalAddresses = append(normalAddresses, addr)
	}

	// Set OkPay addresses and yield gas limit configs
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList = *common.NewOrderedListOfAddresses(len(okPayAddresses))
	for _, addr := range okPayAddresses {
		ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Add(addr)
	}
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Sort()
	ethCfg.DeprecatedTxPool.OkPayBlockPriorityTxsLimit = 8

	// Create a new txpool
	pool, err := New(ch, coreDB, cfg, &ethCfg, sendersCache, *u256.N1, nil, nil, aclsDB)
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	h1 := gointerfaces.ConvertHashToH256([32]byte{})

	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}

	// Fund all addresses with 18 Ether for sending transactions
	v := make([]byte, types.EncodeSenderLengthForStorage(0, *uint256.NewInt(18 * common.Ether)))
	types.EncodeSender(0, *uint256.NewInt(18 * common.Ether), v)

	for _, addr := range okPayAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}

	for _, addr := range normalAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	// Spam the pool and add 100 normal transactions
	var normalTxSlots types.TxSlots
	for i := 0; i < 100; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i)
		normalTxSlots.Append(txSlot, normalAddresses[i][:], true)
	}
	reasons, err := pool.AddLocalTxs(ctx, normalTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	// Add 10 mock OkPay transactions to the pool
	var okPayTxSlots types.TxSlots
	for i := 0; i < 10; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i + 100)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i + 100)
		okPayTxSlots.Append(txSlot, okPayAddresses[i][:], true)
	}
	reasons, err = pool.AddLocalTxs(ctx, okPayTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	slots := types.TxsRlp{}
	// Limit to 10 yield txs
	allConditionsOk, count, err := pool.bestForXLayer(10, &slots, tx, 0, 1, 30_000_000, 0, mapset.NewSet[[32]byte]())
	assert.NoError(err)
	assert.True(allConditionsOk)

	// Check that only 10 transactions are yielded, and normal transactions are included as well
	assert.Equal(10, count)

	// Check only 8 OkPay transactions were included
	okPayCount := 0
	for _, rlpTx := range slots.Txs {
		for _, okPayTx := range okPayTxSlots.Txs {
			if bytes.Equal(rlpTx, okPayTx.Rlp) {
				okPayCount++
			}
		}
	}
	assert.Equal(8, okPayCount)
}

// Test okpay block priority txs limit - more OkPay transactions than the priority slots,
// but not many normal txs.
// Add 9 OkPay txs, 1 normal txs, with okpay priority limit set at 8 per block.
// All txs should be yielded - yield logic should be able to still include okpay txs when there are no
// more normal txs to yield.
func TestAddLocalTxsWithOkPayTxs3(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	_, coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	defer coreDB.Close()

	db := memdb.NewTestPoolDB(t)
	path := fmt.Sprintf("/tmp/db-test-%v", time.Now().UTC().Format(time.RFC3339Nano))
	txPoolDB := newTestTxPoolDB(t, path)
	defer txPoolDB.Close()
	aclsDB := newTestACLDB(t, path)
	defer aclsDB.Close()

	// Check if the dbs are created.
	require.NotNil(t, db)
	require.NotNil(t, txPoolDB)
	require.NotNil(t, aclsDB)

	cfg := txpoolcfg.DefaultConfig
	ethCfg := ethconfig.Defaults
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)

	// Create 1 normal addresses for testing
	var normalAddresses []common.Address
	for i := 0; i < 1; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i+10))
		normalAddresses = append(normalAddresses, addr)
	}

	// Create 9 OkPay addresses for testing
	var okPayAddresses []common.Address
	for i := 0; i < 10; i++ {
		addr := common.HexToAddress(fmt.Sprintf("0x%x", i))
		okPayAddresses = append(okPayAddresses, addr)
	}

	// Set OkPay addresses and yield gas limit configs
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList = *common.NewOrderedListOfAddresses(len(okPayAddresses))
	for _, addr := range okPayAddresses {
		ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Add(addr)
	}
	ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Sort()
	ethCfg.DeprecatedTxPool.OkPayBlockPriorityTxsLimit = 8

	// Create a new txpool
	pool, err := New(ch, coreDB, cfg, &ethCfg, sendersCache, *u256.N1, nil, nil, aclsDB)
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	h1 := gointerfaces.ConvertHashToH256([32]byte{})

	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}

	// Fund all addresses with 18 Ether for sending transactions
	v := make([]byte, types.EncodeSenderLengthForStorage(0, *uint256.NewInt(18 * common.Ether)))
	types.EncodeSender(0, *uint256.NewInt(18 * common.Ether), v)

	for _, addr := range okPayAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}

	for _, addr := range normalAddresses {
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}

	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	// Add 1 normal transactions
	var normalTxSlots types.TxSlots
	for i := 0; i < 1; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i)
		normalTxSlots.Append(txSlot, normalAddresses[i][:], true)
	}
	reasons, err := pool.AddLocalTxs(ctx, normalTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	// Add 9 mock OkPay transactions to the pool
	var okPayTxSlots types.TxSlots
	for i := 0; i < 9; i++ {
		txSlot := &types.TxSlot{
			Rlp:    []byte{byte(i + 1)},
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    21_000,
			Nonce:  0,
		}
		txSlot.IDHash[0] = byte(i + 1)
		okPayTxSlots.Append(txSlot, okPayAddresses[i][:], true)
	}
	reasons, err = pool.AddLocalTxs(ctx, okPayTxSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(Success, reason, reason.String())
	}

	slots := types.TxsRlp{}
	allConditionsOk, count, err := pool.bestForXLayer(10, &slots, tx, 0, 1, 30_000_000, 0, mapset.NewSet[[32]byte]())
	assert.NoError(err)
	assert.True(allConditionsOk)

	// Check that 10 transactions are yielded, and normal transactions are included as well
	assert.Equal(10, count)

	// Check all the OkPay transactions were included
	okPayCount := 0
	for _, rlpTx := range slots.Txs {
		for _, okPayTx := range okPayTxSlots.Txs {
			if bytes.Equal(rlpTx, okPayTx.Rlp) {
				okPayCount++
			}
		}
	}
	assert.Equal(9, okPayCount)
}

// TestHugeTxScenarios tests the bestForXLayer function with various huge tx scenarios
func TestHugeTxScenarios(t *testing.T) {
	mockTxBuilder := func(isHuge bool, i int, gas uint64) *types.TxSlot {
		var tip uint64
		if isHuge {
			tip = 400000
		} else {
			tip = 300000
		}
		return &types.TxSlot{
			Rlp:    []byte(fmt.Sprintf("%08x", i)),
			Tip:    *uint256.NewInt(tip + uint64(i)),
			FeeCap: *uint256.NewInt(1000000000),
			Gas:    gas,
			Nonce:  0,
		}
	}
	estimateTx := mockTxBuilder(false, 0, 0)
	estimateIntrinsicGas, _ := CalcIntrinsicGas(uint64(len(estimateTx.Rlp)), uint64(len(estimateTx.Rlp)), nil, estimateTx.Creation, true, true, false)

	normalGasLimit := fixedgas.TxGas + 1
	hugeGasLimit := fixedgas.TxGas*2 + 1

	const DEFAULT_NEXT_BLOCK_NUMBER = uint64(1)

	tests := []struct {
		name                  string
		availableGas          uint64
		maxTxs                uint16
		hugeTxConfig          ethconfig.HugeTxConfig
		normalTxGas           []uint64
		hugeTxGas             []uint64
		normalPayTxCount      uint64
		hugePayTxCount        uint64
		payTxCountLimit       uint64
		ignoreHugeTxQuota     bool
		expectedNormalTxCount int
		expectedHugeTxCount   int
		description           string
	}{
		{
			name:         "NoHugeTx_GasExhausted",
			availableGas: (estimateIntrinsicGas)*4 + 10,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      50,
				HugeTxQuotaRatio:          50,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   0,
			description:           "no huge tx, normal txs consume almost all availableGas",
		},
		{
			name:         "NoHugeTx_ReachCountLimit",
			availableGas: estimateIntrinsicGas * 10,
			maxTxs:       3,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      50,
				HugeTxQuotaRatio:          50,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 3,
			expectedHugeTxCount:   0,
			description:           "no huge tx, normal txs count reach maxTxs limit",
		},
		{
			name:         "NoHugeTx_SomePayTx_GasExhausted",
			availableGas: (estimateIntrinsicGas)*4 + 10,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      50,
				HugeTxQuotaRatio:          50,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{},
			normalPayTxCount:      2,
			hugePayTxCount:        0,
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   0,
			description:           "no huge tx, normal txs consume almost all availableGas",
		},
		{
			name:         "NoHugeTx_AllPayTx_GasExhausted",
			availableGas: (estimateIntrinsicGas)*4 + 10,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      50,
				HugeTxQuotaRatio:          50,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{},
			normalPayTxCount:      5,
			hugePayTxCount:        0,
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   0,
			description:           "no huge tx, normal txs consume almost all availableGas",
		},
		{
			name:         "NoHugeTx_AllPayTx_ReachCountLimit",
			availableGas: (estimateIntrinsicGas)*4 + 10,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      50,
				HugeTxQuotaRatio:          50,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{},
			hugePayTxCount:        0,
			normalPayTxCount:      5,
			payTxCountLimit:       3,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   0,
			description:           "no huge tx, normal txs consume almost all availableGas",
		},
		{
			name:         "SomeHugeTx_UnderQuota_GasExhausted",
			availableGas: estimateIntrinsicGas*5 + hugeGasLimit + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*5 + hugeGasLimit + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*9)*100/(estimateIntrinsicGas*5+hugeGasLimit+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 5,
			expectedHugeTxCount:   1,
			description:           "there are some huge txs, but under the quota. nomal txs can still consume out the quota that isn't used by huge txs",
		},
		{
			name:         "SomeHugeTx_UnderQuota_ReachCountLimit",
			availableGas: estimateIntrinsicGas*10 + hugeGasLimit*2 + 1,
			maxTxs:       3,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*10 + hugeGasLimit*2 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*5)*100/(estimateIntrinsicGas*10+hugeGasLimit*2+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 1,
			expectedHugeTxCount:   2,
			description:           "there are some huge txs, but under the quota. total txs count reach maxTxs limit",
		},
		{
			name:         "SomeHugeTx_UnderQuota_SomePayTx_GasExhausted",
			availableGas: estimateIntrinsicGas*6 + hugeGasLimit + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*6 + hugeGasLimit + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*6+hugeGasLimit+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        2,
			normalPayTxCount:      2,
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   3,
			description:           "there are some huge txs, but under the quota. total txs count reach maxTxs limit",
		},
		{
			name:         "SomeHugeTx_UnderQuota_SomePayTx_ReachCountLimit",
			availableGas: estimateIntrinsicGas*6 + hugeGasLimit + 1,
			maxTxs:       7,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*6 + hugeGasLimit*2 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*6+hugeGasLimit*2+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        2,
			normalPayTxCount:      2,
			payTxCountLimit:       3,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   3,
			description:           "there are some huge txs, but under the quota. total txs count reach maxTxs limit",
		},
		{
			name:         "SomeHugeTx_OverQuota_GasExhausted",
			availableGas: estimateIntrinsicGas*3 + hugeGasLimit*2 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*3 + hugeGasLimit*2 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*3+hugeGasLimit*2+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 3,
			expectedHugeTxCount:   2,
			// expectedCount:     9,
			// expectedGasUsed:   189000, // 9 * 21000 (all txs use intrinsic gas)
			description: "there are some huge txs and exceed the quota. only pack huge txs in quota, others are normal txs",
		},
		{
			name:         "SomeHugeTx_OverQuota_SomePayTx_GasExhausted",
			availableGas: estimateIntrinsicGas*6 + hugeGasLimit*2 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*6 + hugeGasLimit*2 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*6+hugeGasLimit*2+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        2,
			normalPayTxCount:      2,
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   4,
			description:           "there are some pay huge txs and exceed the quota. but they are all included ",
		},
		{
			name:         "SomeHugeTx_OverQuota_SomePayTx_ReachCountLimmit",
			availableGas: estimateIntrinsicGas*6 + hugeGasLimit*10 + 1,
			maxTxs:       6,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*6 + hugeGasLimit*10 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*6+hugeGasLimit*10+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        2,
			normalPayTxCount:      2,
			payTxCountLimit:       3,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 2,
			expectedHugeTxCount:   4,
			description:           "there are some pay huge txs and exceed the quota. but they are all included ",
		},
		{
			name:         "AllHugeTx_GasExhausted",
			availableGas: hugeGasLimit*4 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (hugeGasLimit*4 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(hugeGasLimit*4+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{}, // No normal txs
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 0,
			expectedHugeTxCount:   4,
			description:           "all txs are huge tx. availableGas will still be consumed",
		},
		{
			name:         "AllHugeTx_ReachCountLimit",
			availableGas: hugeGasLimit*10 + 1,
			maxTxs:       4,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (hugeGasLimit*10 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(hugeGasLimit*10+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{}, // No normal txs
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 0,
			expectedHugeTxCount:   4,
			description:           "all txs are huge tx. availableGas will still be consumed",
		},
		{
			name:         "AllHugeTx_SomePayTx_GasExhausted",
			availableGas: estimateIntrinsicGas*2 + hugeGasLimit*3 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*2 + hugeGasLimit*3 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*2+hugeGasLimit*3+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{}, // No normal txs
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        2,
			normalPayTxCount:      0,
			payTxCountLimit:       100,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 0,
			expectedHugeTxCount:   5,
			description:           "all txs are huge tx. availableGas will still be consumed",
		},
		{
			name:         "AllHugeTx_SomePayTx_ReachCountLimit",
			availableGas: hugeGasLimit*30 + 1,
			maxTxs:       5,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (hugeGasLimit*30 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(hugeGasLimit*30+1) + 1,
				IgnoreHugeTxQuotaInterval: DEFAULT_NEXT_BLOCK_NUMBER,
			},
			normalTxGas:           []uint64{}, // No normal txs
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			hugePayTxCount:        3,
			normalPayTxCount:      0,
			payTxCountLimit:       2,
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 0,
			expectedHugeTxCount:   5,
			description:           "all txs are huge tx. availableGas will still be consumed",
		},
		{
			name:         "IgnoreHugeTxQuota_GasExhausted",
			availableGas: estimateIntrinsicGas*9 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*9 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*9+1) + 1,
				IgnoreHugeTxQuotaInterval: 1,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     true,
			expectedNormalTxCount: 4,
			expectedHugeTxCount:   5,
			description:           "Ignore huge tx quota interval - all txs should be included",
		},
		{
			name:         "IgnoreHugeTxQuota_ReachCountLimit",
			availableGas: estimateIntrinsicGas*90 + 1,
			maxTxs:       6,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*90 + 1),
				HugeTxQuotaRatio:          (hugeGasLimit*2)*100/(estimateIntrinsicGas*90+1) + 1,
				IgnoreHugeTxQuotaInterval: 1,
			},
			normalTxGas:           []uint64{normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit, normalGasLimit},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     true,
			expectedNormalTxCount: 1,
			expectedHugeTxCount:   5,
			description:           "Ignore huge tx quota interval - all txs should be included",
		},
		{
			name:         "NoTxDefinedHuge",
			availableGas: estimateIntrinsicGas + 1,
			maxTxs:       6,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      100,
				HugeTxQuotaRatio:          1,
				IgnoreHugeTxQuotaInterval: 1,
			},
			normalTxGas:           []uint64{estimateIntrinsicGas, estimateIntrinsicGas, estimateIntrinsicGas},
			hugeTxGas:             []uint64{},
			ignoreHugeTxQuota:     true,
			expectedNormalTxCount: 1,
			expectedHugeTxCount:   0,
			description:           "Ignore huge tx quota interval - all txs should be included",
		},
		{
			name:         "Don'tLimitHugeTx",
			availableGas: hugeGasLimit*5 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (hugeGasLimit*5 + 1),
				HugeTxQuotaRatio:          100,
				IgnoreHugeTxQuotaInterval: 1,
			},
			normalTxGas:           []uint64{estimateIntrinsicGas, estimateIntrinsicGas},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 0,
			expectedHugeTxCount:   5,
			description:           "Ignore huge tx quota interval - all txs should be included",
		},
		{
			name:         "ZeroHugeTxQuota",
			availableGas: estimateIntrinsicGas*5 + 1,
			maxTxs:       100,
			hugeTxConfig: ethconfig.HugeTxConfig{
				HugeTxThresholdRatio:      hugeGasLimit * 100 / (estimateIntrinsicGas*5 + 1),
				HugeTxQuotaRatio:          0,
				IgnoreHugeTxQuotaInterval: 1,
			},
			normalTxGas:           []uint64{estimateIntrinsicGas, estimateIntrinsicGas, estimateIntrinsicGas, estimateIntrinsicGas, estimateIntrinsicGas, estimateIntrinsicGas},
			hugeTxGas:             []uint64{hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit, hugeGasLimit},
			ignoreHugeTxQuota:     false,
			expectedNormalTxCount: 5,
			expectedHugeTxCount:   0,
			description:           "Ignore huge tx quota interval - all txs should be included",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)

			// Setup test environment
			ch := make(chan types.Announcements, 100)
			_, coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
			defer coreDB.Close()

			db := memdb.NewTestPoolDB(t)
			path := fmt.Sprintf("/tmp/db-test-%v", time.Now().UTC().Format(time.RFC3339Nano))
			txPoolDB := newTestTxPoolDB(t, path)
			defer txPoolDB.Close()
			aclsDB := newTestACLDB(t, path)
			defer aclsDB.Close()

			cfg := txpoolcfg.DefaultConfig
			ethCfg := ethconfig.Defaults
			ethCfg.DeprecatedTxPool.HugeTxConfig = tt.hugeTxConfig
			sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)

			// Create addresses for testing
			var addresses []common.Address
			totalTxs := len(tt.normalTxGas) + len(tt.hugeTxGas)
			for i := 0; i < totalTxs; i++ {
				addr := common.HexToAddress(fmt.Sprintf("0x%x", i))
				addresses = append(addresses, addr)
			}

			payAddresses := make([]common.Address, 0)
			if tt.normalPayTxCount > 0 {
				payAddresses = append(payAddresses, addresses[:tt.normalPayTxCount]...)
			}
			if tt.hugePayTxCount > 0 {
				payAddresses = append(payAddresses, addresses[len(tt.normalTxGas):len(tt.normalTxGas)+int(tt.hugePayTxCount)]...)
			}
			// Set OkPay addresses and priority txs limit
			ethCfg.DeprecatedTxPool.OkPaySenderAccountsList = *common.NewOrderedListOfAddresses(len(payAddresses))
			for _, addr := range payAddresses {
				ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Add(addr)
			}
			ethCfg.DeprecatedTxPool.OkPaySenderAccountsList.Sort()
			ethCfg.DeprecatedTxPool.OkPayBlockPriorityTxsLimit = tt.payTxCountLimit

			// Create a new txpool
			pool, err := New(ch, coreDB, cfg, &ethCfg, sendersCache, *u256.N1, nil, nil, aclsDB)
			assert.NoError(err)
			require.True(pool != nil)

			ctx := context.Background()
			var stateVersionID uint64 = 0
			pendingBaseFee := uint64(200000)
			h1 := gointerfaces.ConvertHashToH256([32]byte{})

			change := &remote.StateChangeBatch{
				StateVersionId:      stateVersionID,
				PendingBlockBaseFee: pendingBaseFee,
				BlockGasLimit:       1000000,
				ChangeBatch: []*remote.StateChange{
					{BlockHeight: 0, BlockHash: h1},
				},
			}

			// Fund all addresses with 18 Ether for sending transactions
			v := make([]byte, types.EncodeSenderLengthForStorage(0, *uint256.NewInt(18 * common.Ether)))
			types.EncodeSender(0, *uint256.NewInt(18 * common.Ether), v)

			for _, addr := range addresses {
				change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
					Action:  remote.Action_UPSERT,
					Address: gointerfaces.ConvertAddressToH160(addr),
					Data:    v,
				})
			}

			tx, err := db.BeginRw(ctx)
			require.NoError(err)
			defer tx.Rollback()
			err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, tx)
			assert.NoError(err)

			// Add normal transactions
			var normalTxSlots types.TxSlots
			for i, gas := range tt.normalTxGas {
				assert.Less(gas, tt.hugeTxConfig.HugeTxThresholdRatio*tt.availableGas/100, "not a normal tx")
				txSlot := mockTxBuilder(false, i, gas)
				txSlot.IDHash[0] = byte(i)
				normalTxSlots.Append(txSlot, addresses[i][:], true)
			}
			if len(tt.normalTxGas) > 0 {
				reasons, err := pool.AddLocalTxs(ctx, normalTxSlots, tx)
				assert.NoError(err)
				for _, reason := range reasons {
					assert.Equal(Success, reason, reason.String())
				}
			}

			// Add huge transactions
			var hugeTxSlots types.TxSlots
			for i, gas := range tt.hugeTxGas {
				assert.GreaterOrEqual(gas, tt.hugeTxConfig.HugeTxThresholdRatio*tt.availableGas/100, "not a huge tx")

				txSlot := mockTxBuilder(true, i+len(tt.normalTxGas), gas)
				txSlot.IDHash[0] = byte(i + len(tt.normalTxGas))
				hugeTxSlots.Append(txSlot, addresses[i+len(tt.normalTxGas)][:], true)
			}
			if len(tt.hugeTxGas) > 0 {
				reasons, err := pool.AddLocalTxs(ctx, hugeTxSlots, tx)
				assert.NoError(err)
				for _, reason := range reasons {
					assert.Equal(Success, reason, reason.String())
				}
			}

			// Test bestForXLayer
			slots := types.TxsRlp{}
			nextBlockNumber := DEFAULT_NEXT_BLOCK_NUMBER
			if tt.ignoreHugeTxQuota {
				nextBlockNumber = tt.hugeTxConfig.IgnoreHugeTxQuotaInterval + 1 // This will trigger ignore quota
			}

			allConditionsOk, count, err := pool.bestForXLayer(tt.maxTxs, &slots, tx, 0, nextBlockNumber, tt.availableGas, 0, mapset.NewSet[[32]byte]())
			assert.NoError(err)
			assert.True(allConditionsOk)

			// Verify results
			assert.LessOrEqual(count, int(tt.maxTxs))

			// Calculate actual gas used
			// actualGasUsed := uint64(0)
			actualNormalTxCount := 0
			actualHugeTxCount := 0
			for i := 0; i < count; i++ {
				// Check if this tx is a huge tx by comparing RLP data
				isHugeTx := false
				for _, hugeTx := range hugeTxSlots.Txs {
					if bytes.Equal(slots.Txs[i], hugeTx.Rlp) {
						actualHugeTxCount++
						isHugeTx = true
						break
					}
				}
				if isHugeTx {
					continue
				}

				isNormalTx := false
				// isShanghai := pool.isShanghai()
				for _, normalTx := range normalTxSlots.Txs {
					if bytes.Equal(slots.Txs[i], normalTx.Rlp) {
						actualNormalTxCount++
						isNormalTx = true
						break
					}
				}
				assert.True(isNormalTx, "tx not found in neither huge txs nor normal txs")
			}

			// Verify gas usage is close to expected
			// assert.LessOrEqual(actualGasUsed, tt.expectedGasUsed, "actualGasUsed should be less than or equal to expectedGasUsed")
			// assert.LessOrEqual(tt.expectedGasUsed-actualGasUsed, fixedgas.TxGas)
			assert.Equal(tt.expectedNormalTxCount, actualNormalTxCount, "normal tx count mismatch for: %s", tt.description)
			assert.Equal(tt.expectedHugeTxCount, actualHugeTxCount, "huge tx count mismatch for: %s", tt.description)
			assert.Equal(tt.expectedNormalTxCount+tt.expectedHugeTxCount, count, "Transaction count mismatch for: %s", tt.description)
		})

	}
}
