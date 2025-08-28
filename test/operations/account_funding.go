package operations

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/stretchr/testify/require"
)

var (
	testPrivateKeys []string
	testAddresses   []common.Address
	testECDSAKeys   []*ecdsa.PrivateKey
	testKeysMutex   sync.Mutex
	testKeysFunded  bool
)

// loadTestKeys loads the 100 private keys from ../private_keys.txt
func LoadAccountKeys() error {
	testKeysMutex.Lock()
	defer testKeysMutex.Unlock()

	if len(testPrivateKeys) > 0 {
		return nil // Already loaded
	}

	file, err := os.Open("../private_keys.txt")
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		privKeyHex := strings.TrimSpace(scanner.Text())
		if privKeyHex == "" {
			continue
		}

		// Convert to ECDSA key
		privKey, err := crypto.HexToECDSA(strings.TrimPrefix(privKeyHex, "0x"))
		if err != nil {
			return err
		}

		address := crypto.PubkeyToAddress(privKey.PublicKey)

		testPrivateKeys = append(testPrivateKeys, privKeyHex)
		testECDSAKeys = append(testECDSAKeys, privKey)
		testAddresses = append(testAddresses, address)
	}

	return scanner.Err()
}

// ensureTestAccountsFunded ensures all test accounts have sufficient ETH for testing
func EnsureAccountsFunded(t *testing.T) {
	// Load keys first (outside of lock to avoid deadlock)
	require.NoError(t, LoadAccountKeys())

	testKeysMutex.Lock()
	defer testKeysMutex.Unlock()

	client, err := ethclient.Dial(DefaultL2NetworkURL)
	require.NoError(t, err)
	defer client.Close()

	ctx := context.Background()

	// Get admin private key
	adminPrivKey, err := crypto.HexToECDSA(strings.TrimPrefix(DefaultL2AdminPrivateKey, "0x"))
	require.NoError(t, err)
	adminAddr := crypto.PubkeyToAddress(adminPrivKey.PublicKey)

	// Get admin nonce
	nonce, err := client.PendingNonceAt(ctx, adminAddr)
	require.NoError(t, err)

	// Check funding status
	fundNeeded := false
	minBalance := uint256.MustFromBig(big.NewInt(5e18)) // 5 ETH minimum to handle high gas transactions

	for _, addr := range testAddresses {
		balance, err := client.BalanceAt(ctx, addr, nil)
		require.NoError(t, err)
		if balance.Cmp(minBalance) < 0 {
			fundNeeded = true
			break
		}
	}

	if !fundNeeded {
		t.Logf("All %d test accounts already funded", len(testAddresses))
		testKeysFunded = true
		return
	}

	t.Logf("Funding %d test accounts...", len(testAddresses))

	// Fund accounts with 0.5 ETH each
	fundAmount, _ := new(big.Int).SetString("100000000000000000000", 10) // 100 ETH
	gasPrice := big.NewInt(2e9)                                          // 2 Gwei

	signer := types.MakeSigner(GetTestChainConfig(DefaultL2ChainID), 1, 0)

	// Send all funding transactions at once, then wait for confirmations
	var allTxs []types.Transaction

	// Send all transactions quickly
	for i, addr := range testAddresses {
		tx := &types.LegacyTx{
			CommonTx: types.CommonTx{
				Nonce: nonce + uint64(i),
				To:    &addr,
				Value: uint256.MustFromBig(fundAmount),
				Gas:   21000,
			},
			GasPrice: uint256.MustFromBig(gasPrice),
		}

		signedTx, err := types.SignTx(tx, *signer, adminPrivKey)
		require.NoError(t, err)

		err = client.SendTransaction(ctx, signedTx)
		require.NoError(t, err)

		allTxs = append(allTxs, signedTx)
	}

	// Wait for all transactions to be mined
	t.Logf("Waiting for %d funding transactions to be mined...", len(allTxs))
	failedCount := 0
	for _, tx := range allTxs {
		err := WaitTxToBeMined(ctx, client, tx, DefaultTimeoutTxToBeMined)
		if err != nil {
			failedCount++
		}
	}

	if failedCount > 0 {
		t.Logf("Warning: %d out of %d funding transactions failed", failedCount, len(allTxs))
	}

	// Verify funding
	fundedCount := 0
	for _, addr := range testAddresses {
		balance, err := client.BalanceAt(ctx, addr, nil)
		require.NoError(t, err)
		if balance.Cmp(minBalance) >= 0 {
			fundedCount++
		}
	}

	require.Equal(t, len(testAddresses), fundedCount, "Not all accounts were funded successfully")
	testKeysFunded = true
	t.Logf("Successfully funded all %d test accounts", len(testAddresses))
}

// getTestKey returns the private key and address for the given index
func GetAccountKey(index int) (*ecdsa.PrivateKey, common.Address) {
	testKeysMutex.Lock()
	defer testKeysMutex.Unlock()

	if len(testECDSAKeys) == 0 {
		panic("Test keys not loaded")
	}

	if index >= len(testECDSAKeys) {
		panic("Test key index out of range")
	}

	return testECDSAKeys[index], testAddresses[index]
}

// getTestKeyCount returns the number of available test keys
func GetAccountKeyCount() int {
	testKeysMutex.Lock()
	defer testKeysMutex.Unlock()
	return len(testECDSAKeys)
}
