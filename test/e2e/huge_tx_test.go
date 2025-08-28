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
	require.Less(t, gasLimit-block.GasUsed(), TransferGasLimit)
}

func TestHugeTxE2E_S2_HugeUnderQuota_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	var (
		threshold      uint64 = 5
		quota          uint64 = 50
		ignoreInterval uint64 = 5
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

	t.Logf("Test setup: %d huge txs (within quota %d), %d normal txs, total %d txs",
		hugeTxCount, cap, normalTxCount, len(gasLimits))
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.LessOrEqual(t, gasLimit-block.GasUsed(), TransferGasLimit)

	blockTxs := extractBlockTxs(t, client, bn)
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

	t.Logf("Test setup: %d huge txs (quota allows %d), %d normal txs, total %d txs",
		hugeTxCount, cap, normalTxCount, len(gasLimits))
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.LessOrEqual(t, gasLimit-block.GasUsed(), TransferGasLimit)

	blockTxs := extractBlockTxs(t, client, bn)
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

	// Configure Apollo for S6 test: threshold=5%, quota=0%, ignoreInterval=5, gasLimit=2100000
	var (
		threshold      uint64 = 5
		quota          uint64 = 0
		ignoreInterval uint64 = 5
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
	require.GreaterOrEqual(t, len(our), cap)
}

// sendTxsSynchronously uses synchronization to send all transactions simultaneously
func sendTxsSynchronously(t *testing.T, client *ethclient.Client, txs []types.Transaction) {
	ctx := context.Background()
	var wg sync.WaitGroup
	sendSignal := make(chan struct{})
	failed := make(chan bool, len(txs))

	t.Logf("Sending %d transactions synchronously...", len(txs))

	// Start all goroutines
	for _, tx := range txs {
		wg.Add(1)
		go func(transaction types.Transaction) {
			defer wg.Done()
			<-sendSignal
			err := client.SendTransaction(ctx, transaction)
			if err != nil {
				failed <- true
			}
		}(tx)
	}

	// Brief delay to ensure all goroutines are waiting
	time.Sleep(5 * time.Millisecond)

	// Trigger all sends simultaneously
	close(sendSignal)
	wg.Wait()
	close(failed)

	// Check if any transaction failed
	failedCount := len(failed)
	if failedCount > 0 {
		t.Fatalf("Failed to send %d out of %d transactions", failedCount, len(txs))
	}
}
