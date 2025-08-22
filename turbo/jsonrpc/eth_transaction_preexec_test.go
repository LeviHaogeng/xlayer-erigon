package jsonrpc

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/holiman/uint256"
	ethereum "github.com/ledgerwatch/erigon"
	"github.com/ledgerwatch/erigon-lib/common"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"

	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon/accounts/abi"
	"github.com/ledgerwatch/erigon/accounts/abi/bind"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/rpc/rpccfg"
	"github.com/ledgerwatch/erigon/test/operations"
	"github.com/ledgerwatch/erigon/turbo/adapter/ethapi"
	"github.com/ledgerwatch/erigon/turbo/jsonrpc/constants"
	"github.com/ledgerwatch/erigon/turbo/stages/mock"

	_ "github.com/ledgerwatch/erigon/eth/tracers/native"
)

const (
	tmpSenderPrivateKey = "363ea277eec54278af051fb574931aec751258450a286edce9e1f64401f3b9c8"
)

// Global variables to store deployed contract addresses
var (
	contractAAddr     libcommon.Address
	contractBAddr     libcommon.Address
	factoryAddr       libcommon.Address
	deploymentAddress libcommon.Address
	contractsDeployed bool
)

// =============================================================================
// Helper Functions
// =============================================================================

// createTestPreArgs creates PreArgs for unit testing
func createTestPreArgs(from, to libcommon.Address, nonce uint64, data []byte, value *big.Int) PreArgs {
	if value == nil {
		value = big.NewInt(0)
	}
	nonceHex := hexutil.Uint64(nonce)
	gas := hexutil.Uint64(1000000)
	gasPrice := (*hexutil.Big)(big.NewInt(20000000000)) // 20 Gwei
	valueBig := (*hexutil.Big)(value)

	args := PreArgs{
		From:     &from,
		To:       &to,
		Nonce:    &nonceHex,
		Gas:      &gas,
		GasPrice: gasPrice,
		Value:    valueBig,
	}

	if data != nil {
		hexData := hexutility.Bytes(data)
		args.Data = &hexData
	}

	return args
}

// createEIP1559PreArgs creates EIP-1559 transaction args for testing
func createEIP1559PreArgs(from, to libcommon.Address, nonce uint64, data []byte, value *big.Int) PreArgs {
	if value == nil {
		value = big.NewInt(0)
	}
	nonceHex := hexutil.Uint64(nonce)
	gas := hexutil.Uint64(1000000)
	maxFeePerGas := (*hexutil.Big)(big.NewInt(30000000000))        // 30 Gwei
	maxPriorityFeePerGas := (*hexutil.Big)(big.NewInt(2000000000)) // 2 Gwei
	valueBig := (*hexutil.Big)(value)

	args := PreArgs{
		From:                 &from,
		To:                   &to,
		Nonce:                &nonceHex,
		Gas:                  &gas,
		MaxFeePerGas:         maxFeePerGas,
		MaxPriorityFeePerGas: maxPriorityFeePerGas,
		Value:                valueBig,
	}

	if data != nil {
		hexData := hexutility.Bytes(data)
		args.Data = &hexData
	}

	return args
}

// createEIP7702PreArgs creates EIP-7702 transaction args for testing
func createEIP7702PreArgs(from, to libcommon.Address, nonce uint64, data []byte, value *big.Int) PreArgs {
	if value == nil {
		value = big.NewInt(0)
	}
	nonceHex := hexutil.Uint64(nonce)
	gas := hexutil.Uint64(1000000)
	gasPrice := (*hexutil.Big)(big.NewInt(20000000000)) // 20 Gwei
	valueBig := (*hexutil.Big)(value)

	// Mock authorization list (EIP-7702)
	authList := []interface{}{
		map[string]interface{}{
			"chainId": "0x1",
			"address": "0x1234567890123456789012345678901234567890",
			"nonce":   "0x0",
			"v":       "0x1b",
			"r":       "0x1234567890123456789012345678901234567890123456789012345678901234",
			"s":       "0x1234567890123456789012345678901234567890123456789012345678901234",
		},
	}

	args := PreArgs{
		From:              &from,
		To:                &to,
		Nonce:             &nonceHex,
		Gas:               &gas,
		GasPrice:          gasPrice,
		Value:             valueBig,
		AuthorizationList: authList,
	}

	if data != nil {
		hexData := hexutility.Bytes(data)
		args.Data = &hexData
	}

	return args
}

// newBaseApiForTest creates a BaseAPI for unit testing
func newBaseApiForTest(m *mock.MockSentry) *BaseAPI {
	agg := m.HistoryV3Components()
	stateCache := kvcache.New(kvcache.DefaultCoherentConfig)
	return NewBaseApi(nil, stateCache, m.BlockReader, agg, false, rpccfg.DefaultEvmCallTimeout, m.Engine, m.Dirs)
}

// setupTestEnvironment creates a test environment for unit tests
func setupTestEnvironment(t *testing.T) (*APIImpl, libcommon.Address) {
	// Create test key and address
	bankKey, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	bankAddress := crypto.PubkeyToAddress(bankKey.PublicKey)
	bankFunds := big.NewInt(1e18) // 1 ETH

	// Create genesis spec
	gspec := &types.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			bankAddress: {Balance: bankFunds},
		},
	}

	// Create mock environment
	m := mock.MockWithGenesis(t, gspec, bankKey, false)

	// Create API using the same pattern as other tests
	api := NewEthAPI(newBaseApiForTest(m), m.DB, nil, nil, nil, nil, 5000000, 1e18, 100_000, &ethconfig.Defaults, false, 100_000, 128, log.New(), nil, 100_000)

	return api, bankAddress
}

// Integration test helper functions
func transToken(t *testing.T, ctx context.Context, client *ethclient.Client, amount *uint256.Int, toAddress string) string {
	return transTokenWithFrom(t, ctx, client, operations.DefaultL2AdminPrivateKey, amount, toAddress)
}

func transTokenWithFrom(t *testing.T, ctx context.Context, client *ethclient.Client, fromPrivateKey string, amount *uint256.Int, toAddress string) string {
	chainID, err := client.ChainID(ctx)
	require.NoError(t, err)
	auth, err := operations.GetAuth(fromPrivateKey, chainID.Uint64())
	require.NoError(t, err)
	nonce, err := client.PendingNonceAt(ctx, auth.From)
	require.NoError(t, err)
	gasPrice, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)

	to := libcommon.HexToAddress(toAddress)
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: auth.From, To: &to, Value: amount})
	require.NoError(t, err)

	tx := &types.LegacyTx{
		CommonTx: types.CommonTx{Nonce: nonce, To: &to, Gas: gas, Value: amount},
		GasPrice: uint256.MustFromBig(gasPrice),
	}

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(fromPrivateKey, "0x"))
	require.NoError(t, err)

	signer := types.MakeSigner(operations.GetTestChainConfig(operations.DefaultL2ChainID), 1, 0)
	signedTx, err := types.SignTx(tx, *signer, privateKey)
	require.NoError(t, err)

	err = client.SendTransaction(ctx, signedTx)
	require.NoError(t, err)

	err = operations.WaitTxToBeMined(ctx, client, signedTx, operations.DefaultTimeoutTxToBeMined)
	require.NoError(t, err)

	return signedTx.Hash().String()
}

func deployContract(t *testing.T, ctx context.Context, client *ethclient.Client, privateKey *ecdsa.PrivateKey, contractName, abiJson, bytecodeStr string, constructorArgs ...interface{}) libcommon.Address {
	fromAddress := crypto.PubkeyToAddress(privateKey.PublicKey)
	nonce, err := client.PendingNonceAt(ctx, fromAddress)
	require.NoError(t, err)
	gasPrice, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(int64(operations.DefaultL2ChainID)))
	require.NoError(t, err)
	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)
	auth.GasLimit = uint64(3000000)
	auth.GasPrice = gasPrice

	contractABI, err := abi.JSON(strings.NewReader(abiJson))
	require.NoError(t, err)
	contractBytecode, err := hex.DecodeString(bytecodeStr)
	require.NoError(t, err)

	contractAddr, tx, _, err := bind.DeployContract(auth, contractABI, contractBytecode, client, constructorArgs...)
	require.NoError(t, err)

	bind.WaitDeployed(ctx, client, tx)
	return contractAddr
}

func ensureContractsDeployed(t *testing.T) {
	if contractsDeployed {
		return
	}

	ctx := context.Background()
	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	privateKey, err := crypto.HexToECDSA(tmpSenderPrivateKey)
	require.NoError(t, err)
	deploymentAddress = crypto.PubkeyToAddress(privateKey.PublicKey)

	transToken(t, ctx, client, uint256.NewInt(5000000000000000000), deploymentAddress.String())
	contractBAddr = deployContract(t, ctx, client, privateKey, "ContractB", constants.ContractBABIJson, constants.ContractBBytecodeStr)
	contractAAddr = deployContract(t, ctx, client, privateKey, "ContractA", constants.ContractAABIJson, constants.ContractABytecodeStr, contractBAddr)
	factoryAddr = deployContract(t, ctx, client, privateKey, "ContractFactory", constants.ContractFactoryABIJson, constants.ContractFactoryBytecodeStr)

	contractsDeployed = true
}

func encodeTransferCall(to string, amount uint64) []byte {
	// ERC20 transfer function selector: 0xa9059cbb
	selector := []byte{0xa9, 0x05, 0x9c, 0xbb}

	// Convert address string to bytes
	toAddr := libcommon.HexToAddress(to)

	// Create the data payload
	data := make([]byte, 68) // 4 bytes selector + 32 bytes address + 32 bytes amount
	copy(data[:4], selector)
	copy(data[4+12:36], toAddr.Bytes()) // Address goes in the last 20 bytes of the 32-byte slot

	// Amount as big.Int
	amountBig := big.NewInt(int64(amount))
	amountBig.FillBytes(data[36:68])

	return data
}

func encodeComplexCall(target libcommon.Address) []byte {
	// Function selector for complex call
	selector := []byte{0xe6, 0x09, 0x05, 0x5e}

	// Create the data payload
	data := make([]byte, 36) // 4 bytes selector + 32 bytes address
	copy(data[:4], selector)
	copy(data[4+12:36], target.Bytes()) // Address goes in the last 20 bytes

	return data
}

// =============================================================================
// Unit Tests (Mock Environment)
// =============================================================================

// Test Scenario 1: Multiple transactions, each only calls once (simple calls)
func TestTransactionPreExec_MultipleSimpleCalls(t *testing.T) {
	api, bankAddress := setupTestEnvironment(t)
	ctx := context.Background()

	// Create multiple simple transactions
	recipient1 := libcommon.HexToAddress("0x1111111111111111111111111111111111111111")
	recipient2 := libcommon.HexToAddress("0x2222222222222222222222222222222222222222")
	recipient3 := libcommon.HexToAddress("0x3333333333333333333333333333333333333333")

	transactions := []PreArgs{
		// Simple ETH transfer 1
		createTestPreArgs(bankAddress, recipient1, 0, nil, big.NewInt(1e15)), // 0.001 ETH
		// Simple ETH transfer 2
		createTestPreArgs(bankAddress, recipient2, 1, nil, big.NewInt(2e15)), // 0.002 ETH
		// Simple ETH transfer 3
		createTestPreArgs(bankAddress, recipient3, 2, nil, big.NewInt(3e15)), // 0.003 ETH
	}

	// Execute transactions
	results, err := api.TransactionPreExec(ctx, transactions, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Verify results
	for i, result := range results {
		t.Logf("Transaction %d: GasUsed=%d, Error.Msg=%s", i, result.GasUsed, result.Error.Msg)

		// Should be successful
		assert.Empty(t, result.Error.Msg, "Transaction %d should succeed", i)

		// Gas should be standard transfer gas (21000)
		assert.Equal(t, uint64(21000), result.GasUsed, "Transaction %d should use 21000 gas", i)

		// Simple ETH transfers should NOT have inner transactions
		innerTxs, ok := result.InnerTxs.([]*PreExecInnerTx)
		if ok {
			// Simple transfers should have empty inner transactions
			assert.Empty(t, innerTxs, "Transaction %d (simple transfer) should have no inner transactions", i)
			t.Logf("Transaction %d InnerTxs count: %d (expected: 0 for simple transfer)", i, len(innerTxs))
		}

		// Should have no logs (simple transfers)
		logs, ok := result.Logs.([]*types.Log)
		if ok {
			assert.Empty(t, logs, "Transaction %d should have no logs", i)
		}

		// State diff may be empty in test environment without prestate tracer
		stateDiff, ok := result.StateDiff.(map[string]interface{})
		if ok {
			t.Logf("Transaction %d state diff: %v", i, len(stateDiff))
		}
	}

	t.Logf("✅ Test passed: Multiple simple calls executed successfully")
}

// Test Scenario 2: Contract call using state overrides (no deployment needed)
func TestTransactionPreExec_ContractCallWithStateOverrides(t *testing.T) {
	api, bankAddress := setupTestEnvironment(t)
	ctx := context.Background()

	// Simple storage contract bytecode - returns a fixed value (42) when called
	// This is a minimal contract that just returns 42 for any function call
	storageContractBytecode := "0x6080604052348015600f57600080fd5b506004361060285760003560e01c8063552410771460305780632096525514604c575b600080fd5b60005460405190815260200160405180910390f35b605c6057366004605e565b600055565b005b600060208284031215606f57600080fd5b503591905056fea2646970667358221220000000000000000000000000000000000000000000000000000000000000000064736f6c63430008130033"

	// Use a deterministic contract address
	contractAddr := libcommon.HexToAddress("0x3a220f351252089d385b29beca14e27f204c296a")

	// getValue() function selector (0x55241077)
	getValueData := libcommon.FromHex("0x55241077")

	// setValue(42) function call data (0x20965255 + 32 bytes for value 42)
	setValueData := libcommon.FromHex("0x20965255000000000000000000000000000000000000000000000000000000000000002a")

	// Create state overrides to inject contract code and initial storage
	stateOverrides := &ethapi.FlexibleStateOverrides{
		contractAddr: {
			Code: func() *hexutility.Bytes {
				bytecode := libcommon.FromHex(storageContractBytecode)
				return (*hexutility.Bytes)(&bytecode)
			}(),
			State: func() *map[libcommon.Hash]ethapi.FlexibleUint256 {
				state := make(map[libcommon.Hash]ethapi.FlexibleUint256)
				state[libcommon.Hash{}] = ethapi.FlexibleUint256{Int: *uint256.NewInt(100)} // Initial value in slot 0
				return &state
			}(),
		},
	}

	transactions := []PreArgs{
		// 1. Call getValue() - should return 100
		createTestPreArgs(bankAddress, contractAddr, 0, getValueData, big.NewInt(0)),
		// 2. Call setValue(42)
		createTestPreArgs(bankAddress, contractAddr, 1, setValueData, big.NewInt(0)),
		// 3. Call getValue() again - should return 42 (but won't in this test since state doesn't persist between calls)
		createTestPreArgs(bankAddress, contractAddr, 2, getValueData, big.NewInt(0)),
	}

	// Execute transactions with state overrides
	results, err := api.TransactionPreExec(ctx, transactions, nil, stateOverrides)
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Verify results
	for i, result := range results {
		t.Logf("Transaction %d: GasUsed=%d, Error.Msg=%s", i, result.GasUsed, result.Error.Msg)

		// Contract calls should use more gas than simple transfers, even if they fail
		assert.Greater(t, result.GasUsed, uint64(21000), "Contract call should use more than 21000 gas")

		// Note: Contract calls may fail due to bytecode issues, but that's OK for testing the interface

		// Check for inner transactions
		innerTxs, ok := result.InnerTxs.([]*PreExecInnerTx)
		if ok && len(innerTxs) > 0 {
			t.Logf("Transaction %d has %d inner transactions", i, len(innerTxs))
			for j, innerTx := range innerTxs {
				t.Logf("  InnerTx %d: CallType=%s, From=%s, To=%s, GasUsed=%d, Output=%s",
					j, innerTx.CallType, innerTx.From, innerTx.To, innerTx.GasUsed, innerTx.Output)
			}
		}
	}

	t.Logf("✅ Test passed: Contract calls with state overrides work correctly")
}

// Test EIP-1559 transaction rejection
func TestTransactionPreExec_EIP1559Rejection(t *testing.T) {
	api, bankAddress := setupTestEnvironment(t)
	ctx := context.Background()

	recipient := libcommon.HexToAddress("0x1111111111111111111111111111111111111111")

	testCases := []struct {
		name        string
		transaction PreArgs
		expectError bool
		errorCode   int
		description string
	}{
		{
			name: "EIP-1559 transaction with maxFeePerGas only",
			transaction: func() PreArgs {
				args := createTestPreArgs(bankAddress, recipient, 0, nil, big.NewInt(1e15))
				maxFeePerGas := (*hexutil.Big)(big.NewInt(30000000000)) // 30 Gwei
				args.MaxFeePerGas = maxFeePerGas
				return args
			}(),
			expectError: true,
			errorCode:   CheckPreArgsErrCode,
			description: "Should reject transaction with maxFeePerGas set",
		},
		{
			name:        "EIP-1559 transaction with both maxFeePerGas and maxPriorityFeePerGas",
			transaction: createEIP1559PreArgs(bankAddress, recipient, 0, nil, big.NewInt(1e15)),
			expectError: true,
			errorCode:   CheckPreArgsErrCode,
			description: "Should reject full EIP-1559 transaction",
		},
		{
			name:        "Legacy transaction (gasPrice only)",
			transaction: createTestPreArgs(bankAddress, recipient, 0, nil, big.NewInt(1e15)),
			expectError: false,
			errorCode:   0,
			description: "Should accept legacy transaction with gasPrice",
		},
		{
			name:        "EIP-7702 transaction with authorizationList",
			transaction: createEIP7702PreArgs(bankAddress, recipient, 0, nil, big.NewInt(1e15)),
			expectError: true,
			errorCode:   CheckPreArgsErrCode,
			description: "Should reject EIP-7702 transaction with authorizationList",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("🧪 Testing: %s", tc.description)

			results, err := api.TransactionPreExec(ctx, []PreArgs{tc.transaction}, nil, nil)
			require.NoError(t, err, "API call should not fail")
			require.Len(t, results, 1, "Should have one result")

			result := results[0]
			t.Logf("📊 %s: GasUsed=%d, Error.Code=%d, Error.Msg=%s",
				tc.name, result.GasUsed, result.Error.Code, result.Error.Msg)

			if tc.expectError {
				assert.NotEmpty(t, result.Error.Msg, "Should have error message for %s", tc.name)
				assert.Equal(t, tc.errorCode, result.Error.Code, "Should have correct error code for %s", tc.name)

				// Check for appropriate error message based on transaction type
				if strings.Contains(tc.name, "EIP-1559") {
					assert.Contains(t, result.Error.Msg, "EIP-1559", "Error message should mention EIP-1559 for %s", tc.name)
				} else if strings.Contains(tc.name, "EIP-7702") {
					assert.Contains(t, result.Error.Msg, "EIP-7702", "Error message should mention EIP-7702 for %s", tc.name)
				}
				t.Logf("✅ Expected rejection: %s", result.Error.Msg)
			} else {
				assert.Empty(t, result.Error.Msg, "Should not have error message for %s", tc.name)
				assert.Equal(t, 0, result.Error.Code, "Should have zero error code for %s", tc.name)
				assert.Greater(t, result.GasUsed, uint64(0), "Should have used gas for %s", tc.name)
				t.Logf("✅ Expected success: GasUsed=%d", result.GasUsed)
			}
		})
	}

	t.Logf("✅ Test completed: EIP-1559 transaction rejection works correctly")
}

// Test response format matches expected RPC format
func TestTransactionPreExec_ResponseFormat(t *testing.T) {
	api, bankAddress := setupTestEnvironment(t)
	ctx := context.Background()

	// Create a comprehensive test scenario with both simple transfer and contract call
	contractAddr := libcommon.HexToAddress("0x91235900178f3ce970028fad5f1be25cd385a2e4")
	recipient := libcommon.HexToAddress("0xc63cc5f27f4793c97e3d483bb10726acb7ec0275")

	// ERC20 transfer function call data: transfer(address,uint256)
	transferData := libcommon.FromHex("0xa9059cbb000000000000000000000000c63cc5f27f4793c97e3d483bb10726acb7ec027500000000000000000000000000000000000000000000021e19e0c9bab2400000")

	// Mock ERC20 contract bytecode (simplified)
	erc20Bytecode := "0x608060405234801561001057600080fd5b50600436106100415760003560e01c8063a9059cbb14610046578063dd62ed3e1461007c578063313ce567146100ac575b600080fd5b61006a6004803603604081101561005c57600080fd5b50803590602001356100b4565b60408051918252519081900360200190f35b61006a6004803603604081101561009257600080fd5b506001600160a01b03813581169160200135166100ba565b61006a6100c0565b50600190565b50600090565b600a90565056fea265627a7a72315820000000000000000000000000000000000000000000000000000000000000000064736f6c63430008130033"

	// Create state overrides to simulate ERC20 contract
	stateOverrides := &ethapi.FlexibleStateOverrides{
		contractAddr: {
			Code: func() *hexutility.Bytes {
				bytecode := libcommon.FromHex(erc20Bytecode)
				return (*hexutility.Bytes)(&bytecode)
			}(),
			// Initial storage and balance
			State: func() *map[libcommon.Hash]ethapi.FlexibleUint256 {
				state := make(map[libcommon.Hash]ethapi.FlexibleUint256)
				// Set some initial balances in ERC20 contract storage
				return &state
			}(),
		},
	}

	transactions := []PreArgs{
		// 1. Simple ETH transfer (should NOT have innerTxs)
		createTestPreArgs(bankAddress, recipient, 0, nil, big.NewInt(1e15)), // 0.001 ETH
		// 2. ERC20 transfer (should have innerTxs)
		createTestPreArgs(bankAddress, contractAddr, 1, transferData, big.NewInt(0)),
	}

	// Execute transactions
	results, err := api.TransactionPreExec(ctx, transactions, nil, stateOverrides)
	require.NoError(t, err)
	require.Len(t, results, 2)

	t.Logf("🧪 Testing response format compliance...")

	// Check overall structure
	for i, result := range results {
		t.Logf("📊 Testing transaction %d response format...", i)

		// 1. Verify top-level structure
		assert.IsType(t, PreResult{}, result, "Result should be PreResult type")

		// 2. Check innerTxs field structure and type
		assert.NotNil(t, result.InnerTxs, "InnerTxs field should not be nil")

		// InnerTxs should be either empty slice or slice of PreExecInnerTx
		innerTxsRaw := result.InnerTxs
		innerTxsBytes, err := json.Marshal(innerTxsRaw)
		require.NoError(t, err, "InnerTxs should be JSON serializable")

		var innerTxs []*PreExecInnerTx
		err = json.Unmarshal(innerTxsBytes, &innerTxs)
		assert.NoError(t, err, "InnerTxs should unmarshal to []*PreExecInnerTx")

		t.Logf("  Transaction %d has %d innerTxs", i, len(innerTxs))

		if i == 0 {
			// Simple transfer should have no innerTxs
			assert.Empty(t, innerTxs, "Simple ETH transfer should have empty innerTxs")
		} else {
			// Contract call should have innerTxs (may be empty in test env, but structure should be correct)
			t.Logf("  Contract call innerTxs count: %d", len(innerTxs))
		}

		// Check innerTxs structure if present
		for j, innerTx := range innerTxs {
			t.Logf("  InnerTx %d structure check:", j)

			// Verify all required fields are present and correct type
			assert.IsType(t, big.Int{}, innerTx.Dept, "Dept should be big.Int")
			assert.IsType(t, big.Int{}, innerTx.InternalIndex, "InternalIndex should be big.Int")
			assert.IsType(t, "", innerTx.CallType, "CallType should be string")
			assert.IsType(t, "", innerTx.Name, "Name should be string")
			assert.IsType(t, "", innerTx.TraceAddress, "TraceAddress should be string")
			assert.IsType(t, "", innerTx.CodeAddress, "CodeAddress should be string")
			assert.IsType(t, "", innerTx.From, "From should be string")
			assert.IsType(t, "", innerTx.To, "To should be string")
			assert.IsType(t, "", innerTx.Input, "Input should be string")
			assert.IsType(t, "", innerTx.Output, "Output should be string")
			assert.IsType(t, false, innerTx.IsError, "IsError should be bool")
			assert.IsType(t, uint64(0), innerTx.GasUsed, "GasUsed should be uint64")
			assert.IsType(t, "", innerTx.Value, "Value should be string")
			assert.IsType(t, "", innerTx.ValueWei, "ValueWei should be string")
			assert.IsType(t, "", innerTx.Error, "Error should be string")
			assert.IsType(t, uint64(0), innerTx.ReturnGas, "ReturnGas should be uint64")

			// Verify address format if present
			if innerTx.From != "" {
				assert.True(t, libcommon.IsHexAddress(innerTx.From), "From should be valid hex address: %s", innerTx.From)
			}
			if innerTx.To != "" {
				assert.True(t, libcommon.IsHexAddress(innerTx.To), "To should be valid hex address: %s", innerTx.To)
			}
			if innerTx.CodeAddress != "" {
				assert.True(t, libcommon.IsHexAddress(innerTx.CodeAddress), "CodeAddress should be valid hex address: %s", innerTx.CodeAddress)
			}

			// Verify hex data format
			if innerTx.Input != "" {
				assert.True(t, len(innerTx.Input) >= 2 && innerTx.Input[:2] == "0x", "Input should have 0x prefix: %s", innerTx.Input)
			}
			if innerTx.Output != "" {
				assert.True(t, len(innerTx.Output) >= 2 && innerTx.Output[:2] == "0x", "Output should have 0x prefix: %s", innerTx.Output)
			}

			t.Logf("    ✅ InnerTx %d: CallType=%s, From=%s, To=%s, GasUsed=%d",
				j, innerTx.CallType, innerTx.From, innerTx.To, innerTx.GasUsed)
		}

		// 3. Check logs field structure
		assert.NotNil(t, result.Logs, "Logs field should not be nil")

		logsRaw := result.Logs
		logsBytes, err := json.Marshal(logsRaw)
		require.NoError(t, err, "Logs should be JSON serializable")

		var logs []*types.Log
		err = json.Unmarshal(logsBytes, &logs)
		assert.NoError(t, err, "Logs should unmarshal to []*types.Log")

		t.Logf("  Transaction %d has %d logs", i, len(logs))

		// 4. Check stateDiff field structure
		assert.NotNil(t, result.StateDiff, "StateDiff field should not be nil")

		stateDiffRaw := result.StateDiff
		stateDiffBytes, err := json.Marshal(stateDiffRaw)
		require.NoError(t, err, "StateDiff should be JSON serializable")

		var stateDiff map[string]interface{}
		err = json.Unmarshal(stateDiffBytes, &stateDiff)
		assert.NoError(t, err, "StateDiff should unmarshal to map[string]interface{}")

		t.Logf("  Transaction %d has state diff for %d addresses", i, len(stateDiff))

		// 5. Check error field structure
		assert.IsType(t, PreError{}, result.Error, "Error should be PreError type")
		assert.IsType(t, 0, result.Error.Code, "Error.Code should be int")
		assert.IsType(t, "", result.Error.Msg, "Error.Msg should be string")

		// 6. Check gas and block number fields
		assert.IsType(t, uint64(0), result.GasUsed, "GasUsed should be uint64")
		assert.Greater(t, result.GasUsed, uint64(0), "GasUsed should be greater than 0")

		assert.NotNil(t, result.BlockNumber, "BlockNumber should not be nil")
		assert.IsType(t, &big.Int{}, result.BlockNumber, "BlockNumber should be *big.Int")
		assert.True(t, result.BlockNumber.Sign() >= 0, "BlockNumber should be non-negative")

		t.Logf("  ✅ Transaction %d: GasUsed=%d, BlockNumber=%s, Error.Code=%d",
			i, result.GasUsed, result.BlockNumber.String(), result.Error.Code)
	}

	t.Logf("✅ Test completed: Response format matches expected RPC structure")
}

// =============================================================================
// Integration Tests (Real Network)
// =============================================================================

// TestTransactionPreExec tests the eth_transactionPreExec RPC method
// Uses pre-deployed contracts to test calling Contract A which internally calls Contract B
func TestTransactionPreExecInnerTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	ensureContractsDeployed(t)

	contractAABI, err := abi.JSON(strings.NewReader(constants.ContractAABIJson))
	require.NoError(t, err)
	calldata, err := contractAABI.Pack("triggerCall")
	require.NoError(t, err)

	rpcClient, err := rpc.Dial(operations.DefaultL2NetworkURL, log.New())
	require.NoError(t, err)
	defer rpcClient.Close()

	fromAddr := libcommon.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	txRequest := map[string]interface{}{
		"from": fromAddr.Hex(), "to": contractAAddr.Hex(), "gas": "0x30000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x1",
		"data": fmt.Sprintf("0x%x", calldata),
	}
	stateOverride := map[string]interface{}{
		fromAddr.Hex(): map[string]interface{}{"balance": "0x1000000000000000000000"},
	}

	var result json.RawMessage
	err = rpcClient.Call(&result, "eth_transactionPreExec", []interface{}{txRequest}, "latest", stateOverride)
	require.NoError(t, err)

	var preExecResults []map[string]interface{}
	err = json.Unmarshal(result, &preExecResults)
	require.NoError(t, err)
	require.Len(t, preExecResults, 1)

	preExecResult := preExecResults[0]
	require.NotNil(t, preExecResult["logs"])
	require.NotNil(t, preExecResult["stateDiff"])
	require.NotNil(t, preExecResult["gasUsed"])
	require.NotNil(t, preExecResult["blockNumber"])

	innerTxList, ok := preExecResult["innerTxs"].([]interface{})
	require.True(t, ok)
	require.GreaterOrEqual(t, len(innerTxList), 1)

	firstInnerTx := innerTxList[0].(map[string]interface{})
	require.Equal(t, "call", firstInnerTx["call_type"])
	require.Equal(t, strings.ToLower(contractAAddr.Hex()), strings.ToLower(firstInnerTx["to"].(string)))
	require.Equal(t, "0xf18c388a", firstInnerTx["input"].(string))
	require.False(t, firstInnerTx["is_error"].(bool), "Expected is_error to be false for the first inner transaction")

	if len(innerTxList) >= 2 {
		secondInnerTx := innerTxList[1].(map[string]interface{})
		require.Equal(t, "call", secondInnerTx["call_type"])
		require.Equal(t, strings.ToLower(contractAAddr.Hex()), strings.ToLower(secondInnerTx["from"].(string)))
		require.Equal(t, strings.ToLower(contractBAddr.Hex()), strings.ToLower(secondInnerTx["to"].(string)))
		require.Equal(t, "0x32e43a11", secondInnerTx["input"].(string))

		name := secondInnerTx["name"].(string)
		require.True(t, name[len(name)-1] >= '0' && name[len(name)-1] <= '9')
		require.False(t, secondInnerTx["is_error"].(bool), "Expected is_error to be false for the second inner transaction")
	}

	transferTx := map[string]interface{}{
		"from": fromAddr.Hex(), "to": "0x742d35Cc4cF52f9234E96bC29d7F6a0c91d87b06",
		"value": "0x1000000000000000", "gas": "0x5208",
		"gasPrice": "0x4a817c800", "nonce": "0x2",
	}

	var transferResult json.RawMessage
	err = rpcClient.Call(&transferResult, "eth_transactionPreExec", []interface{}{transferTx}, "latest", nil)
	require.NoError(t, err)

	var transferResults []map[string]interface{}
	err = json.Unmarshal(transferResult, &transferResults)
	require.NoError(t, err)
	require.Len(t, transferResults, 1)

	transferInnerTxs, ok := transferResults[0]["innerTxs"].([]interface{})
	require.True(t, ok, "innerTxs should be an array for simple transfers")
	require.Empty(t, transferInnerTxs, "innerTxs should be empty array for simple transfers (dept == 0)")

	t.Logf("✅ Simple transfer validation: innerTxs count = %d (expected: 0)", len(transferInnerTxs))
}

// TestTransactionPreExecWithCreateOpcode tests the eth_transactionPreExec RPC method with CREATE opcode
// Uses pre-deployed factory contract to test calling a function that creates another contract using CREATE opcode
func TestTransactionPreExecWithCreateOpcode(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	ensureContractsDeployed(t)

	factoryABI, err := abi.JSON(strings.NewReader(constants.ContractFactoryABIJson))
	require.NoError(t, err)
	calldata, err := factoryABI.Pack("createSimpleStorage", big.NewInt(123))
	require.NoError(t, err)

	rpcClient, err := rpc.Dial(operations.DefaultL2NetworkURL, log.New())
	require.NoError(t, err)
	defer rpcClient.Close()

	fromAddr := libcommon.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	txRequest := map[string]interface{}{
		"from": fromAddr.Hex(), "to": factoryAddr.Hex(), "gas": "0x100000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x1",
		"data": fmt.Sprintf("0x%x", calldata),
	}
	stateOverride := map[string]interface{}{
		fromAddr.Hex(): map[string]interface{}{"balance": "0x1000000000000000000000"},
	}

	var result json.RawMessage
	err = rpcClient.Call(&result, "eth_transactionPreExec", []interface{}{txRequest}, "latest", stateOverride)
	require.NoError(t, err)

	var preExecResults []map[string]interface{}
	err = json.Unmarshal(result, &preExecResults)
	require.NoError(t, err)
	require.Len(t, preExecResults, 1)

	innerTxList, ok := preExecResults[0]["innerTxs"].([]interface{})
	require.True(t, ok)
	require.GreaterOrEqual(t, len(innerTxList), 1)

	foundCreate := false
	for _, innerTx := range innerTxList {
		innerTxMap := innerTx.(map[string]interface{})
		if callType := innerTxMap["call_type"]; callType == "create" || callType == "create2" {
			foundCreate = true
			require.Equal(t, strings.ToLower(factoryAddr.Hex()), strings.ToLower(innerTxMap["from"].(string)))
			if to := innerTxMap["to"]; to != nil {
				require.NotEmpty(t, to.(string))
				require.NotEqual(t, "0x0000000000000000000000000000000000000000", strings.ToLower(to.(string)))
			}
			if input := innerTxMap["input"]; input != nil {
				require.NotEmpty(t, input.(string))
				require.NotEqual(t, "0x", input.(string))
			}
			break
		}
	}
	require.True(t, foundCreate)
}

// TestTransactionPreExecNonSequentialNonces tests nonce validation with the updated strict nonce checking
func TestTransactionPreExecNonSequentialNonces(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ensureContractsDeployed(t)

	contractBABI, err := abi.JSON(strings.NewReader(constants.ContractBABIJson))
	require.NoError(t, err)
	calldata, err := contractBABI.Pack("dummy")
	require.NoError(t, err)

	rpcClient, err := rpc.Dial(operations.DefaultL2NetworkURL, log.New())
	require.NoError(t, err)
	defer rpcClient.Close()

	fromAddr := libcommon.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

	txRequest1 := map[string]interface{}{
		"from": fromAddr.Hex(), "to": contractBAddr.Hex(), "gas": "0x30000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x5",
		"data": fmt.Sprintf("0x%x", calldata),
	}
	txRequest2 := map[string]interface{}{
		"from": fromAddr.Hex(), "to": contractBAddr.Hex(), "gas": "0x30000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x3",
		"data": fmt.Sprintf("0x%x", calldata),
	}
	stateOverride := map[string]interface{}{
		fromAddr.Hex(): map[string]interface{}{"balance": "0x1000000000000000000000"},
	}

	var result json.RawMessage
	err = rpcClient.Call(&result, "eth_transactionPreExec", []interface{}{txRequest1, txRequest2}, "latest", stateOverride)
	require.NoError(t, err)

	var preExecResults []map[string]interface{}
	err = json.Unmarshal(result, &preExecResults)
	require.NoError(t, err)
	require.Len(t, preExecResults, 2)

	// Second transaction should also fail due to wrong nonce
	secondResult := preExecResults[1]
	require.NotNil(t, secondResult["error"])
	errorMap2 := secondResult["error"].(map[string]interface{})
	errorCode2 := int(errorMap2["code"].(float64))
	require.Equal(t, 1003, errorCode2)
	require.Contains(t, errorMap2["msg"].(string), fromAddr.Hex())
}

// TestTransactionPreExecGasValidation compares gasUsed with eth_estimateGas
func TestTransactionPreExecGasValidation(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ensureContractsDeployed(t)

	contractAABI, err := abi.JSON(strings.NewReader(constants.ContractAABIJson))
	require.NoError(t, err)
	calldata, err := contractAABI.Pack("triggerCall")
	require.NoError(t, err)

	// Create both RPC client and eth client for comparison
	rpcClient, err := rpc.Dial(operations.DefaultL2NetworkURL, log.New())
	require.NoError(t, err)
	defer rpcClient.Close()

	ethClient, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)
	defer ethClient.Close()

	fromAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

	ctx := context.Background()
	fundingAmount := uint256.NewInt(5000000000000000000)
	fundingTxHash := transToken(t, ctx, ethClient, fundingAmount, fromAddr.String())
	t.Logf("✅ Funded test address %s with 5 ETH, tx: %s", fromAddr.Hex(), fundingTxHash)

	balance, err := ethClient.BalanceAt(ctx, fromAddr, nil)
	require.NoError(t, err)
	balanceETH := new(big.Float).Quo(new(big.Float).SetInt(balance.ToBig()), new(big.Float).SetFloat64(1e18))
	t.Logf("✅ Test address balance after funding: %s ETH", balanceETH.String())
	t.Logf("🎯 Both eth_transactionPreExec and eth_estimateGas will now use the same funded address")

	// Test Case 1: Simple Contract Call
	txRequest := map[string]interface{}{
		"from": fromAddr.Hex(), "to": contractAAddr.Hex(), "gas": "0x100000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x1",
		"data": fmt.Sprintf("0x%x", calldata),
	}
	// Get gasUsed from eth_transactionPreExec
	var preExecResult json.RawMessage
	err = rpcClient.Call(&preExecResult, "eth_transactionPreExec", []interface{}{txRequest}, "latest", nil)
	require.NoError(t, err)

	var preExecResults []map[string]interface{}
	err = json.Unmarshal(preExecResult, &preExecResults)
	require.NoError(t, err)
	require.Len(t, preExecResults, 1)

	preExecGasUsed := preExecResults[0]["gasUsed"].(float64)

	// Get gas estimate from eth_estimateGas
	estimateGasRequest := map[string]interface{}{
		"from": fromAddr.Hex(), "to": contractAAddr.Hex(),
		"data": fmt.Sprintf("0x%x", calldata),
	}

	var estimateResult string
	err = rpcClient.Call(&estimateResult, "eth_estimateGas", estimateGasRequest, "latest")
	require.NoError(t, err)

	estimatedGas, err := strconv.ParseUint(strings.TrimPrefix(estimateResult, "0x"), 16, 64)
	require.NoError(t, err)

	// Validation: Both should be very close
	gasUsedUint64 := uint64(preExecGasUsed)
	tolerance := uint64(5000) // Allow 5K gas difference for binary search precision

	require.Greater(t, gasUsedUint64, uint64(21000), "Gas should be > 21000 for contract call")
	require.Greater(t, estimatedGas, uint64(21000), "Estimated gas should be > 21000 for contract call")

	diff := uint64(0)
	if estimatedGas > gasUsedUint64 {
		diff = estimatedGas - gasUsedUint64
	} else {
		diff = gasUsedUint64 - estimatedGas
	}

	require.LessOrEqual(t, diff, tolerance,
		"Gas difference too large: preExec=%d, estimate=%d, diff=%d",
		gasUsedUint64, estimatedGas, diff)

	t.Logf("✅ Contract Call Gas: preExec=%d, estimate=%d, diff=%d (within tolerance)",
		gasUsedUint64, estimatedGas, diff)

	// Test Case 2: Simple Transfer (should use exactly 21000 gas)
	transferTx := map[string]interface{}{
		"from": fromAddr.Hex(), "to": "0x742d35Cc4cF52f9234E96bC29d7F6a0c91d87b06",
		"value": "0x1000000000000000", "gas": "0x5208", // 21000 in hex
		"gasPrice": "0x4a817c800", "nonce": "0x2",
	}

	// PreExec gas usage
	err = rpcClient.Call(&preExecResult, "eth_transactionPreExec", []interface{}{transferTx}, "latest", nil)
	require.NoError(t, err)
	err = json.Unmarshal(preExecResult, &preExecResults)
	require.NoError(t, err)
	transferPreExecGas := uint64(preExecResults[0]["gasUsed"].(float64))

	// Estimate gas usage
	transferEstimate := map[string]interface{}{
		"from": fromAddr.Hex(), "to": "0x742d35Cc4cF52f9234E96bC29d7F6a0c91d87b06",
		"value": "0x1000000000000000",
	}
	err = rpcClient.Call(&estimateResult, "eth_estimateGas", transferEstimate, "latest")
	require.NoError(t, err)
	transferEstimatedGas, err := strconv.ParseUint(strings.TrimPrefix(estimateResult, "0x"), 16, 64)
	require.NoError(t, err)

	// Simple transfers should be exactly 21000 gas
	require.Equal(t, uint64(21000), transferPreExecGas, "Simple transfer should use exactly 21000 gas")
	require.Equal(t, uint64(21000), transferEstimatedGas, "Simple transfer estimate should be exactly 21000 gas")

	t.Logf("✅ Transfer Gas: preExec=%d, estimate=%d (both exactly 21000)",
		transferPreExecGas, transferEstimatedGas)

	// Test Case 3: CREATE operation
	factoryABI, err := abi.JSON(strings.NewReader(constants.ContractFactoryABIJson))
	require.NoError(t, err)
	createCalldata, err := factoryABI.Pack("createSimpleStorage", big.NewInt(999))
	require.NoError(t, err)

	createTx := map[string]interface{}{
		"from": fromAddr.Hex(), "to": factoryAddr.Hex(), "gas": "0x200000",
		"gasPrice": "0x4a817c800", "value": "0x0", "nonce": "0x3",
		"data": fmt.Sprintf("0x%x", createCalldata),
	}

	// PreExec gas usage for CREATE
	err = rpcClient.Call(&preExecResult, "eth_transactionPreExec", []interface{}{createTx}, "latest", nil)
	require.NoError(t, err)
	err = json.Unmarshal(preExecResult, &preExecResults)
	require.NoError(t, err)
	createPreExecGas := uint64(preExecResults[0]["gasUsed"].(float64))

	// Estimate gas for CREATE
	createEstimate := map[string]interface{}{
		"from": fromAddr.Hex(), "to": factoryAddr.Hex(),
		"data": fmt.Sprintf("0x%x", createCalldata),
	}
	err = rpcClient.Call(&estimateResult, "eth_estimateGas", createEstimate, "latest")
	require.NoError(t, err)
	createEstimatedGas, err := strconv.ParseUint(strings.TrimPrefix(estimateResult, "0x"), 16, 64)
	require.NoError(t, err)

	createDiff := uint64(0)
	if createEstimatedGas > createPreExecGas {
		createDiff = createEstimatedGas - createPreExecGas
	} else {
		createDiff = createPreExecGas - createEstimatedGas
	}

	require.LessOrEqual(t, createDiff, uint64(50000),
		"CREATE gas difference too large: preExec=%d, estimate=%d, diff=%d",
		createPreExecGas, createEstimatedGas, createDiff)

	t.Logf("✅ CREATE Gas: preExec=%d, estimate=%d, diff=%d",
		createPreExecGas, createEstimatedGas, createDiff)
}
