package client

import (
	"encoding/binary"
)

const (
	// Commands
	CmdUnknown Command = iota
	CmdStart
	CmdStop
	CmdHeader
	CmdStartBookmark // CmdStartBookmark for the start from bookmark TCP client command
	CmdEntry         // CmdEntry for the get entry TCP client command
	CmdBookmark      // CmdBookmark for the get bookmark TCP client command
)

const (
	// Custom X Layer commands - use 1000+ range to avoid conflicts with upstream
	CmdLatestL2Block      Command = 1001 // CmdLatestL2Block for the optimized get latest L2Block command
	CmdStartBookmarkBatch Command = 1002 // CmdStartBookmarkBatch for the optimized batch streaming from bookmark
)

// sendHeaderCmd sends the header command to the server.
func (c *StreamClient) sendHeaderCmd() error {
	return c.sendCommand(CmdHeader)
}

// sendBookmarkCmd sends either CmdStartBookmark or CmdBookmark for the provided bookmark value.
// In case streaming parameter is set to true, the CmdStartBookmark is sent, otherwise the CmdBookmark.
func (c *StreamClient) sendBookmarkCmd(bookmark []byte, streaming bool) error {
	// in case we want to stream the entries, CmdStartBookmark is sent, otherwise CmdBookmark command
	command := CmdStartBookmark
	if !streaming {
		command = CmdBookmark
	}

	// Send the command
	if err := c.sendCommand(command); err != nil {
		return err
	}

	// Send bookmark length
	if err := c.writeToConn(uint32(len(bookmark))); err != nil {
		return err
	}

	// Send the bookmark to retrieve
	return c.writeToConn(bookmark)
}

// sendStartCmd sends a start command to the server, indicating
// that the client wishes to start streaming from the given entry number.
func (c *StreamClient) sendStartCmd(from uint64) error {
	if err := c.sendCommand(CmdStart); err != nil {
		return err
	}

	// Send starting/from entry number
	return c.writeToConn(from)
}

// sendEntryCmd sends the get data stream entry by number command to a TCP connection
func (c *StreamClient) sendEntryCmd(entryNum uint64) error {
	// Send CmdEntry command
	if err := c.sendCommand(CmdEntry); err != nil {
		return err
	}

	// Send entry number
	return c.writeToConn(entryNum)
}

// sendLatestL2BlockCmd sends the optimized get latest L2Block command to the server.
func (c *StreamClient) sendLatestL2BlockCmd() error {
	return c.sendCommand(CmdLatestL2Block)
}

// sendBookmarkBatchCmd sends the optimized batch streaming command for the provided bookmark value.
// This replaces the need for separate stopStreaming + GetHeader + initiateDownloadBookmark calls
// Optimized for high-latency networks by sending all data in a single packet (reduces 4 network calls to 1)
func (c *StreamClient) sendBookmarkBatchCmd(bookmark []byte) error {
	// Calculate total size: 8bytes(cmd) + 8bytes(streamType) + 4bytes(length) + bookmark_length
	totalSize := 8 + 8 + 4 + len(bookmark)

	// Create buffer for single network call
	buffer := make([]byte, totalSize)
	offset := 0

	// Write command ID (8 bytes, big endian)
	binary.BigEndian.PutUint64(buffer[offset:], uint64(CmdStartBookmarkBatch))
	offset += 8

	// Write stream type (8 bytes, big endian)
	binary.BigEndian.PutUint64(buffer[offset:], uint64(c.streamType))
	offset += 8

	// Write bookmark length (4 bytes, big endian)
	binary.BigEndian.PutUint32(buffer[offset:], uint32(len(bookmark)))
	offset += 4

	// Write bookmark data
	copy(buffer[offset:], bookmark)

	// Send all data in a single network call (4x latency reduction in high-latency environments)
	return c.writeToConn(buffer)
}

// sendHeaderCmd sends the header command to the server.
func (c *StreamClient) sendStopCmd() error {
	return c.sendCommand(CmdStop)
}

func (c *StreamClient) sendCommand(cmd Command) error {

	// Send command
	if err := c.writeToConn(uint64(cmd)); err != nil {
		return err
	}

	// Send stream type
	return c.writeToConn(uint64(c.streamType))
}
