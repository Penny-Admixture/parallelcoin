package wtxmgr

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/p9c/pod/pkg/amt"
	"time"
	
	"github.com/p9c/pod/pkg/chainhash"
	"github.com/p9c/pod/pkg/walletdb"
	"github.com/p9c/pod/pkg/wire"
)

// Naming
//
// The following variables are commonly used in this file and given reserved names:
//
//   ns: The namespace bucket for this package
//   b:  The primary bucket being operated on
//   k:  A single bucket key
//   v:  A single bucket value
//   c:  A bucket cursor
//   ck: The current cursor key
//   cv: The current cursor value
//
// Functions use the naming scheme `Op[Raw]Type[Field]`, which performs the operation `Op` on the type `Type`,
// optionally dealing with raw keys and values if `Raw` is used. Fetch and extract operations may only need to read some
// portion of a key or value, in which case `Field` describes the component being returned. The following operations are
// used:
//
//   key:     return a db key for some data
//   value:   return a db value for some data
//   put:     insert or replace a value into a bucket
//   fetch:   read and return a value
//   read:    read a value into an out parameter
//   exists:  return the raw (nil if not found) value for some data
//   delete:  remove a k/v pair
//   extract: perform an unchecked slice to extract a key or value
//
// Other operations which are specific to the types being operated on should be explained in a comment. Big endian is
// the preferred byte order, due to cursor scans over integer keys iterating in order.
var byteOrder = binary.BigEndian

// Database versions. Versions start at 1 and increment for each database change.
const (
	// LatestVersion is the most recent store version.
	LatestVersion = 1
)

var (
	// This package makes assumptions that the width of a chainhash.Hash is always 32 bytes. If this is ever changed
	// (unlikely for bitcoin, possible for alts), offsets have to be rewritten. Use a compile-time assertion that this
	// assumption holds true.
	_ [32]byte = chainhash.Hash{}
	// Bucket names
	bucketBlocks         = []byte("b")
	bucketTxRecords      = []byte("t")
	bucketCredits        = []byte("c")
	bucketUnspent        = []byte("u")
	bucketDebits         = []byte("d")
	bucketUnmined        = []byte("m")
	bucketUnminedCredits = []byte("mc")
	bucketUnminedInputs  = []byte("mi")
	// Root (namespace) bucket keys
	rootCreateDate   = []byte("date")
	rootVersion      = []byte("vers")
	rootMinedBalance = []byte("bal")
)

// The root bucket's mined balance k/v pair records the total balance for all unspent credits from mined transactions.
// This includes immature outputs, and outputs spent by mempool transactions, which must be considered when returning
// the actual balance for a given number of block confirmations. The value is the amount serialized as a uint64.
func fetchMinedBalance(ns walletdb.ReadBucket) (amt.Amount, error) {
	v := ns.Get(rootMinedBalance)
	if len(v) != 8 {
		str := fmt.Sprintf(
			"balance: short read (expected 8 bytes, "+
				"read %v)", len(v),
		)
		return 0, storeError(ErrData, str, nil)
	}
	return amt.Amount(byteOrder.Uint64(v)), nil
}
func putMinedBalance(ns walletdb.ReadWriteBucket, amt amt.Amount) (e error) {
	v := make([]byte, 8)
	byteOrder.PutUint64(v, uint64(amt))
	e = ns.Put(rootMinedBalance, v)
	if e != nil {
		str := "failed to put balance"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// Several data structures are given canonical serialization formats as either keys or values. These common formats
// allow keys and values to be reused across different buckets.
//
// The canonical outpoint serialization format is:
//
//   [0:32]  Trasaction hash (32 bytes)
//   [32:36] Output index (4 bytes)
//
// The canonical transaction hash serialization is simply the hash.
func canonicalOutPoint(txHash *chainhash.Hash, index uint32) []byte {
	k := make([]byte, 36)
	copy(k, txHash[:])
	byteOrder.PutUint32(k[32:36], index)
	return k
}
func readCanonicalOutPoint(k []byte, op *wire.OutPoint) (e error) {
	if len(k) < 36 {
		str := "short canonical outpoint"
		return storeError(ErrData, str, nil)
	}
	copy(op.Hash[:], k)
	op.Index = byteOrder.Uint32(k[32:36])
	return nil
}

// Details regarding blocks are saved as k/v pairs in the blocks bucket. blockRecords are keyed by their height. The
// value is serialized as such:
//
//   [0:32]  Hash (32 bytes)
//   [32:40] Unix time (8 bytes)
//   [40:44] Number of transaction hashes (4 bytes)
//   [44:]   For each transaction hash:
//             Hash (32 bytes)
func keyBlockRecord(height int32) []byte {
	k := make([]byte, 4)
	byteOrder.PutUint32(k, uint32(height))
	return k
}
func valueBlockRecord(block *BlockMeta, txHash *chainhash.Hash) []byte {
	v := make([]byte, 76)
	copy(v, block.Hash[:])
	byteOrder.PutUint64(v[32:40], uint64(block.Time.Unix()))
	byteOrder.PutUint32(v[40:44], 1)
	copy(v[44:76], txHash[:])
	return v
}

// appendRawBlockRecord returns a new block record value with a transaction hash appended to the end and an incremented
// number of transactions.
func appendRawBlockRecord(v []byte, txHash *chainhash.Hash) ([]byte, error) {
	if len(v) < 44 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketBlocks, 44, len(v),
		)
		return nil, storeError(ErrData, str, nil)
	}
	newv := append(v[:len(v):len(v)], txHash[:]...)
	n := byteOrder.Uint32(newv[40:44])
	byteOrder.PutUint32(newv[40:44], n+1)
	return newv, nil
}
func putRawBlockRecord(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketBlocks).Put(k, v)
	if e != nil {
		str := "failed to store block"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}
func putBlockRecord(ns walletdb.ReadWriteBucket, block *BlockMeta, txHash *chainhash.Hash) (e error) {
	k := keyBlockRecord(block.Height)
	v := valueBlockRecord(block, txHash)
	return putRawBlockRecord(ns, k, v)
}
func fetchBlockTime(ns walletdb.ReadBucket, height int32) (time.Time, error) {
	k := keyBlockRecord(height)
	v := ns.NestedReadBucket(bucketBlocks).Get(k)
	if len(v) < 44 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketBlocks, 44, len(v),
		)
		return time.Time{}, storeError(ErrData, str, nil)
	}
	return time.Unix(int64(byteOrder.Uint64(v[32:40])), 0), nil
}
func existsBlockRecord(ns walletdb.ReadBucket, height int32) (k, v []byte) {
	k = keyBlockRecord(height)
	v = ns.NestedReadBucket(bucketBlocks).Get(k)
	return
}
func readRawBlockRecord(k, v []byte, block *blockRecord) (e error) {
	if len(k) < 4 {
		str := fmt.Sprintf(
			"%s: short key (expected %d bytes, read %d)",
			bucketBlocks, 4, len(k),
		)
		return storeError(ErrData, str, nil)
	}
	if len(v) < 44 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketBlocks, 44, len(v),
		)
		return storeError(ErrData, str, nil)
	}
	numTransactions := int(byteOrder.Uint32(v[40:44]))
	expectedLen := 44 + chainhash.HashSize*numTransactions
	if len(v) < expectedLen {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketBlocks, expectedLen, len(v),
		)
		return storeError(ErrData, str, nil)
	}
	block.Height = int32(byteOrder.Uint32(k))
	copy(block.Hash[:], v)
	block.Time = time.Unix(int64(byteOrder.Uint64(v[32:40])), 0)
	block.transactions = make([]chainhash.Hash, numTransactions)
	off := 44
	for i := range block.transactions {
		copy(block.transactions[i][:], v[off:])
		off += chainhash.HashSize
	}
	return nil
}

type blockIterator struct {
	c    walletdb.ReadWriteCursor
	seek []byte
	ck   []byte
	cv   []byte
	elem blockRecord
	err  error
}

// func makeBlockIterator(ns walletdb.ReadWriteBucket, height int32) blockIterator {
// 	seek := make([]byte, 4)
// 	byteOrder.PutUint32(seek, uint32(height))
// 	c := ns.NestedReadWriteBucket(bucketBlocks).ReadWriteCursor()
// 	return blockIterator{c: c, seek: seek}
// }
func makeReadBlockIterator(ns walletdb.ReadBucket, height int32) blockIterator {
	seek := make([]byte, 4)
	byteOrder.PutUint32(seek, uint32(height))
	c := ns.NestedReadBucket(bucketBlocks).ReadCursor()
	return blockIterator{c: readCursor{c}, seek: seek}
}

// Works just like makeBlockIterator but will initially position the cursor at the last k/v pair. Use this with
// blockIterator.prev.
func makeReverseBlockIterator(ns walletdb.ReadWriteBucket) blockIterator {
	seek := make([]byte, 4)
	byteOrder.PutUint32(seek, ^uint32(0))
	c := ns.NestedReadWriteBucket(bucketBlocks).ReadWriteCursor()
	return blockIterator{c: c, seek: seek}
}
func makeReadReverseBlockIterator(ns walletdb.ReadBucket) blockIterator {
	seek := make([]byte, 4)
	byteOrder.PutUint32(seek, ^uint32(0))
	c := ns.NestedReadBucket(bucketBlocks).ReadCursor()
	return blockIterator{c: readCursor{c}, seek: seek}
}
func (it *blockIterator) next() bool {
	if it.c == nil {
		return false
	}
	if it.ck == nil {
		it.ck, it.cv = it.c.Seek(it.seek)
	} else {
		it.ck, it.cv = it.c.Next()
	}
	if it.ck == nil {
		it.c = nil
		return false
	}
	e := readRawBlockRecord(it.ck, it.cv, &it.elem)
	if e != nil {
		it.c = nil
		it.err = e
		return false
	}
	return true
}
func (it *blockIterator) prev() bool {
	if it.c == nil {
		return false
	}
	if it.ck == nil {
		it.ck, it.cv = it.c.Seek(it.seek)
		// Seek positions the cursor at the next k/v pair if one with this prefix was not found. If this happened (the
		// prefixes won't match in this case) move the cursor backward.
		//
		// This technically does not correct for multiple keys with matching prefixes by moving the cursor to the last
		// matching key, but this doesn't need to be considered when dealing with block records since the key (and seek
		// prefix) is just the block height.
		if !bytes.HasPrefix(it.ck, it.seek) {
			it.ck, it.cv = it.c.Prev()
		}
	} else {
		it.ck, it.cv = it.c.Prev()
	}
	if it.ck == nil {
		it.c = nil
		return false
	}
	e := readRawBlockRecord(it.ck, it.cv, &it.elem)
	if e != nil {
		it.c = nil
		it.err = e
		return false
	}
	return true
}

// unavailable until https://github.com/boltdb/bolt/issues/620 is fixed.
// func (it *blockIterator) delete() (e error) {
// 	e := it.c.Delete()
// 	if e != nil  {
// DB// 		str := "failed to delete block record"
// 		storeError(ErrDatabase, str, err)
// 	}
// 	return nil
// }
func (it *blockIterator) reposition(height int32) {
	it.c.Seek(keyBlockRecord(height))
}
func deleteBlockRecord(ns walletdb.ReadWriteBucket, height int32) (e error) {
	k := keyBlockRecord(height)
	return ns.NestedReadWriteBucket(bucketBlocks).Delete(k)
}

// Transaction records are keyed as such:
//
//   [0:32]  Transaction hash (32 bytes)
//   [32:36] Block height (4 bytes)
//   [36:68] Block hash (32 bytes)
//
// The leading transaction hash allows to prefix filter for all records with a matching hash. The block height and hash
// records a particular incidence of the transaction in the blockchain.
//
// The record value is serialized as such:
//
//   [0:8]   Received time (8 bytes)
//   [8:]    Serialized transaction (varies)
func keyTxRecord(txHash *chainhash.Hash, block *Block) []byte {
	k := make([]byte, 68)
	copy(k, txHash[:])
	byteOrder.PutUint32(k[32:36], uint32(block.Height))
	copy(k[36:68], block.Hash[:])
	return k
}
func valueTxRecord(rec *TxRecord) ([]byte, error) {
	var v []byte
	if rec.SerializedTx == nil {
		txSize := rec.MsgTx.SerializeSize()
		v = make([]byte, 8, 8+txSize)
		e := rec.MsgTx.Serialize(bytes.NewBuffer(v[8:]))
		if e != nil {
			str := fmt.Sprintf("unable to serialize transaction %v", rec.Hash)
			return nil, storeError(ErrInput, str, e)
		}
		v = v[:cap(v)]
	} else {
		v = make([]byte, 8+len(rec.SerializedTx))
		copy(v[8:], rec.SerializedTx)
	}
	byteOrder.PutUint64(v, uint64(rec.Received.Unix()))
	return v, nil
}
func putTxRecord(ns walletdb.ReadWriteBucket, rec *TxRecord, block *Block) (e error) {
	k := keyTxRecord(&rec.Hash, block)
	v, e := valueTxRecord(rec)
	if e != nil {
		return e
	}
	e = ns.NestedReadWriteBucket(bucketTxRecords).Put(k, v)
	if e != nil {
		str := fmt.Sprintf("%s: put failed for %v", bucketTxRecords, rec.Hash)
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// func putRawTxRecord(// 	ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
// 	e := ns.NestedReadWriteBucket(bucketTxRecords).Put(k, v)
// 	if e != nil  {
// DB// 		str := fmt.Sprintf("%s: put failed", bucketTxRecords)
// 		return storeError(ErrDatabase, str, err)
// 	}
// 	return nil
// }
func readRawTxRecord(txHash *chainhash.Hash, v []byte, rec *TxRecord) (e error) {
	if len(v) < 8 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketTxRecords, 8, len(v),
		)
		return storeError(ErrData, str, nil)
	}
	rec.Hash = *txHash
	rec.Received = time.Unix(int64(byteOrder.Uint64(v)), 0)
	e = rec.MsgTx.Deserialize(bytes.NewReader(v[8:]))
	if e != nil {
		str := fmt.Sprintf(
			"%s: failed to deserialize transaction %v",
			bucketTxRecords, txHash,
		)
		return storeError(ErrData, str, e)
	}
	return nil
}
func readRawTxRecordBlock(k []byte, block *Block) (e error) {
	if len(k) < 68 {
		str := fmt.Sprintf(
			"%s: short key (expected %d bytes, read %d)",
			bucketTxRecords, 68, len(k),
		)
		return storeError(ErrData, str, nil)
	}
	block.Height = int32(byteOrder.Uint32(k[32:36]))
	copy(block.Hash[:], k[36:68])
	return nil
}

func fetchTxRecord(ns walletdb.ReadBucket, txHash *chainhash.Hash, block *Block) (rec *TxRecord,e error) {
	k := keyTxRecord(txHash, block)
	v := ns.NestedReadBucket(bucketTxRecords).Get(k)
	rec = new(TxRecord)
	e = readRawTxRecord(txHash, v, rec)
	return rec, e
}

// TODO: This reads more than necessary. Pass the pkscript location instead to
//  avoid the wire.MsgTx deserialization.
func fetchRawTxRecordPkScript(k, v []byte, index uint32) ([]byte, error) {
	var rec TxRecord
	copy(rec.Hash[:], k) // Silly but need an array
	e := readRawTxRecord(&rec.Hash, v, &rec)
	if e != nil {
		return nil, e
	}
	if int(index) >= len(rec.MsgTx.TxOut) {
		str := "missing transaction output for credit index"
		return nil, storeError(ErrData, str, nil)
	}
	return rec.MsgTx.TxOut[index].PkScript, nil
}
func existsTxRecord(ns walletdb.ReadBucket, txHash *chainhash.Hash, block *Block) (k, v []byte) {
	k = keyTxRecord(txHash, block)
	v = ns.NestedReadBucket(bucketTxRecords).Get(k)
	return
}

func existsRawTxRecord(ns walletdb.ReadBucket, k []byte) (v []byte) {
	return ns.NestedReadBucket(bucketTxRecords).Get(k)
}

func deleteTxRecord(ns walletdb.ReadWriteBucket, txHash *chainhash.Hash, block *Block) (e error) {
	k := keyTxRecord(txHash, block)
	return ns.NestedReadWriteBucket(bucketTxRecords).Delete(k)
}

// latestTxRecord searches for the newest recorded mined transaction record with a matching hash. In case of a hash
// collision, the record from the newest block is returned. Returns (nil, nil) if no matching transactions are found.
func latestTxRecord(ns walletdb.ReadBucket, txHash *chainhash.Hash) (k, v []byte) {
	prefix := txHash[:]
	c := ns.NestedReadBucket(bucketTxRecords).ReadCursor()
	ck, cv := c.Seek(prefix)
	var lastKey, lastVal []byte
	for bytes.HasPrefix(ck, prefix) {
		lastKey, lastVal = ck, cv
		ck, cv = c.Next()
	}
	return lastKey, lastVal
}

// All transaction credits (outputs) are keyed as such:
//
//   [0:32]  Transaction hash (32 bytes)
//   [32:36] Block height (4 bytes)
//   [36:68] Block hash (32 bytes)
//   [68:72] Output index (4 bytes)
//
// The first 68 bytes match the key for the transaction record and may be used as a prefix filter to iterate through all
// credits in order.
//
// The credit value is serialized as such:
//
//   [0:8]   Amount (8 bytes)
//   [8]     Flags (1 byte)
//             0x01: Spent
//             0x02: Change
//   [9:81]  OPTIONAL Debit bucket key (72 bytes)
//             [9:41]  Spender transaction hash (32 bytes)
//             [41:45] Spender block height (4 bytes)
//             [45:77] Spender block hash (32 bytes)
//             [77:81] Spender transaction input index (4 bytes)
//
// The optional debits key is only included if the credit is spent by another
// mined debit.
func keyCredit(txHash *chainhash.Hash, index uint32, block *Block) []byte {
	k := make([]byte, 72)
	copy(k, txHash[:])
	byteOrder.PutUint32(k[32:36], uint32(block.Height))
	copy(k[36:68], block.Hash[:])
	byteOrder.PutUint32(k[68:72], index)
	return k
}

// valueUnspentCredit creates a new credit value for an unspent credit. All credits are created unspent, and are only
// marked spent later, so there is no value function to create either spent or unspent credits.
func valueUnspentCredit(cred *credit) []byte {
	v := make([]byte, 9)
	byteOrder.PutUint64(v, uint64(cred.amount))
	if cred.change {
		v[8] |= 1 << 1
	}
	return v
}
func putRawCredit(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketCredits).Put(k, v)
	if e != nil {
		str := "failed to put credit"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// putUnspentCredit puts a credit record for an unspent credit. It may only be used when the credit is already know to
// be unspent, or spent by an unconfirmed transaction.
func putUnspentCredit(ns walletdb.ReadWriteBucket, cred *credit) (e error) {
	k := keyCredit(&cred.outPoint.Hash, cred.outPoint.Index, &cred.block)
	v := valueUnspentCredit(cred)
	return putRawCredit(ns, k, v)
}
func extractRawCreditTxRecordKey(k []byte) []byte {
	return k[0:68]
}
func extractRawCreditIndex(k []byte) uint32 {
	return byteOrder.Uint32(k[68:72])
}

// fetchRawCreditAmount returns the amount of the credit.
func fetchRawCreditAmount(v []byte) (amt.Amount, error) {
	if len(v) < 9 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketCredits, 9, len(v),
		)
		return 0, storeError(ErrData, str, nil)
	}
	return amt.Amount(byteOrder.Uint64(v)), nil
}

// fetchRawCreditAmountSpent returns the amount of the credit and whether the credit is spent.
func fetchRawCreditAmountSpent(v []byte) (amt.Amount, bool, error) {
	if len(v) < 9 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketCredits, 9, len(v),
		)
		return 0, false, storeError(ErrData, str, nil)
	}
	return amt.Amount(byteOrder.Uint64(v)), v[8]&(1<<0) != 0, nil
}

// fetchRawCreditAmountChange returns the amount of the credit and whether the credit is marked as change.
func fetchRawCreditAmountChange(v []byte) (amt.Amount, bool, error) {
	if len(v) < 9 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketCredits, 9, len(v),
		)
		return 0, false, storeError(ErrData, str, nil)
	}
	return amt.Amount(byteOrder.Uint64(v)), v[8]&(1<<1) != 0, nil
}

// fetchRawCreditUnspentValue returns the unspent value for a raw credit key. This may be used to mark a credit as
// unspent.
func fetchRawCreditUnspentValue(k []byte) ([]byte, error) {
	if len(k) < 72 {
		str := fmt.Sprintf(
			"%s: short key (expected %d bytes, read %d)",
			bucketCredits, 72, len(k),
		)
		return nil, storeError(ErrData, str, nil)
	}
	return k[32:68], nil
}

// spendRawCredit marks the credit with a given key as mined at some particular block as spent by the input at some
// transaction incidence. The debited amount is returned.
func spendCredit(ns walletdb.ReadWriteBucket, k []byte, spender *indexedIncidence) (amt.Amount, error) {
	v := ns.NestedReadBucket(bucketCredits).Get(k)
	newv := make([]byte, 81)
	copy(newv, v)
	v = newv
	v[8] |= 1 << 0
	copy(v[9:41], spender.txHash[:])
	byteOrder.PutUint32(v[41:45], uint32(spender.block.Height))
	copy(v[45:77], spender.block.Hash[:])
	byteOrder.PutUint32(v[77:81], spender.index)
	return amt.Amount(byteOrder.Uint64(v[0:8])), putRawCredit(ns, k, v)
}

// unspendRawCredit rewrites the credit for the given key as unspent. The output amount of the credit is returned. It
// returns without error if no credit exists for the key.
func unspendRawCredit(ns walletdb.ReadWriteBucket, k []byte) (amt.Amount, error) {
	b := ns.NestedReadWriteBucket(bucketCredits)
	v := b.Get(k)
	if v == nil {
		return 0, nil
	}
	newv := make([]byte, 9)
	copy(newv, v)
	newv[8] &^= 1 << 0
	e := b.Put(k, newv)
	if e != nil {
		str := "failed to put credit"
		return 0, storeError(ErrDatabase, str, e)
	}
	return amt.Amount(byteOrder.Uint64(v[0:8])), nil
}

func existsCredit(ns walletdb.ReadBucket, txHash *chainhash.Hash, index uint32, block *Block) (k, v []byte) {
	k = keyCredit(txHash, index, block)
	v = ns.NestedReadBucket(bucketCredits).Get(k)
	return
}

func existsRawCredit(ns walletdb.ReadBucket, k []byte) []byte {
	return ns.NestedReadBucket(bucketCredits).Get(k)
}

func deleteRawCredit(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketCredits).Delete(k)
	if e != nil {
		str := "failed to delete credit"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// creditIterator allows for in-order iteration of all credit records for a mined transaction.
//
// Example usage:
//
//   prefix := keyTxRecord(txHash, block)
//   it := makeCreditIterator(ns, prefix)
//   for it.next() {
//           // Use it.elem
//           // If necessary, read additional details from it.ck, it.cv
//   }
//   if it.err != nil {
//           // Handle error
//   }
//
// The elem's Spent field is not set to true if the credit is spent by an unmined transaction. To check for this case:
//
//   k := canonicalOutPoint(&txHash, it.elem.Index)
//   it.elem.Spent = existsRawUnminedInput(ns, k) != nil
type creditIterator struct {
	c      walletdb.ReadWriteCursor // Set to nil after final iteration
	prefix []byte
	ck     []byte
	cv     []byte
	elem   CreditRecord
	err    error
}

// func makeCreditIterator(// 	ns walletdb.ReadWriteBucket, prefix []byte) creditIterator {
// 	c := ns.NestedReadWriteBucket(bucketCredits).ReadWriteCursor()
// 	return creditIterator{c: c, prefix: prefix}
// }
func makeReadCreditIterator(ns walletdb.ReadBucket, prefix []byte) creditIterator {
	c := ns.NestedReadBucket(bucketCredits).ReadCursor()
	return creditIterator{c: readCursor{c}, prefix: prefix}
}
func (it *creditIterator) readElem() (e error) {
	if len(it.ck) < 72 {
		str := fmt.Sprintf(
			"%s: short key (expected %d bytes, read %d)",
			bucketCredits, 72, len(it.ck),
		)
		return storeError(ErrData, str, nil)
	}
	if len(it.cv) < 9 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketCredits, 9, len(it.cv),
		)
		return storeError(ErrData, str, nil)
	}
	it.elem.Index = byteOrder.Uint32(it.ck[68:72])
	it.elem.Amount = amt.Amount(byteOrder.Uint64(it.cv))
	it.elem.Spent = it.cv[8]&(1<<0) != 0
	it.elem.Change = it.cv[8]&(1<<1) != 0
	return nil
}
func (it *creditIterator) next() bool {
	if it.c == nil {
		return false
	}
	if it.ck == nil {
		it.ck, it.cv = it.c.Seek(it.prefix)
	} else {
		it.ck, it.cv = it.c.Next()
	}
	if !bytes.HasPrefix(it.ck, it.prefix) {
		it.c = nil
		return false
	}
	e := it.readElem()
	if e != nil {
		it.err = e
		return false
	}
	return true
}

// The unspent index records all outpoints for mined credits which are not spent by any other mined transaction records
// (but may be spent by a mempool transaction).
//
// Keys are use the canonical outpoint serialization:
//
//   [0:32]  Transaction hash (32 bytes)
//   [32:36] Output index (4 bytes)
//
// Values are serialized as such:
//
//   [0:4]   Block height (4 bytes)
//   [4:36]  Block hash (32 bytes)
func valueUnspent(block *Block) []byte {
	v := make([]byte, 36)
	byteOrder.PutUint32(v, uint32(block.Height))
	copy(v[4:36], block.Hash[:])
	return v
}
func putUnspent(ns walletdb.ReadWriteBucket, outPoint *wire.OutPoint, block *Block) (e error) {
	k := canonicalOutPoint(&outPoint.Hash, outPoint.Index)
	v := valueUnspent(block)
	e = ns.NestedReadWriteBucket(bucketUnspent).Put(k, v)
	if e != nil {
		str := "cannot put unspent"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}
func putRawUnspent(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnspent).Put(k, v)
	if e != nil {
		str := "cannot put unspent"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}
func readUnspentBlock(v []byte, block *Block) (e error) {
	if len(v) < 36 {
		str := "short unspent value"
		return storeError(ErrData, str, nil)
	}
	block.Height = int32(byteOrder.Uint32(v))
	copy(block.Hash[:], v[4:36])
	return nil
}

// existsUnspent returns the key for the unspent output and the corresponding key for the credits bucket. If there is no
// unspent output recorded, the credit key is nil.
func existsUnspent(ns walletdb.ReadBucket, outPoint *wire.OutPoint) (k, credKey []byte) {
	k = canonicalOutPoint(&outPoint.Hash, outPoint.Index)
	credKey = existsRawUnspent(ns, k)
	return k, credKey
}

// existsRawUnspent returns the credit key if there exists an output recorded for the raw unspent key. It returns nil if
// the k/v pair does not exist.
func existsRawUnspent(ns walletdb.ReadBucket, k []byte) (credKey []byte) {
	if len(k) < 36 {
		return nil
	}
	v := ns.NestedReadBucket(bucketUnspent).Get(k)
	if len(v) < 36 {
		return nil
	}
	credKey = make([]byte, 72)
	copy(credKey, k[:32])
	copy(credKey[32:68], v)
	copy(credKey[68:72], k[32:36])
	return credKey
}
func deleteRawUnspent(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnspent).Delete(k)
	if e != nil {
		str := "failed to delete unspent"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// All transaction debits (inputs which spend credits) are keyed as such:
//
//   [0:32]  Transaction hash (32 bytes)
//   [32:36] Block height (4 bytes)
//   [36:68] Block hash (32 bytes)
//   [68:72] Input index (4 bytes)
//
// The first 68 bytes match the key for the transaction record and may be used as a prefix filter to iterate through all
// debits in order.
//
// The debit value is serialized as such:
//
//   [0:8]   Amount (8 bytes)
//   [8:80]  Credits bucket key (72 bytes)
//             [8:40]  Transaction hash (32 bytes)
//             [40:44] Block height (4 bytes)
//             [44:76] Block hash (32 bytes)
//             [76:80] Output index (4 bytes)
func keyDebit(txHash *chainhash.Hash, index uint32, block *Block) []byte {
	k := make([]byte, 72)
	copy(k, txHash[:])
	byteOrder.PutUint32(k[32:36], uint32(block.Height))
	copy(k[36:68], block.Hash[:])
	byteOrder.PutUint32(k[68:72], index)
	return k
}

func putDebit(
	ns walletdb.ReadWriteBucket,
	txHash *chainhash.Hash,
	index uint32,
	amount amt.Amount,
	block *Block,
	credKey []byte,
) (e error) {
	k := keyDebit(txHash, index, block)
	v := make([]byte, 80)
	byteOrder.PutUint64(v, uint64(amount))
	copy(v[8:80], credKey)
	e = ns.NestedReadWriteBucket(bucketDebits).Put(k, v)
	if e != nil {
		str := fmt.Sprintf(
			"failed to update debit %s input %d",
			txHash, index,
		)
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

func extractRawDebitCreditKey(v []byte) []byte {
	return v[8:80]
}

// existsDebit checks for the existance of a debit. If found, the debit and previous credit keys are returned. If the
// debit does not exist, both keys are nil.
func existsDebit(ns walletdb.ReadBucket, txHash *chainhash.Hash, index uint32, block *Block) (
	k, credKey []byte,
	e error,
) {
	k = keyDebit(txHash, index, block)
	v := ns.NestedReadBucket(bucketDebits).Get(k)
	if v == nil {
		return nil, nil, nil
	}
	if len(v) < 80 {
		str := fmt.Sprintf(
			"%s: short read (expected 80 bytes, read %v)",
			bucketDebits, len(v),
		)
		return nil, nil, storeError(ErrData, str, nil)
	}
	return k, v[8:80], nil
}

func deleteRawDebit(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketDebits).Delete(k)
	if e != nil {
		str := "failed to delete debit"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// debitIterator allows for in-order iteration of all debit records for a mined transaction.
//
// Example usage:
//
//   prefix := keyTxRecord(txHash, block)
//   it := makeDebitIterator(ns, prefix)
//   for it.next() {
//           // Use it.elem
//           // If necessary, read additional details from it.ck, it.cv
//   }
//   if it.err != nil {
//           // Handle error
//   }
type debitIterator struct {
	c      walletdb.ReadWriteCursor // Set to nil after final iteration
	prefix []byte
	ck     []byte
	cv     []byte
	elem   DebitRecord
	err    error
}

// func makeDebitIterator(// 	ns walletdb.ReadWriteBucket, prefix []byte) debitIterator {
// 	c := ns.NestedReadWriteBucket(bucketDebits).ReadWriteCursor()
// 	return debitIterator{c: c, prefix: prefix}
// }
func makeReadDebitIterator(ns walletdb.ReadBucket, prefix []byte) debitIterator {
	c := ns.NestedReadBucket(bucketDebits).ReadCursor()
	return debitIterator{c: readCursor{c}, prefix: prefix}
}

func (it *debitIterator) readElem() (e error) {
	if len(it.ck) < 72 {
		str := fmt.Sprintf(
			"%s: short key (expected %d bytes, read %d)",
			bucketDebits, 72, len(it.ck),
		)
		return storeError(ErrData, str, nil)
	}
	if len(it.cv) < 80 {
		str := fmt.Sprintf(
			"%s: short read (expected %d bytes, read %d)",
			bucketDebits, 80, len(it.cv),
		)
		return storeError(ErrData, str, nil)
	}
	it.elem.Index = byteOrder.Uint32(it.ck[68:72])
	it.elem.Amount = amt.Amount(byteOrder.Uint64(it.cv))
	return nil
}

func (it *debitIterator) next() bool {
	if it.c == nil {
		return false
	}
	if it.ck == nil {
		it.ck, it.cv = it.c.Seek(it.prefix)
	} else {
		it.ck, it.cv = it.c.Next()
	}
	if !bytes.HasPrefix(it.ck, it.prefix) {
		it.c = nil
		return false
	}
	e := it.readElem()
	if e != nil {
		it.err = e
		return false
	}
	return true
}

// All unmined transactions are saved in the unmined bucket keyed by the transaction hash. The value matches that of
// mined transaction records:
//
//   [0:8]   Received time (8 bytes)
//   [8:]    Serialized transaction (varies)
func putRawUnmined(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnmined).Put(k, v)
	if e != nil {
		str := "failed to put unmined record"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

func readRawUnminedHash(k []byte, txHash *chainhash.Hash) (e error) {
	if len(k) < 32 {
		str := "short unmined key"
		return storeError(ErrData, str, nil)
	}
	copy(txHash[:], k)
	return nil
}

func existsRawUnmined(ns walletdb.ReadBucket, k []byte) (v []byte) {
	return ns.NestedReadBucket(bucketUnmined).Get(k)
}

func deleteRawUnmined(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnmined).Delete(k)
	if e != nil {
		str := "failed to delete unmined record"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// Unmined transaction credits use the canonical serialization format:
//
//  [0:32]   Transaction hash (32 bytes)
//  [32:36]  Output index (4 bytes)
//
// The value matches the format used by mined credits, but the spent flag is never set and the optional debit record is
// never included. The simplified format is thus:
//
//   [0:8]   Amount (8 bytes)
//   [8]     Flags (1 byte)
//             0x02: Change
func valueUnminedCredit(amount amt.Amount, change bool) []byte {
	v := make([]byte, 9)
	byteOrder.PutUint64(v, uint64(amount))
	if change {
		v[8] = 1 << 1
	}
	return v
}
func putRawUnminedCredit(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnminedCredits).Put(k, v)
	if e != nil {
		str := "cannot put unmined credit"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}
func fetchRawUnminedCreditIndex(k []byte) (uint32, error) {
	if len(k) < 36 {
		str := "short unmined credit key"
		return 0, storeError(ErrData, str, nil)
	}
	return byteOrder.Uint32(k[32:36]), nil
}
func fetchRawUnminedCreditAmount(v []byte) (amt.Amount, error) {
	if len(v) < 9 {
		str := "short unmined credit value"
		return 0, storeError(ErrData, str, nil)
	}
	return amt.Amount(byteOrder.Uint64(v)), nil
}
func fetchRawUnminedCreditAmountChange(v []byte) (amt.Amount, bool, error) {
	if len(v) < 9 {
		str := "short unmined credit value"
		return 0, false, storeError(ErrData, str, nil)
	}
	amt := amt.Amount(byteOrder.Uint64(v))
	change := v[8]&(1<<1) != 0
	return amt, change, nil
}
func existsRawUnminedCredit(ns walletdb.ReadBucket, k []byte) []byte {
	return ns.NestedReadBucket(bucketUnminedCredits).Get(k)
}
func deleteRawUnminedCredit(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnminedCredits).Delete(k)
	if e != nil {
		str := "failed to delete unmined credit"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// unminedCreditIterator allows for cursor iteration over all credits, in order, from a single unmined transaction.
//
//  Example usage:
//
//   it := makeUnminedCreditIterator(ns, txHash)
//   for it.next() {
//           // Use it.elem, it.ck and it.cv
//           // Optionally, use it.delete() to remove this k/v pair
//   }
//   if it.err != nil {
//           // Handle error
//   }
//
// The spentness of the credit is not looked up for performance reasons (because for unspent credits, it requires
// another lookup in another bucket). If this is needed, it may be checked like this:
//
//   spent := existsRawUnminedInput(ns, it.ck) != nil
type unminedCreditIterator struct {
	c      walletdb.ReadWriteCursor
	prefix []byte
	ck     []byte
	cv     []byte
	elem   CreditRecord
	err    error
}

type readCursor struct {
	walletdb.ReadCursor
}

// Delete ...
func (r readCursor) Delete() (e error) {
	str := "failed to delete current cursor item from read-only cursor"
	return storeError(ErrDatabase, str, walletdb.ErrTxNotWritable)
}

func makeUnminedCreditIterator(ns walletdb.ReadWriteBucket, txHash *chainhash.Hash) unminedCreditIterator {
	c := ns.NestedReadWriteBucket(bucketUnminedCredits).ReadWriteCursor()
	return unminedCreditIterator{c: c, prefix: txHash[:]}
}

func makeReadUnminedCreditIterator(ns walletdb.ReadBucket, txHash *chainhash.Hash) unminedCreditIterator {
	c := ns.NestedReadBucket(bucketUnminedCredits).ReadCursor()
	return unminedCreditIterator{c: readCursor{c}, prefix: txHash[:]}
}

func (it *unminedCreditIterator) readElem() (e error) {
	index, e := fetchRawUnminedCreditIndex(it.ck)
	if e != nil {
		return e
	}
	amount, change, e := fetchRawUnminedCreditAmountChange(it.cv)
	if e != nil {
		return e
	}
	it.elem.Index = index
	it.elem.Amount = amount
	it.elem.Change = change
	// Spent intentionally not set
	return nil
}

func (it *unminedCreditIterator) next() bool {
	if it.c == nil {
		return false
	}
	if it.ck == nil {
		it.ck, it.cv = it.c.Seek(it.prefix)
	} else {
		it.ck, it.cv = it.c.Next()
	}
	if !bytes.HasPrefix(it.ck, it.prefix) {
		it.c = nil
		return false
	}
	e := it.readElem()
	if e != nil {
		it.err = e
		return false
	}
	return true
}

// // unavailable until https://github.com/boltdb/bolt/issues/620 is fixed.
// // func (it *unminedCreditIterator) delete() (e error) {
// // 	e := it.c.Delete()
// // 	if e != nil  {
// DB// // 		str := "failed to delete unmined credit"
// // 		return storeError(ErrDatabase, str, err)
// // 	}
// // 	return nil
// // }
// func (it *unminedCreditIterator) reposition(txHash *chainhash.Hash, index uint32) {
// 	it.c.Seek(canonicalOutPoint(txHash, index))
// }

// Outpoints spent by unmined transactions are saved in the unmined inputs bucket. This bucket maps between each
// previous output spent, for both mined and unmined transactions, to the hash of the unmined transaction.
//
// The key is serialized as such:
//
//   [0:32]   Transaction hash (32 bytes)
//   [32:36]  Output index (4 bytes)
//
// The value is serialized as such:
//
//   [0:32]   Transaction hash (32 bytes)

// putRawUnminedInput maintains a list of unmined transaction hashes that have spent an outpoint. Each entry in the
// bucket is keyed by the outpoint being spent.
func putRawUnminedInput(ns walletdb.ReadWriteBucket, k, v []byte) (e error) {
	spendTxHashes := ns.NestedReadBucket(bucketUnminedInputs).Get(k)
	spendTxHashes = append(spendTxHashes, v...)
	e = ns.NestedReadWriteBucket(bucketUnminedInputs).Put(k, spendTxHashes)
	if e != nil {
		str := "failed to put unmined input"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

func existsRawUnminedInput(ns walletdb.ReadBucket, k []byte) (v []byte) {
	return ns.NestedReadBucket(bucketUnminedInputs).Get(k)
}

// fetchUnminedInputSpendTxHashes fetches the list of unmined transactions that spend the serialized outpoint.
func fetchUnminedInputSpendTxHashes(ns walletdb.ReadBucket, k []byte) []chainhash.Hash {
	rawSpendTxHashes := ns.NestedReadBucket(bucketUnminedInputs).Get(k)
	if rawSpendTxHashes == nil {
		return nil
	}
	// Each transaction hash is 32 bytes.
	spendTxHashes := make([]chainhash.Hash, 0, len(rawSpendTxHashes)/32)
	for len(rawSpendTxHashes) > 0 {
		var spendTxHash chainhash.Hash
		copy(spendTxHash[:], rawSpendTxHashes[:32])
		spendTxHashes = append(spendTxHashes, spendTxHash)
		rawSpendTxHashes = rawSpendTxHashes[32:]
	}
	return spendTxHashes
}

func deleteRawUnminedInput(ns walletdb.ReadWriteBucket, k []byte) (e error) {
	e = ns.NestedReadWriteBucket(bucketUnminedInputs).Delete(k)
	if e != nil {
		str := "failed to delete unmined input"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// openStore opens an existing transaction store from the passed namespace.
func openStore(ns walletdb.ReadBucket) (e error) {
	v := ns.Get(rootVersion)
	if len(v) != 4 {
		str := "no transaction store exists in namespace"
		return storeError(ErrNoExists, str, nil)
	}
	version := byteOrder.Uint32(v)
	if version < LatestVersion {
		str := fmt.Sprintf(
			"a database upgrade is required to upgrade "+
				"wtxmgr from recorded version %d to the latest version %d",
			version, LatestVersion,
		)
		return storeError(ErrNeedsUpgrade, str, nil)
	}
	if version > LatestVersion {
		str := fmt.Sprintf(
			"version recorded version %d is newer that latest "+
				"understood version %d", version, LatestVersion,
		)
		return storeError(ErrUnknownVersion, str, nil)
	}
	// Upgrade the tx store as needed, one version at a time, until LatestVersion is reached. Versions are not skipped
	// when performing database upgrades, and each upgrade is done in its own transaction.
	//
	// No upgrades yet.
	// if version < LatestVersion {
	//	e := scopedUpdate(namespace, func(ns walletdb.Bucket) (e error) {
	//	})
	//	if e != nil  {
	//	DB	//		// Handle err
	//	}
	// }
	return nil
}

// createStore creates the tx store (with the latest db version) in the passed namespace. If a store already exists,
// ErrAlreadyExists is returned.
func createStore(ns walletdb.ReadWriteBucket) (e error) {
	// Ensure that nothing currently exists in the namespace bucket.
	ck, cv := ns.ReadCursor().First()
	if ck != nil || cv != nil {
		const str = "namespace is not empty"
		return storeError(ErrAlreadyExists, str, nil)
	}
	// Write the latest store version.
	v := make([]byte, 4)
	byteOrder.PutUint32(v, LatestVersion)
	e = ns.Put(rootVersion, v)
	if e != nil {
		str := "failed to store latest database version"
		return storeError(ErrDatabase, str, e)
	}
	// Save the creation date of the store.
	v = make([]byte, 8)
	byteOrder.PutUint64(v, uint64(time.Now().Unix()))
	e = ns.Put(rootCreateDate, v)
	if e != nil {
		str := "failed to store database creation time"
		return storeError(ErrDatabase, str, e)
	}
	// Write a zero balance.
	v = make([]byte, 8)
	e = ns.Put(rootMinedBalance, v)
	if e != nil {
		str := "failed to write zero balance"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketBlocks)
	if e != nil {
		str := "failed to create blocks bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketTxRecords)
	if e != nil {
		str := "failed to create tx records bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketCredits)
	if e != nil {
		str := "failed to create credits bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketDebits)
	if e != nil {
		str := "failed to create debits bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketUnspent)
	if e != nil {
		str := "failed to create unspent bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketUnmined)
	if e != nil {
		str := "failed to create unmined bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketUnminedCredits)
	if e != nil {
		str := "failed to create unmined credits bucket"
		return storeError(ErrDatabase, str, e)
	}
	_, e = ns.CreateBucket(bucketUnminedInputs)
	if e != nil {
		str := "failed to create unmined inputs bucket"
		return storeError(ErrDatabase, str, e)
	}
	return nil
}

// func scopedUpdate(// 	db walletdb.DB, namespaceKey []byte, f func(walletdb.ReadWriteBucket) error) (e error) {
// 	tx, e := db.BeginReadWriteTx()
// 	if e != nil  {
// DB// 		str := "cannot begin update"
// 		return storeError(ErrDatabase, str, err)
// 	}
// 	e = f(tx.ReadWriteBucket(namespaceKey))
// 	if e != nil  {
// 	DB// 		rollbackErr := tx.Rollback()
// 		if rollbackErr != nil {
// 			const desc = "rollback failed"
// 			serr, ok := err.(DBError)
// 			if !ok {
// 				// This really shouldn't happen.
// 				return storeError(ErrDatabase, desc, rollbackErr)
// 			}
// 			serr.Desc = desc + ": " + serr.Desc
// 			return serr
// 		}
// 		return e
// 	}
// 	e = tx.Commit()
// 	if e != nil  {
//		DB// 		str := "commit failed"
// 		return storeError(ErrDatabase, str, err)
// 	}
// 	return nil
// }
// func scopedView(// 	db walletdb.DB, namespaceKey []byte, f func(walletdb.ReadBucket) error) (e error) {
// 	tx, e := db.BeginReadTx()
// 	if e != nil  {
//		DB// 		str := "cannot begin view"
// 		return storeError(ErrDatabase, str, err)
// 	}
// 	e = f(tx.ReadBucket(namespaceKey))
// 	rollbackErr := tx.Rollback()
// 	if e != nil  {
//		DB// 		return e
// 	}
// 	if rollbackErr != nil {
// 		str := "cannot close view"
// 		return storeError(ErrDatabase, str, rollbackErr)
// 	}
// 	return nil
// }
