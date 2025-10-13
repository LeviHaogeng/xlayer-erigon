package rpchelper

import "strings"

// isTxPoolDisabledErr returns true if err represents a disabled txpool without importing zk/txpool
// It checks error text to avoid a package dependency which would create an import cycle.
func isTxPoolDisabledErr(err error) bool {
	if err == nil {
		return false
	}
	// Known messages used by txpool when disabled
	// - fmt.Errorf("TxPool Disabled") in zk/txpool/txpool_grpc_server.go
	// - wrapped errors may contain this substring
	const disabledMsg = "TxPool Disabled"
	return strings.Contains(err.Error(), disabledMsg)
}
