package subscription

import "github.com/ledgerwatch/erigon-lib/common"

type StreamCriteria struct {
	NewHeads             bool
	TransactionExtraInfo bool
	TransactionReceipt   bool
	TransactionInnerTxs  bool
	SubscribedAddresses  []common.Address
}
