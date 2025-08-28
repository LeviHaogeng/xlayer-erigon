package operations

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/ethclient"
	"github.com/stretchr/testify/require"
)

// GasConsumer contract in Solidity (for reference):
/*
pragma solidity ^0.8.19;

contract GasConsumer {
    uint256[] private dataArray;
    uint256 public totalConsumed;

    event GasConsumed(uint256 level, uint256 gasUsed);

    // Constructor to initialize array to avoid high initial costs
    constructor() {
        dataArray.push(1);
        totalConsumed = 0;
    }

    // Method to consume gas
    function consumeGas(uint256 level) external {
        require(level == 0 || level == 1, "Level must be 0 (mid) or 1 (high)");

        uint256 gasStart = gasleft();
        if (level == 0) {
            // Medium gas consumption: 20 loops with minimal storage
            for (uint256 i = 0; i < 20; i++) {
                dataArray.push(i * 2); // Store to array, increases gas cost
                totalConsumed += i; // Update state variable
            }
        } else {
            // High gas consumption: 100 loops with more storage and computation
            for (uint256 i = 0; i < 100; i++) {
                dataArray.push(i * i); // Square calculation, increases gas
                totalConsumed += i * i; // More complex update
                if (i % 10 == 0) {
                    // Every 10 iterations, additional storage to increase gas cost
                    dataArray.push(totalConsumed);
                }
            }
        }
        uint256 gasUsed = gasStart - gasleft();
        emit GasConsumed(level, gasUsed);
    }

    // View current array length (for debugging)
    function getArrayLength() external view returns (uint256) {
        return dataArray.length;
    }

    // View total consumed value (for debugging)
    function getTotalConsumed() external view returns (uint256) {
        return totalConsumed;
    }
}
*/

// GasConsumer contract bytecode - properly consumes gas through array operations
// This contract has a consumeGas function that takes level parameter (0=medium, 1=high)
const GasBurnerBytecode = "0x608060405234801561001057600080fd5b5060006001908060018154018082558091505060019003906000526020600020016000909190919091505560006001819055506105cd806100526000396000f3fe608060405234801561001057600080fd5b506004361061004c5760003560e01c80630849cc99146100515780631000d0091461006f57806394d9da371461008d578063a329e8de146100ab575b600080fd5b6100596100c7565b60405161006691906102d1565b60405180910390f35b6100776100d3565b60405161008491906102d1565b60405180910390f35b6100956100d9565b6040516100a291906102d1565b60405180910390f35b6100c560048036038101906100c0919061031d565b6100e3565b005b60008080549050905090565b60015481565b6000600154905090565b60008114806100f25750600181145b610131576040517f08c379a0000000000000000000000000000000000000000000000000000000008152600401610128906103cd565b60405180910390fd5b60005a9050600082036101b15760005b60148110156101ab576000600282610159919061041c565b90806001815401808255809150506001900390600052602060002001600090919091909150558060016000828254610191919061045e565b9250508190555080806101a390610492565b915050610141565b5061026a565b60005b606481101561026857600081826101cb919061041c565b908060018154018082558091505060019003906000526020600020016000909190919091505580816101fd919061041c565b6001600082825461020e919061045e565b925050819055506000600a826102249190610509565b0361025557600060015490806001815401808255809150506001900390600052602060002001600090919091909150555b808061026090610492565b9150506101b4565b505b60005a82610278919061053a565b90507ff111d3174cf905dfe32c6e23405f873b8afa292dee117c5d31958183453b3af983826040516102ab92919061056e565b60405180910390a1505050565b6000819050919050565b6102cb816102b8565b82525050565b60006020820190506102e660008301846102c2565b92915050565b600080fd5b6102fa816102b8565b811461030557600080fd5b50565b600081359050610317816102f1565b92915050565b600060208284031215610333576103326102ec565b5b600061034184828501610308565b91505092915050565b600082825260208201905092915050565b7f4c6576656c206d757374206265203020286d696429206f72203120286869676860008201527f2900000000000000000000000000000000000000000000000000000000000000602082015250565b60006103b760218361034a565b91506103c28261035b565b604082019050919050565b600060208201905081810360008301526103e6816103aa565b9050919050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052601160045260246000fd5b6000610427826102b8565b9150610432836102b8565b9250828202610440816102b8565b91508282048414831517610457576104566103ed565b5b5092915050565b6000610469826102b8565b9150610474836102b8565b925082820190508082111561048c5761048b6103ed565b5b92915050565b600061049d826102b8565b91507fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff82036104cf576104ce6103ed565b5b600182019050919050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052601260045260246000fd5b6000610514826102b8565b915061051f836102b8565b92508261052f5761052e6104da565b5b828206905092915050565b6000610545826102b8565b9150610550836102b8565b9250828203905081811115610568576105676103ed565b5b92915050565b600060408201905061058360008301856102c2565b61059060208301846102c2565b939250505056fea26469706673582212201d650bf9de8cc8f41544c2baf6bcee3382927f52b0afefa92e1ac5ee8ad73e4864736f6c63430008130033"

// GasConsumer contract ABI
const GasBurnerABI = `[
	{
		"inputs": [],
		"stateMutability": "nonpayable",
		"type": "constructor"
	},
	{
		"anonymous": false,
		"inputs": [
			{
				"indexed": false,
				"internalType": "uint256",
				"name": "level",
				"type": "uint256"
			},
			{
				"indexed": false,
				"internalType": "uint256",
				"name": "gasUsed",
				"type": "uint256"
			}
		],
		"name": "GasConsumed",
		"type": "event"
	},
	{
		"inputs": [
			{
				"internalType": "uint256",
				"name": "level",
				"type": "uint256"
			}
		],
		"name": "consumeGas",
		"outputs": [],
		"stateMutability": "nonpayable",
		"type": "function"
	},
	{
		"inputs": [],
		"name": "getArrayLength",
		"outputs": [
			{
				"internalType": "uint256",
				"name": "",
				"type": "uint256"
			}
		],
		"stateMutability": "view",
		"type": "function"
	},
	{
		"inputs": [],
		"name": "getTotalConsumed",
		"outputs": [
			{
				"internalType": "uint256",
				"name": "",
				"type": "uint256"
			}
		],
		"stateMutability": "view",
		"type": "function"
	},
	{
		"inputs": [],
		"name": "totalConsumed",
		"outputs": [
			{
				"internalType": "uint256",
				"name": "",
				"type": "uint256"
			}
		],
		"stateMutability": "view",
		"type": "function"
	}
]`

var (
	gasConsumerAddress  common.Address
	gasConsumerDeployed bool
)

// contractConfig holds the deployed contract information
type contractConfig struct {
	Address  string `json:"address"`
	Deployed bool   `json:"deployed"`
}

// getConfigPath returns the path to the contract config file
func getConfigPath() string {
	// Use relative path from project root
	return "tmp/gas_consumer_contract.json"
}

// loadContractConfig loads the contract configuration from file
func loadContractConfig() *contractConfig {
	configPath := getConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return &contractConfig{}
	}

	var config contractConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return &contractConfig{}
	}
	return &config
}

// saveContractConfig saves the contract configuration to file
func saveContractConfig(address common.Address) {
	config := &contractConfig{
		Address:  address.Hex(),
		Deployed: true,
	}

	data, err := json.Marshal(config)
	if err != nil {
		return
	}

	configPath := getConfigPath()
	// Create tmp directory if it doesn't exist
	os.MkdirAll("tmp", 0755)
	os.WriteFile(configPath, data, 0644)
}

// GetGasConsumerContractAddress returns the deployed gas consumer contract address
// First checks cached address, then config file, then falls back to deployment
func GetGasConsumerContractAddress(t *testing.T, client *ethclient.Client) common.Address {
	// Check if we have a cached address
	if gasConsumerDeployed && gasConsumerAddress != (common.Address{}) {
		return gasConsumerAddress
	}

	// Check config file for contract address
	config := loadContractConfig()
	if config.Deployed && config.Address != "" {
		addr := common.HexToAddress(config.Address)
		// Verify the contract exists at this address
		code, err := client.CodeAt(context.Background(), addr, nil)
		if err == nil && len(code) > 0 {
			gasConsumerAddress = addr
			gasConsumerDeployed = true
			t.Logf("Using existing gas consumer contract from config: %s", addr.Hex())
			return addr
		}
		t.Logf("Contract not found at config address %s, will deploy new one", addr.Hex())
	}

	// Deploy new contract if none exists
	return DeployGasConsumeContract(t, client)
}

// DeployGasConsumeContract deploys the gas consume contract
func DeployGasConsumeContract(t *testing.T, client *ethclient.Client) common.Address {
	if gasConsumerDeployed {
		return gasConsumerAddress
	}

	ctx := context.Background()

	// Get admin private key for deployment
	adminPrivKey, err := crypto.HexToECDSA(strings.TrimPrefix(DefaultL2AdminPrivateKey, "0x"))
	require.NoError(t, err)
	adminAddr := crypto.PubkeyToAddress(adminPrivKey.PublicKey)

	// Get nonce
	nonce, err := client.PendingNonceAt(ctx, adminAddr)
	require.NoError(t, err)

	// Create contract deployment transaction
	gasPrice := big.NewInt(2e9) // 2 Gwei

	tx := &types.LegacyTx{
		CommonTx: types.CommonTx{
			Nonce: nonce,
			To:    nil, // nil for contract deployment
			Value: uint256.NewInt(0),
			Gas:   1500000, // 1.5M gas for deployment (under block limit)
			Data:  common.FromHex(GasBurnerBytecode),
		},
		GasPrice: uint256.MustFromBig(gasPrice),
	}

	signer := types.MakeSigner(GetTestChainConfig(DefaultL2ChainID), 1, 0)
	signedTx, err := types.SignTx(tx, *signer, adminPrivKey)
	require.NoError(t, err)

	// Send deployment transaction
	err = client.SendTransaction(ctx, signedTx)
	require.NoError(t, err)

	t.Logf("Deploying gas consumer contract, tx: %s", signedTx.Hash().Hex())

	// Wait for deployment using the proper method
	err = WaitTxToBeMined(ctx, client, signedTx, DefaultTimeoutTxToBeMined)
	require.NoError(t, err, "Contract deployment failed")

	t.Logf("Gas consumer contract deployed successfully")

	// Get receipt to check deployment status and get contract address
	receipt, err := client.TransactionReceipt(ctx, signedTx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Status, "Contract deployment failed")

	gasConsumerAddress = receipt.ContractAddress
	gasConsumerDeployed = true

	// Save contract address to config file for future tests
	saveContractConfig(gasConsumerAddress)
	t.Logf("Gas consumer contract deployed at: %s (saved to config)", gasConsumerAddress.Hex())

	// Perform a warmup call to stabilize gas consumption for subsequent calls
	t.Logf("Performing warmup call to stabilize gas consumption...")
	warmupTx := CreateGasConsumeTx(t, client, gasConsumerAddress, adminPrivKey, 600000, 2, 0) // level 0 with 600k gas limit

	err = client.SendTransaction(ctx, warmupTx)
	require.NoError(t, err)
	t.Logf("Warmup transaction sent: %s", warmupTx.Hash().Hex())

	// Wait for warmup transaction to be mined
	err = WaitTxToBeMined(ctx, client, warmupTx, DefaultTimeoutTxToBeMined)
	require.NoError(t, err)

	// Get receipt to verify warmup call succeeded
	warmupReceipt, err := client.TransactionReceipt(ctx, warmupTx.Hash())
	require.NoError(t, err)
	require.Equal(t, uint64(1), warmupReceipt.Status, "Warmup call failed")

	t.Logf("Warmup call completed successfully, gas used: %d", warmupReceipt.GasUsed)
	t.Logf("Contract is now warmed up for stable gas consumption testing")

	return gasConsumerAddress
}

// CreateGasConsumeTx creates a transaction that calls the gas consumer contract
func CreateGasConsumeTx(
	t *testing.T,
	client *ethclient.Client,
	contractAddr common.Address,
	privateKey *ecdsa.PrivateKey,
	gasLimit uint64,
	gasPrice uint64,
	level uint64,
) types.Transaction {

	ctx := context.Background()
	from := crypto.PubkeyToAddress(privateKey.PublicKey)

	// Get nonce
	nonce, err := client.PendingNonceAt(ctx, from)
	require.NoError(t, err)

	var data []byte
	if level == 0 {
		//  Parameter: 0 (level 0 for high gas consumption)
		data = common.FromHex("0xa329e8de0000000000000000000000000000000000000000000000000000000000000000")
	} else {
		//  Parameter: 1 (level 1 for very high gas consumption)
		data = common.FromHex("0xa329e8de0000000000000000000000000000000000000000000000000000000000000001")
	}

	tx := &types.LegacyTx{
		CommonTx: types.CommonTx{
			Nonce: nonce,
			To:    &contractAddr, // Call the gas consumer contract
			Value: uint256.NewInt(0),
			Gas:   gasLimit,
			Data:  data, // Call consumeGas(0)
		},
		GasPrice: uint256.NewInt(gasPrice * 1e9), // Convert Gwei to wei
	}

	signer := types.MakeSigner(GetTestChainConfig(DefaultL2ChainID), 1, 0)
	signedTx, err := types.SignTx(tx, *signer, privateKey)
	require.NoError(t, err)

	return signedTx
}
