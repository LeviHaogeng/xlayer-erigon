package txpool

import (
	"fmt"
	"math"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/fixedgas"
	"github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/log/v3"
)

type txRlp struct {
	Tx      []byte
	TxId    common.Hash
	Sender  common.Address
	IsLocal bool

	gasLimit   uint64
	isOutQuota bool
}

type hugeTxGasUsage struct {
	// gas left for huge txs in quote
	inQuotaGasLeft uint64
	// total gas used for huge txs out quote(i.e. huge txs that can't be included in quote)
	outQuotaGasUsed uint64
}

type readContext struct {
	txs []txRlp
	// index of out quote txs in txs
	outQuoteTxsIndex []int

	// max count of txs that can be included in txs
	txsMaxCount int

	// to define if a tx is huge tx
	hugeTxMinimalGas uint64

	hugeTxGasUsage hugeTxGasUsage
	// gas left for normal txs and huge txs in quote, not include huge txs out quote
	totalAvailableGas uint64
	// blob gas left
	availableBlobGas uint64

	toSkip   mapset.Set[[32]byte]
	toRemove []*metaTx

	blockNumber uint64
}

func NewReadContext(blockNumber uint64, txsMaxCount int, hugeTxMinimalGas, hugeTxGasQuota, totalAvailableGas, availableBlobGas uint64, toSkip mapset.Set[[32]byte]) *readContext {
	return &readContext{
		txs: make([]txRlp, 0, txsMaxCount),

		txsMaxCount: txsMaxCount,

		hugeTxMinimalGas: hugeTxMinimalGas,
		hugeTxGasUsage: hugeTxGasUsage{
			inQuotaGasLeft:  hugeTxGasQuota,
			outQuotaGasUsed: 0,
		},
		totalAvailableGas: totalAvailableGas,
		availableBlobGas:  availableBlobGas,

		toSkip:   toSkip,
		toRemove: make([]*metaTx, 0),

		blockNumber: blockNumber,
	}
}

func (ctx *readContext) isSameBlockNumber(blockNumber uint64) bool {
	return ctx.blockNumber == blockNumber
}

func (ctx *readContext) resetRead(txsMaxCount int, totalAvailableGas, availableBlobGas uint64) {
	ctx.txsMaxCount = txsMaxCount
	ctx.totalAvailableGas = totalAvailableGas
	ctx.availableBlobGas = availableBlobGas
}

func (ctx *readContext) SetIncludeAllTxs() {
	ctx.hugeTxMinimalGas = math.MaxUint64
	ctx.hugeTxGasUsage.inQuotaGasLeft = ctx.totalAvailableGas
}

func (ctx *readContext) SetHugeTxQuota(hugeTxMinimalGas, inQuotaGasLeft uint64) {
	ctx.hugeTxMinimalGas = hugeTxMinimalGas
	ctx.hugeTxGasUsage.inQuotaGasLeft = inQuotaGasLeft
}

func (ctx *readContext) AddTx(rlpTx []byte, txId common.Hash, sender common.Address, isLocal bool, gasLimit uint64, intrinsicGas uint64) bool {
	if ctx.isHugeTx(gasLimit) {
		return ctx.processHugeTx(rlpTx, txId, sender, isLocal, gasLimit, intrinsicGas)
	} else {
		return ctx.processNormalTx(rlpTx, txId, sender, isLocal, gasLimit, intrinsicGas)
	}
}

func (ctx *readContext) AddToSkip(txId common.Hash) {
	ctx.toSkip.Add(txId)
}

func (ctx *readContext) AddToRemove(tx *metaTx) {
	ctx.toRemove = append(ctx.toRemove, tx)
}

func (ctx *readContext) AddToSkipAndRemove(tx *metaTx) {
	ctx.AddToSkip(tx.Tx.IDHash)
	ctx.AddToRemove(tx)
}

func (ctx *readContext) IsFullfilled() bool {
	availableTxs := len(ctx.txs) - len(ctx.outQuoteTxsIndex)
	return availableTxs >= ctx.txsMaxCount || ctx.totalAvailableGas < fixedgas.TxGas
}

func (ctx *readContext) IsSkip(txId *[32]byte) bool {
	return ctx.toSkip.Contains(*txId)
}

func (ctx *readContext) ConsumeBlobGas(blobCount uint64) bool {
	blobGas := blobCount * fixedgas.BlobGasPerBlob
	if blobGas > ctx.availableBlobGas {
		return false
	}
	ctx.availableBlobGas -= blobGas
	return true
}

func (ctx *readContext) Finalize(txs *types.TxsRlp) int {
	if !ctx.IsFullfilled() {
		ctx.tryFullfill()
	}

	txs.Resize(uint(len(ctx.txs)))
	count := 0
	for _, tx := range ctx.txs {
		if tx.isOutQuota {
			continue
		}

		txs.Txs[count] = tx.Tx
		txs.TxIds[count] = tx.TxId
		copy(txs.Senders.At(count), tx.Sender.Bytes())
		txs.IsLocal[count] = tx.IsLocal
		count++
	}

	txs.Resize(uint(count))
	return count
}

func (ctx *readContext) tryFullfill() {
	for i := 0; i < len(ctx.outQuoteTxsIndex); {
		if ctx.IsFullfilled() {
			break
		}

		txsIndex := ctx.outQuoteTxsIndex[i]

		if ctx.totalAvailableGas < ctx.txs[txsIndex].gasLimit {
			// we'd like to select a smaller one
			i++
			continue
		}
		ctx.totalAvailableGas -= ctx.txs[txsIndex].gasLimit
		ctx.hugeTxGasUsage.outQuotaGasUsed -= ctx.txs[txsIndex].gasLimit
		ctx.txs[txsIndex].isOutQuota = false

		ctx.AddToSkip(ctx.txs[txsIndex].TxId)

		// remove current value, so we don't increment index
		ctx.outQuoteTxsIndex = append(ctx.outQuoteTxsIndex[:i], ctx.outQuoteTxsIndex[i+1:]...)
	}
}

func (ctx *readContext) isHugeTx(gasLimit uint64) bool {
	return gasLimit >= ctx.hugeTxMinimalGas
}

func (ctx *readContext) processHugeTx(rlpTx []byte, txId common.Hash, sender common.Address, isLocal bool, gasLimit, intrinsicGas uint64) bool {
	isOutQuota := ctx.hugeTxGasUsage.inQuotaGasLeft < gasLimit

	if isOutQuota {
		// quote of huge tx is not available, try to add it to out quote list
		if ctx.totalAvailableGas < ctx.hugeTxGasUsage.outQuotaGasUsed {
			// it's not necessary to add any more huge txs to out quote list.
			// for the currently out of quote huge txs can supply the lack of normal txs
			log.Debug(fmt.Sprintf("[%s]Huge tx is out of quote, not necessary to be included in quote", logPrefix),
				"txId", txId, "gasLimit", gasLimit, "totalAvailableGas", ctx.totalAvailableGas, "outQuotaGasUsed", ctx.hugeTxGasUsage.outQuotaGasUsed)
			return false
		}
		ctx.hugeTxGasUsage.outQuotaGasUsed += gasLimit
	} else {
		// quote of huge tx is available, try to add it to in quote list
		if !ctx.adjustTotalAvailableGas(gasLimit) {
			log.Debug(fmt.Sprintf("[%s]adjust total available gas for huge tx failed", logPrefix), "txId", txId, "gasLimit", gasLimit, "isOutQuota", isOutQuota, "totalAvailableGas", ctx.totalAvailableGas)
			return false
		}
		ctx.hugeTxGasUsage.inQuotaGasLeft -= gasLimit
	}

	log.Debug(fmt.Sprintf("[%s]Append huge tx to read context", logPrefix), "txId", txId, "gasLimit", gasLimit, "isOutQuota", isOutQuota, "totalAvailableGas", ctx.totalAvailableGas, "inQuotaGasLeft", ctx.hugeTxGasUsage.inQuotaGasLeft, "outQuotaGasUsed", ctx.hugeTxGasUsage.outQuotaGasUsed)
	ctx.appendTx(rlpTx, txId, sender, isLocal, gasLimit, isOutQuota)
	return true
}

func (ctx *readContext) processNormalTx(rlpTx []byte, txId common.Hash, sender common.Address, isLocal bool, gasLimit, intrinsicGas uint64) bool {
	if !ctx.adjustTotalAvailableGas(intrinsicGas) {
		log.Debug(fmt.Sprintf("[%s]adjust total available gas for normal tx failed", logPrefix),
			"txId", txId, "intrinsicGas", intrinsicGas, "totalAvailableGas", ctx.totalAvailableGas)
		return false
	}

	log.Debug(fmt.Sprintf("[%s]Append normal tx to read context", logPrefix),
		"txId", txId, "intrinsicGas", intrinsicGas, "totalAvailableGas", ctx.totalAvailableGas)
	ctx.appendTx(rlpTx, txId, sender, isLocal, gasLimit, false)
	return true
}

func (ctx *readContext) adjustTotalAvailableGas(intrinsicGas uint64) bool {
	if ctx.totalAvailableGas < intrinsicGas {
		// can't adjust total available gas,
		// we might find another TX with a low enough intrinsic gas to include
		return false
	}
	ctx.totalAvailableGas -= intrinsicGas
	return true
}

func (ctx *readContext) appendTx(rlpTx []byte, txId common.Hash, sender common.Address, isLocal bool, gasLimit uint64, isOutQuote bool) {
	tx := txRlp{
		Tx:      rlpTx,
		TxId:    txId,
		Sender:  sender,
		IsLocal: isLocal,

		gasLimit:   gasLimit,
		isOutQuota: isOutQuote,
	}

	if isOutQuote {
		ctx.outQuoteTxsIndex = append(ctx.outQuoteTxsIndex, len(ctx.txs))
	}

	ctx.txs = append(ctx.txs, tx)
	ctx.AddToSkip(txId)
}
