//go:build hugetx && !skip_smoke
// +build hugetx,!skip_smoke

package e2e

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"strings"
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
// This function includes Apollo configuration management to control transaction yielding
func buildAndSendTxs(t *testing.T, client *ethclient.Client, fromPriv string, toAddr common.Address, gasLimits []uint64, gasPricesGwei []uint64) ([]types.Transaction, []*types.Receipt) {
	// Build transactions using different keys for each tx to avoid nonce conflicts
	txs := buildTxsWithMultipleKeys(t, client, toAddr, gasLimits, gasPricesGwei)

	// Apollo configuration management
	time.Sleep(2 * time.Second) // Wait for apollo to detect config change

	// Send transactions with Apollo yielding control
	sendTxsWithApolloControl(t, client, txs)

	// Wait for all transactions to be mined
	receipts := waitForTxsToBeMined(t, client, txs)

	return txs, receipts
}

// buildTxsWithMultipleKeys builds transactions using different keys for each transaction
func buildTxsWithMultipleKeys(t *testing.T, client *ethclient.Client, toAddr common.Address, gasLimits []uint64, gasPricesGwei []uint64) []types.Transaction {
	require.Equal(t, len(gasLimits), len(gasPricesGwei))

	// Ensure test accounts are funded
	operations.EnsureAccountsFunded(t)

	ctx := context.Background()

	// Check if we need contract for high gas transactions
	var contractAddr common.Address
	needContract := false
	for _, gasLimit := range gasLimits {
		if gasLimit > TransferGasLimit {
			needContract = true
			break
		}
	}

	t.Logf("Building %d transactions with gas limits: %v and gas prices: %v (multi-key)", len(gasLimits), gasLimits, gasPricesGwei)

	if needContract {
		contractAddr = operations.GetGasConsumerContractAddress(t, client)
		t.Logf("Using gas consume contract at %s for high gas transactions", contractAddr.Hex())
	}

	var txs []types.Transaction
	for i := range gasLimits {
		// Use different private key for each transaction to avoid nonce conflicts
		keyIndex := i % operations.GetAccountKeyCount()
		pk, _ := operations.GetAccountKey(keyIndex)

		// Get nonce for this specific key
		from := crypto.PubkeyToAddress(pk.PublicKey)
		nonce, err := client.PendingNonceAt(ctx, from)
		require.NoError(t, err)

		var targetAddr common.Address
		var isContract bool
		var contractLevel uint64

		if gasLimits[i] > TransferGasLimit && needContract {
			targetAddr = contractAddr
			isContract = true
			if gasLimits[i] > HighGasLimit {
				contractLevel = 1 // Higher gas consumption
			} else {
				contractLevel = 0 // Standard gas consumption
			}
		} else {
			targetAddr = toAddr
			isContract = false
			contractLevel = 0
		}

		tx := createSingleTransaction(t, client, pk, nonce, gasLimits[i], gasPricesGwei[i], targetAddr, isContract, contractLevel)
		txs = append(txs, tx)
	}

	return txs
}

// sendTxsWithApolloControl sends transactions with Apollo yielding control
func sendTxsWithApolloControl(t *testing.T, client *ethclient.Client, txs []types.Transaction) {
	// Wait for new head before sending
	waitForNewBlockAndSendTx(t, client, func() error {
		return nil
	})

	// Step 1: Disable transaction yielding before sending transactions
	if globalApolloCtrl != nil {
		t.Logf("Disabling transaction yielding to accumulate transactions in pool...")
		globalApolloCtrl.SetHugeTxYieldEnabled(false)
		time.Sleep(2 * time.Second) // Wait for Apollo config change
	}

	// Step 2: Send all transactions simultaneously
	t.Logf("Sending %d transactions with yield disabled...", len(txs))
	sendTxsSynchronously(t, client, txs)

	// Step 3: Re-enable transaction yielding after sending
	if globalApolloCtrl != nil {
		t.Logf("Re-enabling transaction yielding to process accumulated transactions...")
		globalApolloCtrl.SetHugeTxYieldEnabled(true)
	}
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

// createSingleTransaction creates a single transaction with specified parameters
func createSingleTransaction(t *testing.T, client *ethclient.Client, pk *ecdsa.PrivateKey, nonce uint64, gasLimit uint64, gasPrice uint64, target common.Address, isContract bool, contractLevel uint64) types.Transaction {
	var tx types.Transaction

	if isContract {
		// Create gas consuming transaction
		var data []byte
		if contractLevel == 0 {
			data = common.FromHex("0xa329e8de0000000000000000000000000000000000000000000000000000000000000000") // level 0
		} else {
			data = common.FromHex("0xa329e8de0000000000000000000000000000000000000000000000000000000000000001") // level 1
		}

		tx = &types.LegacyTx{
			CommonTx: types.CommonTx{
				Nonce: nonce,
				To:    &target,
				Gas:   gasLimit,
				Value: uint256.NewInt(0),
				Data:  data,
			},
			GasPrice: uint256.NewInt(gasPrice * encoding.Gwei),
		}
	} else {
		// Create normal transfer transaction
		tx = &types.LegacyTx{
			CommonTx: types.CommonTx{
				Nonce: nonce,
				To:    &target,
				Gas:   gasLimit,
				Value: uint256.NewInt(0),
			},
			GasPrice: uint256.NewInt(gasPrice * encoding.Gwei),
		}
	}

	signer := types.MakeSigner(operations.GetTestChainConfig(operations.DefaultL2ChainID), 1, 0)
	signedTx, err := types.SignTx(tx, *signer, pk)
	require.NoError(t, err)
	return signedTx
}

// buildTxsWithoutApollo builds transactions without sending them
func buildTxsWithoutApollo(t *testing.T, client *ethclient.Client, fromPriv string, toAddr common.Address, gasLimits []uint64, gasPricesGwei []uint64) []types.Transaction {
	require.Equal(t, len(gasLimits), len(gasPricesGwei))

	// Ensure test accounts are funded before using them
	operations.EnsureAccountsFunded(t)

	ctx := context.Background()

	// Check if we need contract for high gas transactions
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

	// Get the private key for transactions
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(fromPriv, "0x"))
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(pk.PublicKey)

	// Get starting nonce
	startNonce, err := client.PendingNonceAt(ctx, from)
	require.NoError(t, err)

	var txs []types.Transaction
	for i := range gasLimits {
		var targetAddr common.Address
		var isContract bool
		var contractLevel uint64

		if gasLimits[i] > TransferGasLimit && needContract {
			targetAddr = contractAddr
			isContract = true
			if gasLimits[i] > HighGasLimit {
				contractLevel = 1 // Higher gas consumption
			} else {
				contractLevel = 0 // Standard gas consumption
			}
		} else {
			targetAddr = toAddr
			isContract = false
			contractLevel = 0
		}

		tx := createSingleTransaction(t, client, pk, startNonce+uint64(i), gasLimits[i], gasPricesGwei[i], targetAddr, isContract, contractLevel)
		txs = append(txs, tx)
	}

	return txs
}

// waitForNewBlockAndSendTx is a helper function to wait for new block before sending
func waitForNewBlockAndSendTx(t *testing.T, client *ethclient.Client, action func() error) error {
	// Wait for new head to ensure consistent timing
	wsClient, err := ethclient.Dial(operations.DefaultL2WSURL)
	require.NoError(t, err)
	defer wsClient.Close()
	waitForNewHead(t, wsClient)

	// Execute the action
	return action()
}

// sendTxsWithInterval sends transactions with specified interval between each transaction
func sendTxsWithInterval(t *testing.T, client *ethclient.Client, txs []types.Transaction, intervalMs time.Duration) {
	ctx := context.Background()

	// Wait for new head before sending first transaction
	waitForNewBlockAndSendTx(t, client, func() error {
		t.Logf("Sending %d transactions with %dms intervals...", len(txs), intervalMs/time.Millisecond)
		return nil
	})

	for i, tx := range txs {
		// Send transaction
		err := client.SendTransaction(ctx, tx)
		require.NoError(t, err, "Failed to send transaction %d", i)

		t.Logf("Sent transaction %d/%d: %s", i+1, len(txs), tx.Hash().Hex())

		// Wait before sending next transaction (except for the last one)
		if i < len(txs)-1 {
			time.Sleep(intervalMs)
		}
	}

	t.Logf("All %d transactions sent with %dms intervals", len(txs), intervalMs/time.Millisecond)
}

// waitForTxsToBeMined waits for all transactions to be mined and returns sorted receipts by block number
func waitForTxsToBeMined(t *testing.T, client *ethclient.Client, txs []types.Transaction) []*types.Receipt {
	ctx := context.Background()
	t.Logf("Waiting for %d transactions to be mined...", len(txs))

	// Wait for transactions to be mined
	var receipts []*types.Receipt
	for i, tx := range txs {
		err := operations.WaitTxToBeMined(ctx, client, tx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err, "Failed to wait for transaction %d to be mined", i)

		r, err := client.TransactionReceipt(ctx, tx.Hash())
		require.NoError(t, err, "Failed to get receipt for transaction %d", i)
		receipts = append(receipts, r)
	}

	// Sort receipts by block number to ensure consistent ordering
	return sortReceiptsByBlockNumber(t, receipts)
}

// sortReceiptsByBlockNumber sorts receipts by block number and returns them with earliest block first
func sortReceiptsByBlockNumber(t *testing.T, receipts []*types.Receipt) []*types.Receipt {
	if len(receipts) <= 1 {
		return receipts
	}

	// Find the receipt with minimum block number
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

	return receipts
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

func TestHugeTxE2E_S14_SequentialHugeTxs_SameAddress_NaturalTxpoolBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 50000
		gasLimit       uint64 = 2100000
	)

	// Initialize Apollo config but don't manipulate yielding during the test
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

	// Step 1: Build all 5 huge transactions (1 + 4) with sequential nonces
	gasLimits := []uint64{HighGasLimit, HighGasLimit, HighGasLimit, HighGasLimit, HighGasLimit}
	gasPrices := []uint64{20, 20, 20, 20, 20} // All 20 Gwei

	allTxs := buildTxsWithoutApollo(t, client, fromPriv, to, gasLimits, gasPrices)

	// Step 2: Send all transactions with 100ms intervals between each
	sendTxsWithInterval(t, client, allTxs, 800*time.Millisecond)

	// Step 3: Wait for all transactions to be mined
	allReceipts := waitForTxsToBeMined(t, client, allTxs)

	// Find the first block that contains our transactions
	var firstBlockNum *big.Int
	for _, receipt := range allReceipts {
		if firstBlockNum == nil || receipt.BlockNumber.Cmp(firstBlockNum) < 0 {
			firstBlockNum = receipt.BlockNumber
		}
	}
	require.NotNil(t, firstBlockNum, "Should have found at least one block with our transactions")

	// Get the block and analyze the transactions
	firstBlock := getBlockByNumber(t, client, firstBlockNum)
	firstBlockTxs := firstBlock.Transactions()

	// Create a map of our transaction hashes for easy lookup
	ourTxHashes := make(map[common.Hash]struct{})
	for _, tx := range allTxs {
		ourTxHashes[tx.Hash()] = struct{}{}
	}

	// Count how many of our huge transactions are in the first block
	ourHugeTxsInFirstBlock := 0
	totalHugeTxsInFirstBlock := 0

	for _, tx := range firstBlockTxs {
		isHuge := tx.GetGas() > TransferGasLimit
		if isHuge {
			totalHugeTxsInFirstBlock++
		}

		// Check if this is one of our transactions
		if _, isOurTx := ourTxHashes[tx.Hash()]; isOurTx && isHuge {
			ourHugeTxsInFirstBlock++
		}
	}

	require.Equal(t, int(gasLimit/HighGasLimit), ourHugeTxsInFirstBlock)

	require.Less(t, gasLimit-firstBlock.GasUsed(), HighGasLimit, "Block should be efficiently filled")
}
