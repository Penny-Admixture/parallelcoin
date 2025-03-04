package headerfs

import (
	"bytes"
	"fmt"
	
	"github.com/p9c/pod/pkg/chainhash"
	"github.com/p9c/pod/pkg/wire"
)

// appendRaw appends a new raw header to the end of the flat file.
func (h *headerStore) appendRaw(header []byte) (e error) {
	if _, e = h.file.Write(header); E.Chk(e) {
		return e
	}
	return nil
}

// readRaw reads a raw header from disk from a particular seek distance. The amount of bytes read past the seek distance
// is determined by the specified header type.
func (h *headerStore) readRaw(seekDist uint64) (rh []byte, e error) {
	var headerSize uint32
	// Based on the defined header type, we'll determine the number of bytes that we need to read past the sync point.
	switch h.indexType {
	case Block:
		headerSize = 80
	case RegularFilter:
		headerSize = 32
	default:
		return nil, fmt.Errorf("unknown index type: %v", h.indexType)
	}
	// TODO(roasbeef): add buffer pool
	//
	// With the number of bytes to read determined, we'll create a slice for that number of bytes, and read directly
	// from the file into the buffer.
	rawHeader := make([]byte, headerSize)
	if _, e = h.file.ReadAt(rawHeader[:], int64(seekDist)); E.Chk(e) {
		return nil, e
	}
	return rawHeader[:], nil
}

// readHeaderRange will attempt to fetch a series of headers within the target height range. This method batches a set
// of reads into a single system call thereby increasing performance when reading a set of contiguous headers.
//
// NOTE: The end height is _inclusive_ so we'll fetch all headers from the startHeight up to the end height, including
// the final header.
func (h *blockHeaderStore) readHeaderRange(
	startHeight uint32,
	endHeight uint32,
) ([]wire.BlockHeader, error) {
	// Based on the defined header type, we'll determine the number of bytes that we need to read past the sync point.
	var headerSize uint32
	switch h.indexType {
	case Block:
		headerSize = 80
	case RegularFilter:
		headerSize = 32
	default:
		return nil, fmt.Errorf("unknown index type: %v", h.indexType)
	}
	// Each header is 80 bytes, so using this information, we'll seek a distance to cover that height based on the size
	// of block headers.
	seekDistance := uint64(startHeight) * uint64(headerSize)
	// Based on the number of headers in the range, we'll allocate a single slice that's able to hold the entire range
	// of headers.
	numHeaders := endHeight - startHeight + 1
	rawHeaderBytes := make([]byte, headerSize*numHeaders)
	// Now that we have our slice allocated, we'll read out the entire range of headers with a single system call.
	_, e := h.file.ReadAt(rawHeaderBytes, int64(seekDistance))
	if e != nil {
		return nil, e
	}
	// We'll now incrementally parse out the set of individual headers from our set of serialized contiguous raw
	// headers.
	headerReader := bytes.NewReader(rawHeaderBytes)
	headers := make([]wire.BlockHeader, 0, numHeaders)
	for headerReader.Len() != 0 {
		var nextHeader wire.BlockHeader
		if e := nextHeader.Deserialize(headerReader); E.Chk(e) {
			return nil, e
		}
		headers = append(headers, nextHeader)
	}
	return headers, nil
}

// readHeader reads a full block header from the flat-file. The header read is determined by the hight value.
func (h *blockHeaderStore) readHeader(height uint32) (wire.BlockHeader, error) {
	var header wire.BlockHeader
	// Each header is 80 bytes, so using this information, we'll seek a distance to cover that height based on the size
	// of block headers.
	seekDistance := uint64(height) * 80
	// With the distance calculated, we'll raw a raw header start from that offset.
	rawHeader, e := h.readRaw(seekDistance)
	if e != nil {
		return header, e
	}
	headerReader := bytes.NewReader(rawHeader)
	// Finally, decode the raw bytes into a proper bitcoin header.
	if e := header.Deserialize(headerReader); E.Chk(e) {
		return header, e
	}
	return header, nil
}

// readHeader reads a single filter header at the specified height from the flat files on disk.
func (f *FilterHeaderStore) readHeader(height uint32) (*chainhash.Hash, error) {
	seekDistance := uint64(height) * 32
	rawHeader, e := f.readRaw(seekDistance)
	if e != nil {
		return nil, e
	}
	return chainhash.NewHash(rawHeader)
}
