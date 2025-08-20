package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon/accounts/abi"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/tracers"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/adapter/ethapi"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	zktypes "github.com/ledgerwatch/erigon/zk/types"
)

const (
	UnKnownErrCode             = 1000
	InsufficientBalanceErrCode = 1001
	RevertedErrCode            = 1002
	CheckPreArgsErrCode        = 1003

	MaxGasLimit = 30000000
)

// PreArgs represents the arguments for transaction pre-execution
type PreArgs struct {
	ChainId              *big.Int          `json:"chainId,omitempty"`
	From                 *common.Address   `json:"from"`
	To                   *common.Address   `json:"to"`
	Gas                  *hexutil.Uint64   `json:"gas"`
	GasPrice             *hexutil.Big      `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big      `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big      `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big      `json:"value"`
	Nonce                *hexutil.Uint64   `json:"nonce"`
	Data                 *hexutility.Bytes `json:"data"`
	Input                *hexutility.Bytes `json:"input"`
}

func (args PreArgs) ToLogString() string {
	argsBytes, _ := json.Marshal(args)
	return string(argsBytes)
}

// PreError represents an error in pre-execution
type PreError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// StateAccount represents an account state for prestate tracer
type StateAccount struct {
	Balance string            `json:"balance"`
	Code    string            `json:"code"`
	Nonce   uint64            `json:"nonce"`
	Storage map[string]string `json:"storage"`
}

func toPreError(err error, result *core.ExecutionResult) PreError {
	preErr := PreError{
		Code: UnKnownErrCode,
	}
	if err != nil {
		preErr.Msg = err.Error()
	}
	if result != nil && result.Err != nil {
		preErr.Msg = result.Err.Error()
	}
	if strings.HasPrefix(preErr.Msg, "execution reverted") {
		preErr.Code = RevertedErrCode
		if result != nil {
			preErr.Msg, _ = abi.UnpackRevert(result.Revert())
		}
	}
	if strings.HasPrefix(preErr.Msg, "out of gas") {
		preErr.Code = RevertedErrCode
	}
	if strings.HasPrefix(preErr.Msg, "insufficient funds") {
		preErr.Code = InsufficientBalanceErrCode
	}
	return preErr
}

// PreResult represents the result of transaction pre-execution
type PreResult struct {
	InnerTxs    interface{} `json:"innerTxs"`
	Logs        interface{} `json:"logs"`
	StateDiff   interface{} `json:"stateDiff"`
	Error       PreError    `json:"error"`
	GasUsed     uint64      `json:"gasUsed"`
	BlockNumber *big.Int    `json:"blockNumber"`
}

func (res PreResult) ToLogString() string {
	// Simplified logging - just marshal basic result
	resBytes, _ := json.Marshal(res)
	if len(resBytes) > 500 {
		return string(resBytes[:500]) + "..."
	}
	return string(resBytes)
}

func toPreResult(innerTxs []*zktypes.InnerTx, logs []*types.Log, stateDiff map[string]interface{},
	preError PreError, gasUsed uint64, number *big.Int) PreResult {
	preResult := PreResult{
		Error:       preError,
		GasUsed:     gasUsed,
		BlockNumber: number,
	}
	if len(innerTxs) > 0 {
		preResult.InnerTxs = innerTxs
	} else {
		preResult.InnerTxs = make([]*zktypes.InnerTx, 0)
	}
	if len(logs) > 0 {
		preResult.Logs = logs
	} else {
		preResult.Logs = make([]*types.Log, 0)
	}
	if len(stateDiff) > 0 {
		preResult.StateDiff = stateDiff
	} else {
		preResult.StateDiff = make(map[string]interface{})
	}

	return preResult
}

// TransactionPreExec executes multiple transactions in sequence and returns their execution results
func (api *APIImpl) TransactionPreExec(ctx context.Context, origins []PreArgs, stateOverrides *ethapi.StateOverrides) ([]PreResult, error) {
	start := time.Now()
	requestID := uuid.NewString()
	defer func(s time.Time, id string) {
		log.Info("Executing TransactionPreExec call finished", "requestID", id, "runtime", time.Since(s))
	}(start, requestID)

	preResList := make([]PreResult, 0)

	// Get database transaction
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Get latest block state
	blockNrOrHash := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	blockNumber, _, _, err := rpchelper.GetBlockNumber(blockNrOrHash, tx, api.filters)
	if err != nil {
		return nil, err
	}

	// Create state reader
	stateReader, err := rpchelper.CreateStateReader(ctx, tx, blockNrOrHash, 0, api.filters, api.stateCache, api.historyV3(tx), "")
	if err != nil {
		return nil, err
	}

	// Get header
	header, err := api._blockReader.HeaderByNumber(ctx, tx, blockNumber)
	if err != nil {
		return nil, err
	}
	if header == nil {
		return nil, fmt.Errorf("block header not found: %d", blockNumber)
	}

	// Create state
	ibs := state.New(stateReader)

	// Apply state overrides if provided
	if stateOverrides != nil {
		log.Info("TransactionPreExec: applying state overrides", "requestID", requestID, "overrides", len(*stateOverrides))
		err = stateOverrides.Override(ibs)
		if err != nil {
			return nil, err
		}
		// Log the state after overrides
		for addr, override := range *stateOverrides {
			if override.Balance != nil {
				log.Info("TransactionPreExec: state override applied", "requestID", requestID, "address", addr.Hex())
			}
		}
	}

	blockBigNumber := new(big.Int).Set(header.Number)

	// Setup context with timeout
	timeout := 5 * time.Second
	if len(origins) > 0 {
		timeout = time.Duration(len(origins)) * timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Process each transaction
	for i, origin := range origins {
		var gasUsed uint64
		log.Info("TransactionPreExec", "requestID", requestID, "input index", i, "input args", origin.ToLogString())

		// Basic parameter validation
		if origin.From == nil {
			preError := PreError{
				Code: CheckPreArgsErrCode,
				Msg:  "from address is required",
			}
			preResult := toPreResult(nil, nil, nil, preError, gasUsed, blockBigNumber)
			preResList = append(preResList, preResult)
			continue
		}

		// Set default gas if not provided
		if origin.Gas == nil {
			gas := uint64(MaxGasLimit)
			origin.Gas = (*hexutil.Uint64)(&gas)
		} else if uint64(*origin.Gas) > MaxGasLimit {
			gas := uint64(MaxGasLimit)
			origin.Gas = (*hexutil.Uint64)(&gas)
		}

		// Get chain config
		chainConfig, err := api.chainConfig(ctx, tx)
		if err != nil {
			preError := PreError{
				Code: UnKnownErrCode,
				Msg:  fmt.Sprintf("failed to get chain config: %v", err),
			}
			preResult := toPreResult(nil, nil, nil, preError, gasUsed, blockBigNumber)
			preResList = append(preResList, preResult)
			continue
		}

		// Convert to transaction args
		txArgs := ethapi.CallArgs{
			From:                 origin.From,
			To:                   origin.To,
			Gas:                  origin.Gas,
			GasPrice:             origin.GasPrice,
			MaxFeePerGas:         origin.MaxFeePerGas,
			MaxPriorityFeePerGas: origin.MaxPriorityFeePerGas,
			Value:                origin.Value,
			Data:                 origin.Data,
			Input:                origin.Input,
		}

		// Convert to message
		var baseFee *uint256.Int
		if header.BaseFee != nil {
			var overflow bool
			baseFee, overflow = uint256.FromBig(header.BaseFee)
			if overflow {
				log.Error("TransactionPreExec: header.BaseFee uint256 overflow", "requestID", requestID)
				preError := PreError{
					Code: UnKnownErrCode,
					Msg:  "header.BaseFee uint256 overflow",
				}
				preResult := toPreResult(nil, nil, nil, preError, gasUsed, blockBigNumber)
				preResList = append(preResList, preResult)
				continue
			}
		}
		msg, err := txArgs.ToMessage(api.GasCap, baseFee)
		if err != nil {
			log.Error("TransactionPreExec: tx args to message failed", "requestID", requestID, "error", err.Error())
			preError := PreError{
				Code: UnKnownErrCode,
				Msg:  err.Error(),
			}
			preResult := toPreResult(nil, nil, nil, preError, gasUsed, blockBigNumber)
			preResList = append(preResList, preResult)
			continue
		}

		// Create tracer for state diffs
		txHash := common.BigToHash(big.NewInt(int64(i)))
		traceConfig := []byte(`{
			"prestateTracer": {
				"diffMode": true
			},
			"callTracer": null
		}`)

		tracer, err := tracers.New("muxTracer", &tracers.Context{
			BlockHash: header.Hash(),
			TxIndex:   0,
			TxHash:    txHash,
		}, traceConfig)
		if err != nil {
			log.Error("TransactionPreExec: generate muxTracer failed", "requestID", requestID, "input args", origin.ToLogString(), "error", err.Error())
			// Continue without tracer instead of returning error
			tracer = nil
			log.Warn("TransactionPreExec: proceeding without tracer", "requestID", requestID)
		}

		// Create EVM
		vmconfig := vm.Config{
			Debug:      true, // CRITICAL: Enable debug mode for tracer to work
			NoBaseFee:  true,
			NoInnerTxs: false, // Enable inner transactions tracking
		}
		if tracer != nil {
			vmconfig.Tracer = tracer
		}

		getHashFunc := func(n uint64) common.Hash {
			h, _ := api._blockReader.HeaderByNumber(ctx, tx, n)
			if h != nil {
				return h.Hash()
			}
			return common.Hash{}
		}

		blockCtx := core.NewEVMBlockContext(header, getHashFunc, api.engine(), nil)
		txCtx := core.NewEVMTxContext(msg)

		// Create ZK-EVM instance for proper InnerTx tracking
		evm := vm.NewZkEVM(blockCtx, txCtx, ibs, chainConfig, vm.ZkConfig{Config: vmconfig})

		// Cancel the evm when context is done
		go func() {
			<-ctx.Done()
			evm.Cancel()
		}()

		// Execute the message
		gp := new(core.GasPool).AddGas(MaxGasLimit)
		ibs.SetTxContext(txHash, header.Hash(), i)

		// Create a dummy transaction for ApplyMessageWithTxContext
		var dummyTx types.Transaction
		if msg.To() != nil {
			dummyTx = types.NewTransaction(
				msg.Nonce(),
				*msg.To(),
				msg.Value(),
				msg.Gas(),
				msg.GasPrice(),
				msg.Data(),
			)
		} else {
			// Contract creation transaction
			dummyTx = types.NewContractCreation(
				msg.Nonce(),
				msg.Value(),
				msg.Gas(),
				msg.GasPrice(),
				msg.Data(),
			)
		}

		var usedGasVar uint64
		_, result, innerTxs, err := core.ApplyMessageWithTxContext(
			msg,
			txCtx,
			gp,
			ibs,
			state.NewNoopWriter(), // state writer
			header.Number,         // block number
			dummyTx,               // transaction
			&usedGasVar,           // used gas
			evm,
			true, // shouldFinalizeIbs
		)
		if result != nil {
			gasUsed = result.UsedGas
		}

		// Handle execution errors
		var preError PreError
		if err != nil {
			log.Warn("TransactionPreExec: execution failed", "requestID", requestID, "index", i, "error", err.Error())
			preError = toPreError(err, result)
		} else if result != nil && result.Failed() {
			preError = toPreError(result.Err, result)
		}

		var stateDiff map[string]interface{}

		// Get trace results from tracer if available
		if tracer != nil {
			if rawRes, err := tracer.GetResult(); err == nil {
				// muxTracer returns format: {"prestateTracer": {...}, "callTracer": ...}
				var muxResult map[string]json.RawMessage
				if err := json.Unmarshal(rawRes, &muxResult); err == nil {
					// Extract prestateTracer result
					if prestateRaw, exists := muxResult["prestateTracer"]; exists {
						var prestateResult interface{}
						if err := json.Unmarshal(prestateRaw, &prestateResult); err == nil {
							stateDiff = convertPrestateToStateDiff(prestateResult)
						} else {
							log.Warn("TransactionPreExec: failed to unmarshal prestateTracer result", "requestID", requestID, "error", err)
						}
					}
				} else {
					log.Warn("TransactionPreExec: failed to unmarshal muxTracer result", "requestID", requestID, "error", err)
				}
			} else {
				log.Warn("TransactionPreExec: failed to get tracer result", "requestID", requestID, "error", err)
			}
		}

		preRes := toPreResult(innerTxs, ibs.GetLogs(txHash), stateDiff, preError, gasUsed, blockBigNumber)
		preResList = append(preResList, preRes)
		log.Info("TransactionPreExec execute finished", "requestID", requestID, "index", i, "gasUsed", gasUsed)
	}

	return preResList, nil
}

func convertPrestateToStateDiff(traceResult interface{}) map[string]interface{} {
	if traceResult == nil {
		return make(map[string]interface{})
	}

	result := make(map[string]interface{})

	// First, try to convert to JSON and parse the structure
	stateDiffResultStr, err := json.Marshal(traceResult)
	if err != nil {
		log.Warn("Failed to marshal prestate tracer result", "error", err)
		return result
	}

	// Parse the prestateTracer result which has the format: {"pre": {...}, "post": {...}}
	var prestateResult struct {
		Pre  map[string]*StateAccount `json:"pre"`
		Post map[string]*StateAccount `json:"post"`
	}

	if err := json.Unmarshal(stateDiffResultStr, &prestateResult); err != nil {
		log.Warn("Failed to unmarshal prestate tracer result", "error", err)
		return result
	}

	if len(prestateResult.Pre) == 0 || len(prestateResult.Post) == 0 {
		return result
	}

	// Process each address that has changes
	for addr, postState := range prestateResult.Post {
		if preState, exist := prestateResult.Pre[addr]; exist {
			addrMap := make(map[string]interface{})
			preStateBalance, postStateBalance := new(big.Int), new(big.Int)

			// Parse pre-state balance
			if preState.Balance != "" {
				if strings.HasPrefix(preState.Balance, "0x") {
					preStateBalance, _ = big.NewInt(0).SetString(preState.Balance[2:], 16)
				} else {
					preStateBalance, _ = big.NewInt(0).SetString(preState.Balance, 10)
				}
			}

			// Parse post-state balance
			if postState.Balance != "" {
				if strings.HasPrefix(postState.Balance, "0x") {
					postStateBalance, _ = big.NewInt(0).SetString(postState.Balance[2:], 16)
				} else {
					postStateBalance, _ = big.NewInt(0).SetString(postState.Balance, 10)
				}
			} else {
				// If post balance is empty, use pre balance
				postStateBalance = preStateBalance
			}

			// Add balance changes
			balance := struct {
				Before string `json:"before"`
				After  string `json:"after"`
			}{
				Before: preStateBalance.String(),
				After:  postStateBalance.String(),
			}
			addrMap["balance"] = balance

			// Add nonce changes
			/*
				if preState.Nonce != postState.Nonce {
					nonce := struct {
						Before uint64 `json:"before"`
						After  uint64 `json:"after"`
					}{
						Before: preState.Nonce,
						After:  postState.Nonce,
					}
					addrMap["nonce"] = nonce
				}

				// Add code changes
				if preState.Code != postState.Code {
					code := struct {
						Before string `json:"before"`
						After  string `json:"after"`
					}{
						Before: preState.Code,
						After:  postState.Code,
					}
					addrMap["code"] = code
				}

				// Add storage changes
				storage := make(map[string]interface{})
				// Compare storage from pre and post states
				allKeys := make(map[string]bool)
				if preState.Storage != nil {
					for key := range preState.Storage {
						allKeys[key] = true
					}
				}
				if postState.Storage != nil {
					for key := range postState.Storage {
						allKeys[key] = true
					}
				}

				for key := range allKeys {
					var preval, postval string
					if preState.Storage != nil {
						preval = preState.Storage[key]
					}
					if postState.Storage != nil {
						postval = postState.Storage[key]
					}

					if preval != postval {
						storageChange := struct {
							Before string `json:"before"`
							After  string `json:"after"`
						}{
							Before: preval,
							After:  postval,
						}
						storage[key] = storageChange
					}
				}

				if len(storage) > 0 {
					addrMap["storage"] = storage
				}
			*/

			result[addr] = addrMap
		}
	}

	return result
}
