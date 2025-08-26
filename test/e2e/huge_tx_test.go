//go:build hugetx && !skip_smoke
// +build hugetx,!skip_smoke

package e2e

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/fixedgas"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/ledgerwatch/erigon/test/operations"
	"github.com/ledgerwatch/erigon/zkevm/encoding"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

// Huge Tx E2E tests. each test assumes YAML config has been set before run
// Keys:
// - txpool.huge-tx-threshold-ratio
// - txpool.huge-tx-quota-ratio
// - txpool.ignore-huge-tx-quota-interval

// getLatestBlockGasLimit returns the gas limit of the latest block header
func getLatestBlockGasLimit(t *testing.T, client *ethclient.Client) uint64 {
	ctx := context.Background()
	header, err := client.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	return header.GasLimit
}

// readDynamicBlockGasLimitFromSeqConfig reads zkevm.dynamic-block-gas-limit from test/config/test.erigon.seq.config.yaml.
// If the key is not present or file cannot be read/parsed, returns utils.DynamicBlockGasLimit.Value as default.
func readDynamicBlockGasLimitFromSeqConfig(t *testing.T) uint64 {
	defaultVal := utils.DynamicBlockGasLimit.Value

	if wd, err := os.Getwd(); err == nil {
		t.Logf("cwd=%s", wd)
	}

	path := "test/config/test.erigon.seq.config.yaml"
	data, err := ioutil.ReadFile(path)
	if err != nil {
		// fallback to parent relative path if tests are invoked from subpackages
		path = "../config/test.erigon.seq.config.yaml"
		data, err = ioutil.ReadFile(path)
		require.NoError(t, err)
	}

	m := make(map[string]interface{})
	if err := yaml.Unmarshal(data, &m); err != nil {
		return defaultVal
	}

	if v, ok := m["zkevm.dynamic-block-gas-limit"]; ok {
		switch vv := v.(type) {
		case int:
			return uint64(vv)
		case int64:
			return uint64(vv)
		case uint64:
			return vv
		case float64:
			return uint64(vv)
		case string:
			var parsed uint64
			_, scanErr := fmt.Sscanf(vv, "%d", &parsed)
			if scanErr == nil {
				return parsed
			}
		}
	}
	return defaultVal
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

// buildAndSendTxs builds legacy transactions with provided gas limits and gas prices (in Gwei),
// signs them using the provided private key, sends, waits until mined, and returns txs and receipts
func buildAndSendTxs(t *testing.T, client *ethclient.Client, fromPriv string, toAddr common.Address, gasLimits []uint64, gasPricesGwei []uint64) ([]types.Transaction, []*types.Receipt) {
	require.Equal(t, len(gasLimits), len(gasPricesGwei))

	ctx := context.Background()

	// derive sender address from private key for nonce acquisition
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(fromPriv, "0x"))
	require.NoError(t, err)
	from := crypto.PubkeyToAddress(pk.PublicKey)

	nonce, err := client.PendingNonceAt(ctx, from)
	require.NoError(t, err)

	signer := types.MakeSigner(operations.GetTestChainConfig(operations.DefaultL2ChainID), 1, 0)

	var txs []types.Transaction
	for i := range gasLimits {
		gp := uint256.NewInt(gasPricesGwei[i] * encoding.Gwei)
		var tx types.Transaction = &types.LegacyTx{CommonTx: types.CommonTx{Nonce: nonce + uint64(i), To: &toAddr, Gas: gasLimits[i], Value: uint256.NewInt(0)}, GasPrice: gp}
		signed, err := types.SignTx(tx, *signer, pk)
		require.NoError(t, err)
		txs = append(txs, signed)
	}

	// send txs after a new block is produced to make sure they are included in the same block
	// (since we have set a long seal time, the txs will be sent right after the block is produced)
	wsClient, err := ethclient.Dial("ws://127.0.0.1:8547")
	require.NoError(t, err)
	defer wsClient.Close()
	waitForNewHead(t, wsClient)
	for _, tx := range txs {
		go require.NoError(t, client.SendTransaction(ctx, tx))
	}

	var receipts []*types.Receipt
	for _, tx := range txs {
		err := operations.WaitTxToBeMined(ctx, client, tx, operations.DefaultTimeoutTxToBeMined)
		require.NoError(t, err)
		r, err := client.TransactionReceipt(ctx, tx.Hash())
		require.NoError(t, err)
		receipts = append(receipts, r)
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

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := readDynamicBlockGasLimitFromSeqConfig(t)
	// threshold=5 -> below hugeMin
	normGas := gl*5/100 - 1
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < 1000; i++ {
		gasLimits = append(gasLimits, normGas)
		gasPrices = append(gasPrices, uint64(1000-i))
	}
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gl-block.GasUsed(), fixedgas.TxGas)
}

func TestHugeTxE2E_S2_HugeUnderQuota_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := readDynamicBlockGasLimitFromSeqConfig(t)
	hugeMin := gl*5/100 + 1
	hugeGas := hugeMin
	quotaGas := gl * 50 / 100
	cap := int(quotaGas / hugeGas)
	if cap > 1 {
		cap = cap - 1
	} else {
		cap = 1
	}

	normalTxCount := 900
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < cap; i++ { // huge within quota
		gasLimits = append(gasLimits, hugeGas)
		gasPrices = append(gasPrices, uint64(normalTxCount+i))
	}
	for i := 0; i < normalTxCount; i++ { // normals
		gasLimits = append(gasLimits, 21000)
		gasPrices = append(gasPrices, uint64(normalTxCount-i))
	}
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gl-block.GasUsed(), fixedgas.TxGas)

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

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := readDynamicBlockGasLimitFromSeqConfig(t)
	hugeMin := gl*5/100 + 1
	hugeGas := hugeMin
	quotaGas := gl * 50 / 100
	cap := int(quotaGas / hugeGas)
	if cap < 1 {
		cap = 1
	}

	normalTxCount := 900
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < cap+10; i++ { // exceed quota
		gasLimits = append(gasLimits, hugeGas)
		gasPrices = append(gasPrices, uint64(normalTxCount+i))
	}

	for i := 0; i < normalTxCount; i++ {
		gasLimits = append(gasLimits, 21000)
		gasPrices = append(gasPrices, uint64(normalTxCount-i))
	}
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	block := getBlockByNumber(t, client, bn)
	require.Less(t, gl-block.GasUsed(), fixedgas.TxGas)

	blockTxs := extractBlockTxs(t, client, bn)
	hugeCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() >= hugeGas {
			hugeCnt++
		}
	}
	require.LessOrEqual(t, hugeCnt, cap)
}

func TestHugeTxE2E_S4_AllHuge_ExceedQuota_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := getLatestBlockGasLimit(t, client)
	hugeMin := gl * 5 / 100
	hugeGas := hugeMin
	quotaGas := gl * 50 / 100
	cap := int(quotaGas / hugeGas)
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < cap+5; i++ {
		gasLimits = append(gasLimits, hugeGas)
		gasPrices = append(gasPrices, uint64(60-i))
	}
	_, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	bn := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, bn)
	hugeCnt := 0
	for _, tx := range blockTxs {
		if tx.GetGas() >= hugeGas {
			hugeCnt++
		}
	}
	require.Greater(t, hugeCnt, cap)
}

func TestHugeTxE2E_S5_AllHuge_Quota100_OrderByPrice(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := getLatestBlockGasLimit(t, client)
	hugeMin := gl * 5 / 100
	hugeGas := hugeMin
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < 6; i++ {
		gasLimits = append(gasLimits, hugeGas)
		gasPrices = append(gasPrices, uint64(40-i))
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
	require.GreaterOrEqual(t, len(our), 3)
	for i := 1; i < len(our); i++ {
		require.GreaterOrEqual(t, m[our[i-1].Hash()], m[our[i].Hash()])
	}
}

func TestHugeTxE2E_S6_Quota0_NormalFirst_ThenHuge(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := getLatestBlockGasLimit(t, client)
	hugeMin := gl * 5 / 100
	hugeGas := hugeMin
	normPerBlock := int(gl / 21000)
	countNormals := normPerBlock
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < countNormals; i++ {
		gasLimits = append(gasLimits, 21000)
		gasPrices = append(gasPrices, 30)
	}
	gasLimits = append(gasLimits, hugeGas)
	gasPrices = append(gasPrices, 100)
	txs, receipts := buildAndSendTxs(t, client, fromPriv, to, gasLimits, gasPrices)
	firstBlock := receipts[0].BlockNumber
	blockTxs := extractBlockTxs(t, client, firstBlock)
	isHugeInFirst := false
	lastTxHash := txs[len(txs)-1].Hash()
	for _, tx := range blockTxs {
		if tx.Hash() == lastTxHash && tx.GetGas() >= hugeGas {
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

	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	to := common.HexToAddress(operations.DefaultL2NewAcc1Address)
	fromPriv := operations.DefaultL2AdminPrivateKey

	gl := getLatestBlockGasLimit(t, client)
	hugeMin := gl * 5 / 100
	hugeGas := hugeMin
	var gasLimits []uint64
	var gasPrices []uint64
	for i := 0; i < 5; i++ { // huge
		gasLimits = append(gasLimits, hugeGas)
		gasPrices = append(gasPrices, 100-uint64(i))
	}
	for i := 0; i < 5; i++ { // normal
		gasLimits = append(gasLimits, 21000)
		gasPrices = append(gasPrices, 50-uint64(i))
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
	require.GreaterOrEqual(t, len(our), 3)
	for i := 1; i < len(our); i++ {
		require.GreaterOrEqual(t, m[our[i-1].Hash()], m[our[i].Hash()])
	}
}
