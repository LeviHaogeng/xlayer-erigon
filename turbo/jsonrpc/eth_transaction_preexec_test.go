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

// Global variables to store deployed contract addresses
var (
	contractAAddr     common.Address
	contractBAddr     common.Address
	factoryAddr       common.Address
	deploymentAddress common.Address
	contractsDeployed bool
)

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

	to := common.HexToAddress(toAddress)
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

func deployContract(t *testing.T, ctx context.Context, client *ethclient.Client, privateKey *ecdsa.PrivateKey, contractName, abiJson, bytecodeStr string, constructorArgs ...interface{}) common.Address {
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

	fromAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
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

	if len(innerTxList) >= 2 {
		secondInnerTx := innerTxList[1].(map[string]interface{})
		require.Equal(t, "call", secondInnerTx["call_type"])
		require.Equal(t, strings.ToLower(contractAAddr.Hex()), strings.ToLower(secondInnerTx["from"].(string)))
		require.Equal(t, strings.ToLower(contractBAddr.Hex()), strings.ToLower(secondInnerTx["to"].(string)))
		require.Equal(t, "0x32e43a11", secondInnerTx["input"].(string))

		name := secondInnerTx["name"].(string)
		require.True(t, name[len(name)-1] >= '0' && name[len(name)-1] <= '9')
	}
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

	fromAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
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

	fromAddr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

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

	firstResult := preExecResults[0]
	if errorField, exists := firstResult["error"]; exists && errorField != nil {
		errorMap, ok := errorField.(map[string]interface{})
		if ok && errorMap["code"] != nil {
			if codeFloat, isFloat := errorMap["code"].(float64); isFloat && int(codeFloat) != 0 {
				t.Errorf("First transaction should not have error: %v", errorField)
			}
		}
	}

	secondResult := preExecResults[1]
	require.NotNil(t, secondResult["error"])
	errorMap := secondResult["error"].(map[string]interface{})
	errorCode := int(errorMap["code"].(float64))
	require.Equal(t, 1003, errorCode)
	require.Contains(t, errorMap["msg"].(string), "nonce decreases")
	require.Contains(t, errorMap["msg"].(string), fromAddr.Hex())
}
