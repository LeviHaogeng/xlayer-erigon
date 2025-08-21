//go:build !skip_smoke
// +build !skip_smoke

package jsonrpc

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/holiman/uint256"
	ethereum "github.com/ledgerwatch/erigon"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/accounts/abi"
	"github.com/ledgerwatch/erigon/accounts/abi/bind"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/test/operations"
	"github.com/ledgerwatch/erigon/turbo/jsonrpc/constants"
	"github.com/ledgerwatch/log/v3"
	"github.com/stretchr/testify/require"
)

const (
	tmpSenderPrivateKey = "363ea277eec54278af051fb574931aec751258450a286edce9e1f64401f3b9c8"
)

func transToken(t *testing.T, ctx context.Context, client *ethclient.Client, amount *uint256.Int, toAddress string) string {
	return transTokenWithFrom(t, ctx, client, operations.DefaultL2AdminPrivateKey, amount, toAddress)
}

// transTokenWithFrom sends tokens from a specified private key
func transTokenWithFrom(t *testing.T, ctx context.Context, client *ethclient.Client, fromPrivateKey string, amount *uint256.Int, toAddress string) string {
	chainID, err := client.ChainID(ctx)
	require.NoError(t, err)
	auth, err := operations.GetAuth(fromPrivateKey, chainID.Uint64())
	require.NoError(t, err)
	nonce, err := client.PendingNonceAt(ctx, auth.From)
	require.NoError(t, err)
	gasPrice, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)

	to := common.HexToAddress(toAddress)
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{
		From:  auth.From,
		To:    &to,
		Value: amount,
	})
	require.NoError(t, err)
	t.Logf("gas: %d", gas)
	t.Logf("gasPrice: %d", gasPrice)

	var tx types.Transaction = &types.LegacyTx{
		CommonTx: types.CommonTx{
			Nonce: nonce,
			To:    &to,
			Gas:   gas,
			Value: amount,
		},
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

func deployContract(t *testing.T, ctx context.Context, client *ethclient.Client, privateKey *ecdsa.PrivateKey, contractName, abiJson, bytecodeStr string, constructorArgs ...interface{}) common.Address {
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	// Fetch nonce
	nonce, err := client.PendingNonceAt(ctx, fromAddress)
	require.NoError(t, err)

	// Define gas parameters
	gasPrice, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)

	// Set up transaction options
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

	// Deploy contract with optional constructor arguments
	contractAddr, tx, _, err := bind.DeployContract(auth, contractABI, contractBytecode, client, constructorArgs...)
	require.NoError(t, err)

	t.Logf("%s deployed at: %s, transaction hash: %s", contractName, contractAddr.Hex(), tx.Hash().Hex())

	// Wait for contract deployment to be mined
	bind.WaitDeployed(ctx, client, tx)

	return contractAddr
}

// TestTransactionPreExec tests the eth_transactionPreExec RPC method
// This deploys two contracts and tests calling Contract A which internally calls Contract B
func TestTransactionPreExec(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	ctx := context.Background()
	client, err := ethclient.Dial(operations.DefaultL2NetworkURL)
	require.NoError(t, err)

	// Deploy the contracts first
	privateKey, err := crypto.HexToECDSA(tmpSenderPrivateKey)
	require.NoError(t, err)

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	// Fund the deployment address
	transToken(t, ctx, client, uint256.NewInt(1000000000000000000), fromAddress.String())

	contractBAddr := deployContract(t, ctx, client, privateKey, "ContractB", constants.ContractBABIJson, constants.ContractBBytecodeStr)

	contractAAddr := deployContract(t, ctx, client, privateKey, "ContractA", constants.ContractAABIJson, constants.ContractABytecodeStr, contractBAddr)

	contractAABI, err := abi.JSON(strings.NewReader(constants.ContractAABIJson))
	require.NoError(t, err)
	calldata, err := contractAABI.Pack("triggerCall")
	require.NoError(t, err)

	rpcClient, err := rpc.Dial(operations.DefaultL2NetworkURL, log.New())
	require.NoError(t, err)
	defer rpcClient.Close()

	fromAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	txRequest := map[string]interface{}{
		"from":     fromAddr.Hex(),
		"to":       contractAAddr.Hex(),
		"gas":      "0x30000",
		"gasPrice": "0x4a817c800",
		"value":    "0x0",
		"nonce":    "0x1",
		"data":     fmt.Sprintf("0x%x", calldata),
	}

	stateOverride := map[string]interface{}{
		fromAddr.Hex(): map[string]interface{}{
			"balance": "0x1000000000000000000000",
		},
	}

	var result json.RawMessage
	err = rpcClient.Call(&result, "eth_transactionPreExec", []interface{}{txRequest}, "latest", stateOverride)
	require.NoError(t, err)

	// Parse the result
	var preExecResults []map[string]interface{}
	err = json.Unmarshal(result, &preExecResults)
	require.NoError(t, err)
	require.Len(t, preExecResults, 1, "Should have one result")

	preExecResult := preExecResults[0]
	t.Logf("PreExec Result: %+v", preExecResult)

	if errorField, exists := preExecResult["error"]; exists && errorField != nil {
		errorMap, ok := errorField.(map[string]interface{})
		if ok {
			if code, codeExists := errorMap["code"]; codeExists {
				if codeFloat, isFloat := code.(float64); isFloat {
					errorCode := int(codeFloat)
					if errorCode != 0 {
						t.Logf("⚠️  PreExec returned error code %d: %v", errorCode, errorField)
					}
				}
			}
		}
	}

	require.NotNil(t, preExecResult["logs"], "Should have logs")
	require.NotNil(t, preExecResult["stateDiff"], "Should have state difference")
	require.NotNil(t, preExecResult["gasUsed"], "Should have gas used")
	require.NotNil(t, preExecResult["blockNumber"], "Should have block number")

	if innerTxs, exists := preExecResult["innerTxs"]; exists {
		innerTxList, ok := innerTxs.([]interface{})
		require.True(t, ok, "innerTxs should be an array")
		require.Greater(t, len(innerTxList), 0, "Should have inner transactions")

		t.Logf("Found %d inner transactions", len(innerTxList))
		for i, innerTx := range innerTxList {
			t.Logf("Inner Tx %d: %+v", i, innerTx)
		}

		require.GreaterOrEqual(t, len(innerTxList), 1, "Should have at least 1 inner transaction")

		// Verify the first inner transaction (external call to ContractA)
		firstInnerTx := innerTxList[0].(map[string]interface{})
		require.Equal(t, "call", firstInnerTx["call_type"], "Should be a call transaction")
		require.Equal(t, strings.ToLower(contractAAddr.Hex()), strings.ToLower(firstInnerTx["to"].(string)), "Call should be made to ContractA")
		require.Equal(t, "0xf18c388a", firstInnerTx["input"].(string), "Should call triggerCall() function")

		if len(innerTxList) >= 2 {
			secondInnerTx := innerTxList[1].(map[string]interface{})
			require.Equal(t, "call", secondInnerTx["call_type"], "Should be a call transaction")
			require.Equal(t, strings.ToLower(contractAAddr.Hex()), strings.ToLower(secondInnerTx["from"].(string)), "Call should originate from ContractA")
			require.Equal(t, strings.ToLower(contractBAddr.Hex()), strings.ToLower(secondInnerTx["to"].(string)), "Call should be made to ContractB")
			require.Equal(t, "0x32e43a11", secondInnerTx["input"].(string), "Should call dummy() function")
			require.NotNil(t, secondInnerTx["name"], "Inner transaction should have a name")

			name := secondInnerTx["name"].(string)
			lastChar := name[len(name)-1]
			require.True(t, lastChar >= '0' && lastChar <= '9', "Nested inner transaction name should end with a number (e.g., call_0, call_1)")

			t.Logf("✅ Successfully captured internal call from ContractA to ContractB!")
		}
	} else {
		t.Fatal("No inner transactions found in preexec result")
	}

	t.Logf("✅ eth_transactionPreExec successfully executed with inner transactions!")
	t.Logf("📋 Contract A address: %s", contractAAddr.Hex())
	t.Logf("📋 Contract B address: %s", contractBAddr.Hex())
	t.Logf("📋 Calldata for triggerCall(): 0x%x", calldata)

}
