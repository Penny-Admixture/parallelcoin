package headerfs

import (
	"bytes"
	"crypto/rand"
	"io/ioutil"
	"os"
	"testing"
	
	"github.com/p9c/pod/pkg/db/walletdb"
	_ "github.com/p9c/pod/pkg/db/walletdb/bdb"
)

func createTestIndex() (func(), *headerIndex, error) {
	tempDir, e := ioutil.TempDir("", "neutrino")
	if e != nil  {
		return nil, nil, e
	}
	db, e := walletdb.Create("bdb", tempDir+"/test.db")
	if e != nil  {
		return nil, nil, e
	}
	cleanUp := func() {
		if e := os.RemoveAll(tempDir); dbg.Chk(e) {
		}
		if e := db.Close(); dbg.Chk(e) {
		}
	}
	filterDB, e := newHeaderIndex(db, Block)
	if e != nil  {
		return nil, nil, e
	}
	return cleanUp, filterDB, nil
}

func TestAddHeadersIndexRetrieve(t *testing.T) {
	var e error
	var hIndex *headerIndex
	var cleanUp func()
	if cleanUp, hIndex, e = createTestIndex(); !dbg.Chk(e) {
		defer cleanUp()
	} else {
		t.Fatalf("unable to create test db: %v", err)
	}
	// First, we'll create a a series of random headers that we'll use to write into the database.
	const numHeaders = 100
	headerEntries := make(headerBatch, numHeaders)
	headerIndex := make(map[uint32]headerEntry)
	for i := uint32(0); i < numHeaders; i++ {
		var header headerEntry
		if _, e = rand.Read(header.hash[:]); dbg.Chk(e) {
			t.Fatalf("unable to read header: %v", err)
		}
		header.height = i
		headerEntries[i] = header
		headerIndex[i] = header
	}
	// With the headers constructed, we'll write them to disk in a single batch.
	if e := hIndex.addHeaders(headerEntries); dbg.Chk(e) {
		t.Fatalf("unable to add headers: %v", err)
	}
	// Next, verify that the database tip matches the _final_ header inserted.
	dbTip, dbHeight, e := hIndex.chainTip()
	if e != nil  {
		t.Fatalf("unable to obtain chain tip: %v", err)
	}
	lastEntry := headerIndex[numHeaders-1]
	if dbHeight != lastEntry.height {
		t.Fatalf("height doesn't match: expected %v, got %v",
			lastEntry.height, dbHeight)
	}
	if !bytes.Equal(dbTip[:], lastEntry.hash[:]) {
		t.Fatalf("tip doesn't match: expected %x, got %x",
			lastEntry.hash[:], dbTip[:])
	}
	// For each header written, check that we're able to retrieve the entry both by hash and height.
	for i, headerEntry := range headerEntries {
		height, e := hIndex.heightFromHash(&headerEntry.hash)
		if e != nil  {
			t.Fatalf("unable to retreive height(%v): %v", i, err)
		}
		if height != headerEntry.height {
			t.Fatalf("height doesn't match: expected %v, got %v",
				headerEntry.height, height)
		}
	}
	// Next if we truncate the index by one, then we should end up at the second to last entry for the tip.
	newTip := headerIndex[numHeaders-2]
	if e := hIndex.truncateIndex(&newTip.hash, true); dbg.Chk(e) {
		t.Fatalf("unable to truncate index: %v", err)
	}
	// This time the database tip should be the _second_ to last entry inserted.
	dbTip, dbHeight, e = hIndex.chainTip()
	if e != nil  {
		t.Fatalf("unable to obtain chain tip: %v", err)
	}
	lastEntry = headerIndex[numHeaders-2]
	if dbHeight != lastEntry.height {
		t.Fatalf("height doesn't match: expected %v, got %v",
			lastEntry.height, dbHeight)
	}
	if !bytes.Equal(dbTip[:], lastEntry.hash[:]) {
		t.Fatalf("tip doesn't match: expected %x, got %x",
			lastEntry.hash[:], dbTip[:])
	}
}
