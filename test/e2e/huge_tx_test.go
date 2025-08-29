//go:build hugetx && !skip_smoke
// +build hugetx,!skip_smoke

package e2e

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/ledgerwatch/erigon/test/operations"
	"github.com/ledgerwatch/erigon/zkevm/encoding"
	"github.com/stretchr/testify/require"
)

// Huge Tx E2E tests. each test assumes YAML config has been set before run
// Keys:
// - txpool.huge-tx-threshold-ratio
// - txpool.huge-tx-quota-ratio
// - txpool.ignore-huge-tx-quota-interval

const (
	VeryHighGasLimit = uint64(2650000)
	HighGasLimit     = uint64(480000)
	TransferGasLimit = uint64(21000)
)

// Global Apollo configuration controller for all tests
var globalApolloCtrl *operations.ApolloConfigController

// initApolloConfig initializes the global Apollo configuration controller
func initApolloConfig(t *testing.T) {
	if globalApolloCtrl == nil {
		globalApolloCtrl = operations.NewApolloConfigController(t)
	}
}

// cleanupApolloConfig cleans up the global Apollo configuration controller
func cleanupApolloConfig() {
	if globalApolloCtrl != nil {
		globalApolloCtrl.Close()
		globalApolloCtrl = nil
	}
}

// waitForNewHead waits for the next newly mined block header before proceeding.
// This ensures transactions are sent right after a block is produced, avoiding
// sending them right before the next block is sealed.
func waitForNewHead(t *testing.T, client *ethclient.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch := make(chan *types.Header, 1)
	sub, err := client.SubscribeNewHead(ctx, ch)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	select {
	case <-ctx.Done():
		require.FailNow(t, "timeout waiting for new head")
	case err := <-sub.Err():
		require.NoError(t, err)
	case <-ch:
		// received the next head
	}
}

// buildAndSendTxs builds transactions with provided gas limits and gas prices (in Gwei),
// signs them using different private keys for each transaction, sends, waits until mined, and returns txs and receipts
// For high gas limits (>= 100k), it creates contract calls that actually consume the gas
// This function now includes Apollo configuration management to control transaction yielding
func buildAndSendTxs(t *testing.T, client *ethclient.Client, fromPriv string, toAddr common.Address, gasLimits []uint64, gasPricesGwei []uint64) ([]types.Transaction, []*types.Receipt) {
	require.Equal(t, len(gasLimits), len(gasPricesGwei))

	// Ensure test accounts are funded before using them
	operations.EnsureAccountsFunded(t)

	ctx := context.Background()

	// Deploy gas consumer contract for high gas transactions
	var contractAddr common.Address
	needContract := false
	for _, gasLimit := range gasLimits {
		if gasLimit > TransferGasLimit {
			needContract = true
			break
		}
	}

	t.Logf("Building %d transactions with gas limits: %v and gas prices: %v", len(gasLimits), gasLimits, gasPricesGwei)

	if needContract {
		contractAddr = operations.GetGasConsumerContractAddress(t, client)
		t.Logf("Using gas consume contract at %s for high gas transactions", contractAddr.Hex())
	}

	var txs []types.Transaction
	for i := range gasLimits {
		// Use different private key for each transaction to avoid nonce conflicts
		keyIndex := i % operations.GetAccountKeyCount()
		pk, _ := operations.GetAccountKey(keyIndex)

		var tx types.Transaction

		if gasLimits[i] > TransferGasLimit && needContract {
			// Create gas consuming transaction for high gas limits
			tx = operations.CreateGasConsumeTx(t, client, contractAddr, pk, gasLimits[i], gasPricesGwei[i], 0)
		} else if gasLimits[i] > HighGasLimit && needContract {
			tx = operations.CreateGasConsumeTx(t, client, contractAddr, pk, gasLimits[i], gasPricesGwei[i], 1)
		} else {
			// Create normal transfer transaction for low gas limits
			from := crypto.PubkeyToAddress(pk.PublicKey)
			nonce, err := client.PendingNonceAt(ctx, from)
			require.NoError(t, err)

			gp := uint256.NewInt(gasPricesGwei[i] * encoding.Gwei)
			tx = &types.LegacyTx{
				CommonTx: types.CommonTx{
					Nonce: nonce,
					To:    &toAddr,
					Gas:   gasLimits[i],
					Value: uint256.NewInt(0),
				},
				GasPrice: gp,
			}

			signer := types.MakeSigner(operations.GetTestChainConfig(operations.DefaultL2ChainID), 1, 0)
			signedTx, err := types.SignTx(tx, *signer, pk)
			require.NoError(t, err)
			tx = signedTx
		}

		txs = append(txs, tx)
	}

	time.Sleep(2 * time.Second) // Wait for apollo to detect config change (with 1s SyncServerTimeout)

	// send txs after a new block is produced to make sure they are included in the same block
	// (since we have set a long seal time, the txs will be sent right after the block is produced)
	wsClient, err := ethclient.Dial(operations.DefaultL2WSURL)
	require.NoError(t, err)
	defer wsClient.Close()
	waitForNewHead(t, wsClient)

	// Step 1: Disable transaction yielding before sending transactions
	// This ensures transactions accumulate in the pool for proper testing
	if globalApolloCtrl != nil {
		t.Logf("Disabling transaction yielding to accumulate transactions in pool...")
		globalApolloCtrl.SetHugeTxYieldEnabled(false)
		// Wait for Apollo's long polling cycle (2s) + processing time
		time.Sleep(2 * time.Second) // Ensure config change is detected and propagated
	}

	// Step 2: Send all transactions simultaneously
	t.Logf("Sending %d transactions with yield disabled...", len(txs))
	sendTxsSynchronously(t, client, txs)

	// Step 3: Re-enable transaction yielding after sending
	// This allows the txpool to process the accumulated transactions
	if globalApolloCtrl != nil {
		t.Logf("Re-enabling transaction yielding to process accumulated transactions...")
		globalApolloCtrl.SetHugeTxYieldEnabled(true)
	}

	var receipts []*types.Receipt
	for _, tx := range txs {
		err := operations.WaitTxToBeMined(ctx, client, tx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err)
		r, err := client.TransactionReceipt(ctx, tx.Hash())
		require.NoError(t, err)
		receipts = append(receipts, r)
	}

	// Sort receipts by block number to ensure we return the first block
	if len(receipts) > 1 {
		minBlockNum := receipts[0].BlockNumber
		minIndex := 0
		for i, receipt := range receipts {
			if receipt.BlockNumber.Cmp(minBlockNum) < 0 {
				minBlockNum = receipt.BlockNumber
				minIndex = i
			}
		}
		// Swap the earliest receipt to index 0 for consistent test logic
		if minIndex != 0 {
			receipts[0], receipts[minIndex] = receipts[minIndex], receipts[0]
			t.Logf("Found first block with test transactions: %s (swapped receipt order)", minBlockNum.String())
		}
	}

	return txs, receipts
}

// extractBlockTxs fetches a block by number and returns its transactions
func extractBlockTxs(t *testing.T, client *ethclient.Client, blockNum *big.Int) []types.Transaction {
	ctx := context.Background()
	blk, err := client.BlockByNumber(ctx, blockNum)
	require.NoError(t, err)
	return blk.Transactions()
}

// getBlockByNumber returns the full block for the given block number
func getBlockByNumber(t *testing.T, client *ethclient.Client, number *big.Int) *types.Block {
	ctx := context.Background()
	blk, err := client.BlockByNumber(ctx, number)
	require.NoError(t, err)
	return blk
}

// checkHugeTxsPrioritized verifies that huge transactions appear in the first few positions
func checkHugeTxsPrioritized(t *testing.T, blockTxs []types.Transaction, expectedHugeCount int, testName string) {
	hugeTxsInFront := 0
	for i := 0; i < len(blockTxs) && i < expectedHugeCount; i++ { // Check first few positions
		if blockTxs[i].GetGas() > TransferGasLimit {
			hugeTxsInFront++
		}
	}

	t.Logf("%s: Found %d huge txs in first %d positions", testName, hugeTxsInFront, expectedHugeCount)

	require.GreaterOrEqual(t, hugeTxsInFront, expectedHugeCount,
		"%s: Huge transactions should be prioritized and appear in front positions", testName)
}

// checkNormalTxAtPosition verifies that the transaction at specified position is a normal transaction
func checkNormalTxAtPosition(t *testing.T, blockTxs []types.Transaction, position int, testName string) {
	require.Greater(t, len(blockTxs), position-1, "%s: Block should contain at least %d transactions", testName, position)

	targetTx := blockTxs[position-1] // Convert 1-based position to 0-based index
	isTargetTxNormal := targetTx.GetGas() <= TransferGasLimit

	t.Logf("%s: Transaction at position %d gas limit: %d (normal: %v)",
		testName, position, targetTx.GetGas(), isTargetTxNormal)

	require.True(t, isTargetTxNormal,
		"%s: Transaction at position %d should be normal (gas <= %d), but got gas = %d",
		testName, position, TransferGasLimit, targetTx.GetGas())
}

// sendTxsSynchronously uses synchronization to send all transactions and wait for them to enter pending pool
func sendTxsSynchronously(t *testing.T, client *ethclient.Client, txs []types.Transaction) {
	ctx := context.Background()
	var wg sync.WaitGroup
	failed := make(chan bool, len(txs))

	t.Logf("Sending %d transactions synchronously...", len(txs))

	// Start all goroutines to send transactions
	for _, tx := range txs {
		wg.Add(1)
		go func(transaction types.Transaction) {
			defer wg.Done()
			err := client.SendTransaction(ctx, transaction)
			if err != nil {
				failed <- true
				return
			}

			// Wait for transaction to appear in pending pool
			txHash := transaction.Hash()
			maxRetries := 5
			for i := 0; i < maxRetries; i++ {
				_, pending, err := client.TransactionByHash(ctx, txHash)
				if err == nil && pending {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}

			// If we reach here, transaction didn't appear in pending pool within timeout
			t.Logf("Warning: Transaction %s didn't appear in pending pool within 50ms", txHash.Hex())
			failed <- true
		}(tx)
	}

	wg.Wait()
	close(failed)

	// Check if any transaction failed
	failedCount := len(failed)
	if failedCount > 0 {
		t.Fatalf("Failed to send %d out of %d transactions or they didn't enter pending pool", failedCount, len(txs))
	}

	t.Logf("All %d transactions successfully entered pending pool", len(txs))
}

func TestHugeTxE2E_S1_NoHuge_AllOrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 10
		ignoreInterval uint64 = 5
		gasLimit       uint64 = 2100000
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	// threshold=5 -> below hugeMin
	var gasLimits []uint64
	var gasPrices []uint64
	// Limit to 100 transactions
	for i := 0; i < 100; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, uint64(100-i))
	}
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit, "Block should be efficiently filled")
}

func TestHugeTxE2E_S2_HugeUnderQuota_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit) // How many contract calls can fit in quota
	if cap > 1 {
		cap = cap - 1
	} else {
		cap = 1
	}

	// Limit total transactions to 100
	totalTxs := 100
	hugeTxCount := cap // huge within quota
	normalTxCount := totalTxs - hugeTxCount

	if hugeTxCount > totalTxs {
		hugeTxCount = totalTxs
		normalTxCount = 0
	}

	for i := 0; i < hugeTxCount; i++ { // huge within quota
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(20-i)) // Higher gas prices for huge txs (10+ Gwei)
	}
	for i := 0; i < normalTxCount; i++ { // normals
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, uint64(5-i/20)) // Lower gas prices for normal txs (5 Gwei)
	}

	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit, "Block should be efficiently filled")

	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (appear in front positions)
	checkHugeTxsPrioritized(t, blockTxs, hugeTxCount, "S2_HugeUnderQuota")

	// verify all huge txs are included in this block
	hugeSet := make(map[[32]byte]struct{})
	for i := 0; i < cap; i++ {
		hugeSet[txs[i].Hash()] = struct{}{}
	}
	for _, btx := range blockTxs {
		if _, ok := hugeSet[btx.Hash()]; ok {
			delete(hugeSet, btx.Hash())
		}
	}
	require.Equal(t, 0, len(hugeSet), "not all huge txs included in the block")
}

func TestHugeTxE2E_S3_HugeOverQuota_LimitHugeCount(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)
	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit) // How many contract calls can fit in quota
	if cap < 1 {
		cap = 1
	}

	// Limit total transactions to 100 to work with our test keys
	totalTxs := 100
	hugeTxCount := cap + 10                 // exceed quota
	normalTxCount := totalTxs - hugeTxCount // remaining for normal txs

	if hugeTxCount > totalTxs {
		hugeTxCount = totalTxs
		normalTxCount = 0
	}

	// Add huge transactions with higher gas prices
	// Use 600k gas limit to accommodate consumeGas(0) which uses ~495k gas
	for i := 0; i < hugeTxCount; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(20-i)) // Higher gas prices for huge txs (10+ Gwei)
	}

	// Add normal transactions with lower gas prices
	for i := 0; i < normalTxCount; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, uint64(5-i/20)) // Lower gas prices for normal txs (5 Gwei)
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit, "Block should be efficiently filled")

	blockTxs := extractBlockTxs(t, client, bn)

	checkHugeTxsPrioritized(t, blockTxs, cap, "S3_HugeOverQuota_LimitHugeCount")

	hugeCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		}
	}
	require.LessOrEqual(t, hugeCnt, cap)
}

func TestHugeTxE2E_S4_AllHuge_ExceedQuota_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)
	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit)
	for i := 0; i < cap+5; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(20-i))
	}
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (all should be huge, ordered by price)
	checkHugeTxsPrioritized(t, blockTxs, int(gasLimit/HighGasLimit), "S4_AllHuge_ExceedQuota")

	hugeCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		}
	}
	require.Greater(t, hugeCnt, cap, "huge tx count should be greater than cap")
}

func TestHugeTxE2E_S5_AllHuge_Quota100_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 100
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit) // How many contract calls can fit in quota
	if cap < 1 {
		cap = 1
	}
	for i := 0; i < cap; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(20-i))
	}
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (all should be huge, ordered by price)
	checkHugeTxsPrioritized(t, blockTxs, cap, "S5_AllHuge_Quota100")

	var our []types.Transaction
	m := make(map[[32]byte]uint64)
	for i := range txs {
		m[txs[i].Hash()] = gasPrices[i]
	}
	for _, tx := range blockTxs {
		if _, ok := m[tx.Hash()]; ok {
			our = append(our, tx)
		}
	}
	require.Equal(t, len(our), cap, "huge tx count should be equal to cap")
}

func TestHugeTxE2E_S6_Quota0_NormalFirst_ThenHuge(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 0
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	transferPerBlock := int(gasLimit / TransferGasLimit)
	countNormals := transferPerBlock - 1
	for i := 0; i < countNormals; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, 10)
	}
	gasLimits = append(gasLimits, HighGasLimit)
	gasPrices = append(gasPrices, 20)
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	firstBlock := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, firstBlock)

	// Check that normal transactions are prioritized (first tx should be normal)
	checkNormalTxAtPosition(t, blockTxs, 1, "S6_Quota0_NormalFirst")

	isHugeInFirst := false
	lastTxHash := txs[len(txs)-1].Hash()
	for _, tx := range blockTxs {
		if tx.Hash() == lastTxHash && tx.GetGas() >= TransferGasLimit {
			isHugeInFirst = true
			break
		}
	}
	require.False(t, isHugeInFirst)
}

func TestHugeTxE2E_S7_IgnoreInterval0_Mix_HugeOverQuotaAllowed_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 0
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit)

	for i := 0; i < 5; i++ { // huge
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, 20-uint64(i))
	}
	for i := 0; i < 5; i++ { // normal
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, 5-uint64(i/20))
	}
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (higher price: 20-16 vs 5-4)
	checkHugeTxsPrioritized(t, blockTxs, int(gasLimit/HighGasLimit), "S7_IgnoreInterval0_Mix")

	var our []types.Transaction
	m := make(map[[32]byte]uint64)
	for i := range txs {
		m[txs[i].Hash()] = gasPrices[i]
	}
	for _, tx := range blockTxs {
		if _, ok := m[tx.Hash()]; ok {
			our = append(our, tx)
		}
	}
	require.Greater(t, len(our), cap)
}

func TestHugeTxE2E_S8_HugeOverQuota_NormalPartialFill_HugeFillsRemainder(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 25
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	quotaGas := gasLimit * quota / 100
	cap := int(quotaGas / HighGasLimit) // How many huge txs fit in quota

	// 40 normal transactions (partial fill - doesn't use full block)
	normalTxCount := 40
	hugeTxCount := cap + 3 // Exceed quota by 3 transactions

	// Add normal transactions with moderate gas price
	for i := 0; i < normalTxCount; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, 15) // Moderate gas price
	}

	// Add huge transactions with high gas price (should be selected first)
	for i := 0; i < hugeTxCount; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(25-i)) // High gas prices for huge txs
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)

	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (higher price: 25-22 vs 15)
	checkHugeTxsPrioritized(t, blockTxs, 2, "S8_HugeOverQuota_NormalPartialFill")

	hugeCnt := 0
	normalCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		} else {
			normalCnt++
		}
	}

	t.Logf("Block results: %d normal txs, %d huge txs, gas used: %d/%d",
		normalCnt, hugeCnt, block.GasUsed(), gasLimit)

	// Should have more huge txs than quota allows due to remaining gas
	require.Greater(t, hugeCnt, cap, "Should have more huge txs than quota due to remaining gas")
	// Should have all normal txs (since they don't fill the block)
	require.Equal(t, normalTxCount, normalCnt, "Should include all normal transactions")
}

func TestHugeTxE2E_S9_Quota100_AllNormal_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 100
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	// Add 100 normal transactions with varying gas prices
	for i := 0; i < 100; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, uint64(100-i)) // Decreasing gas prices
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber

	blockTxs := extractBlockTxs(t, client, bn)
	hugeCnt := 0
	normalCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		} else {
			normalCnt++
		}
	}

	require.Equal(t, normalCnt, 100, "Should include all normal transactions")
}

func TestHugeTxE2E_S10_Quota100_Mix_NormalAndHuge_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 100
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	// Add huge transactions with high gas prices
	for i := 0; i < 5; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, 5)
	}

	// Add normal transactions with lower gas prices
	for i := 0; i < 95; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, 5)
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber

	blockTxs := extractBlockTxs(t, client, bn)
	hugeCnt := 0
	normalCnt := 0
	totalGasFromHuge := uint64(0)
	totalGasFromNormal := uint64(0)

	block := getBlockByNumber(t, client, bn)
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
			totalGasFromHuge += tx.GetGas()
		} else {
			normalCnt++
			totalGasFromNormal += tx.GetGas()
		}
	}

	require.Greater(t, hugeCnt, 0, "Should have some huge transactions")
	require.Greater(t, normalCnt, 0, "Should have some normal transactions")
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit, "Block should be efficiently filled")
}

func TestHugeTxE2E_S11_Quota0_IgnoreInterval0_Mix(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 0
		ignoreInterval uint64 = 0
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	// Add 50 normal transactions
	for i := 0; i < 50; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, uint64(5))
	}

	// Add 3 huge transactions with higher prices
	for i := 0; i < 3; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(10))
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (higher price: 10 vs 5, ignoreInterval=0 overrides quota=0%)
	checkHugeTxsPrioritized(t, blockTxs, 3, "S11_Quota0_IgnoreInterval0")

	hugeCnt := 0
	normalCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		} else {
			normalCnt++
		}
	}

	require.Equal(t, 3, hugeCnt)
	require.Greater(t, normalCnt, 0, "Should include some normal transactions")
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit, "Block should be efficiently filled")
}

func TestHugeTxE2E_S12_Quota0_IgnoreInterval0_AllHuge(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 0
		ignoreInterval uint64 = 0
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	// Calculate how many huge transactions can fit
	cap := int(gasLimit / HighGasLimit)
	hugeTxCount := cap + 2 // Slightly exceed capacity

	// Add only huge transactions
	for i := 0; i < hugeTxCount; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, uint64(30-i)) // Descending prices
	}

	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	blockTxs := extractBlockTxs(t, client, bn)

	// Check that huge transactions are prioritized (all huge, ordered by price)
	checkHugeTxsPrioritized(t, blockTxs, cap, "S12_Quota0_IgnoreInterval0_AllHuge")

	hugeCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
		}
	}
	require.Equal(t, cap, hugeCnt)
	require.Less(t, gasLimit-block.GasUsed(), HighGasLimit, "Block should be efficiently filled")
}

func TestHugeTxE2E_S13_Quota0_NoGasLeft_ForHuge(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 0
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
		gasLimits      []uint64
		gasPrices      []uint64
	)

	initApolloConfig(t)
	defer cleanupApolloConfig()
	if globalApolloCtrl != nil {
		globalApolloCtrl.SetHugeTxConfigWithGasLimit(threshold, quota, ignoreInterval, gasLimit)
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	normalTxCount := 80
	for i := 0; i < normalTxCount; i++ {
		gasLimits = append(gasLimits, TransferGasLimit)
		gasPrices = append(gasPrices, 5)
	}

	hugeTxCount := 3
	for i := 0; i < hugeTxCount; i++ {
		gasLimits = append(gasLimits, HighGasLimit)
		gasPrices = append(gasPrices, 10)
	}

	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	blockTxs := extractBlockTxs(t, client, bn)

	// Create a map to track gas prices by transaction hash
	priceMap := make(map[[32]byte]uint64)
	for i := range txs {
		priceMap[txs[i].Hash()] = gasPrices[i]
	}

	hugeCnt := 0
	normalCnt := 0
	totalHugeGasPrice := uint64(0)
	totalNormalGasPrice := uint64(0)

	for _, tx := range blockTxs {
		txPrice := priceMap[tx.Hash()]
		if tx.GetGas() > TransferGasLimit {
			hugeCnt++
			totalHugeGasPrice += txPrice
		} else {
			normalCnt++
			totalNormalGasPrice += txPrice
		}
	}

	require.Equal(t, uint64(0), totalHugeGasPrice, "quota=0%% should prevent huge transactions despite higher gas prices")
	require.Equal(t, normalTxCount, normalCnt, "Should include all normal transactions")
	require.Equal(t, 0, hugeCnt, "Should have no huge transactions")
	require.Less(t, gasLimit-block.GasUsed(), HighGasLimit, "Block should be efficiently filled")
}
