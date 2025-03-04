package votingpool

import (
	"bytes"
	"reflect"
	"testing"
	
	"github.com/p9c/pod/pkg/walletdb"
)

func TestPutUsedAddrHash(t *testing.T) {
	tearDown, db, pool := TstCreatePool(t)
	defer tearDown()
	dummyHash := bytes.Repeat([]byte{0x09}, 10)
	e := walletdb.Update(db, func(tx walletdb.ReadWriteTx) (e error) {
		ns, _ := TstRWNamespaces(tx)
		return putUsedAddrHash(ns, pool.ID, 0, 0, 0, dummyHash)
	})
	if e != nil  {
		t.Fatal(e)
	}
	var storedHash []byte
	e = walletdb.View(db, func(tx walletdb.ReadTx) (e error) {
		ns, _ := TstRNamespaces(tx)
		storedHash = getUsedAddrHash(ns, pool.ID, 0, 0, 0)
		return nil
	})
	if e != nil  {
		t.Fatal(e)
	}
	if !bytes.Equal(storedHash, dummyHash) {
		t.Fatalf("Wrong stored hash; got %x, want %x", storedHash, dummyHash)
	}
}
func TestGetMaxUsedIdx(t *testing.T) {
	tearDown, db, pool := TstCreatePool(t)
	defer tearDown()
	e := walletdb.Update(db, func(tx walletdb.ReadWriteTx) (e error) {
		ns, _ := TstRWNamespaces(tx)
		for i, idx := range []int{0, 7, 9, 3001, 41, 500, 6} {
			dummyHash := bytes.Repeat([]byte{byte(i)}, 10)
			e := putUsedAddrHash(ns, pool.ID, 0, 0, Index(idx), dummyHash)
			if e != nil  {
				return e
			}
		}
		return nil
	})
	if e != nil  {
		t.Fatal(e)
	}
	var maxIdx Index
	e = walletdb.View(db, func(tx walletdb.ReadTx) (e error) {
		ns, _ := TstRNamespaces(tx)
		maxIdx, e = getMaxUsedIdx(ns, pool.ID, 0, 0)
		return e
	})
	if e != nil  {
		t.Fatal(e)
	}
	if maxIdx != Index(3001) {
		t.Fatalf("Wrong max idx; got %d, want %d", maxIdx, Index(3001))
	}
}
func TestWithdrawalSerialization(t *testing.T) {
	tearDown, db, pool := TstCreatePool(t)
	defer tearDown()
	dbtx, e := db.BeginReadWriteTx()
	if e != nil  {
		t.Fatal(e)
	}
	defer func() {
		e := dbtx.Commit()
		if e != nil  {
			t.Log(e)
		}
	}()
	ns, addrmgrNs := TstRWNamespaces(dbtx)
	roundID := uint32(0)
	wi := createAndFulfillWithdrawalRequests(t, dbtx, pool, roundID)
	serialized, e := serializeWithdrawal(wi.requests, wi.startAddress, wi.lastSeriesID,
		wi.changeStart, wi.dustThreshold, wi.status)
	if e != nil  {
		t.Fatal(e)
	}
	var wInfo *withdrawalInfo
	TstRunWithManagerUnlocked(t, pool.Manager(), addrmgrNs, func() {
		wInfo, e = deserializeWithdrawal(pool, ns, addrmgrNs, serialized)
		if e != nil  {
			t.Fatal(e)
		}
	})
	if !reflect.DeepEqual(wInfo.startAddress, wi.startAddress) {
		t.Fatalf("Wrong startAddr; got %v, want %v", wInfo.startAddress, wi.startAddress)
	}
	if !reflect.DeepEqual(wInfo.changeStart, wi.changeStart) {
		t.Fatalf("Wrong changeStart; got %v, want %v", wInfo.changeStart, wi.changeStart)
	}
	if wInfo.lastSeriesID != wi.lastSeriesID {
		t.Fatalf("Wrong LastSeriesID; got %d, want %d", wInfo.lastSeriesID, wi.lastSeriesID)
	}
	if wInfo.dustThreshold != wi.dustThreshold {
		t.Fatalf("Wrong DustThreshold; got %d, want %d", wInfo.dustThreshold, wi.dustThreshold)
	}
	if !reflect.DeepEqual(wInfo.requests, wi.requests) {
		t.Fatalf("Wrong output requests; got %v, want %v", wInfo.requests, wi.requests)
	}
	TstCheckWithdrawalStatusMatches(t, wInfo.status, wi.status)
}
func TestPutAndGetWithdrawal(t *testing.T) {
	tearDown, db, _ := TstCreatePool(t)
	defer tearDown()
	serialized := bytes.Repeat([]byte{1}, 10)
	poolID := []byte{0x00}
	roundID := uint32(0)
	e := walletdb.Update(db, func(tx walletdb.ReadWriteTx) (e error) {
		ns, _ := TstRWNamespaces(tx)
		return putWithdrawal(ns, poolID, roundID, serialized)
	})
	if e != nil  {
		t.Fatal(e)
	}
	var retrieved []byte
	e = walletdb.View(db, func(tx walletdb.ReadTx) (e error) {
		ns, _ := TstRNamespaces(tx)
		retrieved = getWithdrawal(ns, poolID, roundID)
		return nil
	})
	if e != nil  {
		t.Fatal(e)
	}
	if !bytes.Equal(retrieved, serialized) {
		t.Fatalf("Wrong value retrieved from DB; got %x, want %x", retrieved, serialized)
	}
}
