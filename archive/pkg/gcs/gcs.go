package gcs

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	
	"github.com/aead/siphash"
	"github.com/kkdai/bstream"
	
	"github.com/p9c/pod/pkg/wire"
)

// Inspired by https://github.com/rasky/gcs
var (
	// ErrNTooBig signifies that the filter can't handle N items.
	ErrNTooBig = fmt.Errorf("N is too big to fit in uint32")
	// ErrPTooBig signifies that the filter can't handle `1/2**P` collision probability.
	ErrPTooBig = fmt.Errorf("P is too big to fit in uint32")
)

const (
	// KeySize is the size of the byte array required for key material for the
	// SipHash keyed hash function.
	KeySize = 16
	// varIntProtoVer is the protocol version to use for serializing N as a VarInt.
	varIntProtoVer uint32 = 0
)

// fastReduction calculates a mapping that's more ore less equivalent to: x mod
// N. However, instead of using a mod operation, which using a non-power of two
// will lead to slowness on many processors due to unnecessary division, we
// instead use a "multiply-and-shift" trick which eliminates all divisions,
// described in:
// https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
//  * v * N  >> log_2(N)
//
// In our case, using 64-bit integers, log_2 is 64. As most processors don't
// support 128-bit arithmetic natively, we'll be super portable and unfold the
// operation into several operations with 64-bit arithmetic. As inputs, we the
// number to reduce, and our modulus N divided into its high 32-bits and lower
// 32-bits.
func fastReduction(v, nHi, nLo uint64) uint64 {
	// First, we'll spit the item we need to reduce into its higher and lower bits.
	vhi := v >> 32
	vlo := uint64(uint32(v))
	// Then, we distribute multiplication over each part.
	vnphi := vhi * nHi
	vnpmid := vhi * nLo
	npvmid := nHi * vlo
	vnplo := vlo * nLo
	// We calculate the carry bit.
	carry := (uint64(uint32(vnpmid)) + uint64(uint32(npvmid)) +
		(vnplo >> 32)) >> 32
	// Last, we add the high bits, the middle bits, and the carry.
	v = vnphi + (vnpmid >> 32) + (npvmid >> 32) + carry
	return v
}

// Filter describes an immutable filter that can be built from a set of data
// elements, serialized, deserialized, and queried in a thread-safe manner. The
// serialized form is compressed as a Golomb Coded Set (GCS), but does not
// include N or P to allow the user to encode the metadata separately if
// necessary. The hash function used is SipHash, a keyed function; the key used
// in building the filter is required in order to match filter values and is not
// included in the serialized form.
type Filter struct {
	n          uint32
	p          uint8
	modulusNP  uint64
	filterData []byte
}

// BuildGCSFilter builds a new GCS filter with the collision probability of
// `1/(2**P)`, key `key`, and including every `[]byte` in `data` as a member of
// the set.
func BuildGCSFilter(P uint8, M uint64, key [KeySize]byte, data [][]byte) (*Filter, error) {
	// Some initial parameter checks: make sure we have data from which to podbuild the
	// filter, and make sure our parameters will fit the hash function we're using.
	if uint64(len(data)) >= (1 << 32) {
		return nil, ErrNTooBig
	}
	if P > 32 {
		return nil, ErrPTooBig
	}
	// Create the filter object and insert metadata.
	f := Filter{
		n: uint32(len(data)),
		p: P,
	}
	// First we'll compute the value of m, which is the modulus we use within our
	// finite field. We want to compute: mScalar * 2^P. We use math.Round in order
	// to round the value up, rather than down.
	f.modulusNP = uint64(f.n) * M
	// Shortcut if the filter is empty.
	if f.n == 0 {
		return &f, nil
	}
	// Build the filter.
	values := make(uint64Slice, 0, len(data))
	b := bstream.NewBStreamWriter(0)
	// Insert the hash (fast-ranged over a space of N*P) of each data element into a
	// slice and txsort the slice. This can be greatly optimized with native 128-bit
	// multiplication, but we're going to be fully portable for now.
	//
	// First, we cache the high and low bits of modulusNP for the multiplication of
	// 2 64-bit integers into a 128-bit integer.
	nphi := f.modulusNP >> 32
	nplo := uint64(uint32(f.modulusNP))
	for _, d := range data {
		// For each datum, we assign the initial hash to a uint64.
		v := siphash.Sum64(d, &key)
		v = fastReduction(v, nphi, nplo)
		values = append(values, v)
	}
	sort.Sort(values)
	// Write the sorted list of values into the filter bitstream, compressing it
	// using Golomb coding.
	var value, lastValue, remainder uint64
	for _, v := range values {
		// Calculate the difference between this value and the last, modulo P.
		remainder = (v - lastValue) & ((uint64(1) << f.p) - 1)
		// Calculate the difference between this value and the last, divided by P.
		value = (v - lastValue - remainder) >> f.p
		lastValue = v
		// Write the P multiple into the bitstream in unary; the average should be
		// around 1 (2 bits - 0b10).
		for value > 0 {
			b.WriteBit(true)
			value--
		}
		b.WriteBit(false)
		// Write the remainder as a big-endian integer with enough bits to represent the
		// appropriate collision probability.
		b.WriteBits(remainder, int(f.p))
	}
	// Copy the bitstream into the filter object and return the object.
	f.filterData = b.Bytes()
	return &f, nil
}

// FromBytes deserializes a GCS filter from a known N, P, and serialized filter
// as returned by Hash().
func FromBytes(N uint32, P uint8, M uint64, d []byte) (*Filter, error) {
	// Basic sanity check.
	if P > 32 {
		return nil, ErrPTooBig
	}
	// Create the filter object and insert metadata.
	f := &Filter{
		n: N,
		p: P,
	}
	// First we'll compute the value of m, which is the modulus we use within our
	// finite field. We want to compute: mScalar * 2^P. We use math.Round in order
	// to round the value up, rather than down.
	f.modulusNP = uint64(f.n) * M
	// Copy the filter.
	f.filterData = make([]byte, len(d))
	copy(f.filterData, d)
	return f, nil
}

// FromNBytes deserializes a GCS filter from a known P, and serialized N and
// filter as returned by NBytes().
func FromNBytes(P uint8, M uint64, d []byte) (*Filter, error) {
	buffer := bytes.NewBuffer(d)
	N, e := wire.ReadVarInt(buffer, varIntProtoVer)
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	if N >= (1 << 32) {
		return nil, ErrNTooBig
	}
	return FromBytes(uint32(N), P, M, buffer.Bytes())
}

// Bytes returns the serialized format of the GCS filter, which does not include
// N or P (returned by separate methods) or the key used by SipHash.
func (f *Filter) Bytes() ([]byte, error) {
	filterData := make([]byte, len(f.filterData))
	copy(filterData, f.filterData)
	return filterData, nil
}

// NBytes returns the serialized format of the GCS filter with N, which does not
// include P (returned by a separate method) or the key used by SipHash.
func (f *Filter) NBytes() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.Grow(wire.VarIntSerializeSize(uint64(f.n)) + len(f.filterData))
	e := wire.WriteVarInt(&buffer, varIntProtoVer, uint64(f.n))
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	_, e = buffer.Write(f.filterData)
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	return buffer.Bytes(), nil
}

// PBytes returns the serialized format of the GCS filter with P, which does not
// include N (returned by a separate method) or the key used by SipHash.
func (f *Filter) PBytes() ([]byte, error) {
	filterData := make([]byte, len(f.filterData)+1)
	filterData[0] = f.p
	copy(filterData[1:], f.filterData)
	return filterData, nil
}

// NPBytes returns the serialized format of the GCS filter with N and P, which does not include the key used by SipHash.
func (f *Filter) NPBytes() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.Grow(wire.VarIntSerializeSize(uint64(f.n)) + 1 + len(f.filterData))
	e := wire.WriteVarInt(&buffer, varIntProtoVer, uint64(f.n))
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	e = buffer.WriteByte(f.p)
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	_, e = buffer.Write(f.filterData)
	if e != nil {
		E.Ln(e)
		return nil, e
	}
	return buffer.Bytes(), nil
}

// P returns the filter's collision probability as a negative power of 2 (that is, a collision probability of `1/2**20`
// is represented as 20).
func (f *Filter) P() uint8 {
	return f.p
}

// N returns the size of the data set used to podbuild the filter.
func (f *Filter) N() uint32 {
	return f.n
}

// Match checks whether a []byte value is likely (within collision probability) to be a member of the set represented by
// the filter.
func (f *Filter) Match(key [KeySize]byte, data []byte) (yn bool, e error) {
	// Create a filter bitstream.
	var filterData []byte
	if filterData, e = f.Bytes(); E.Chk(e) {
		return false, e
	}
	b := bstream.NewBStreamReader(filterData)
	// We take the high and low bits of modulusNP for the multiplication of 2 64-bit integers into a 128-bit integer.
	nphi := f.modulusNP >> 32
	nplo := uint64(uint32(f.modulusNP))
	// Then we hash our search term with the same parameters as the filter.
	term := siphash.Sum64(data, &key)
	term = fastReduction(term, nphi, nplo)
	// Go through the search filter and look for the desired value.
	var lastValue uint64
	for lastValue < term {
		// Read the difference between previous and new value from bitstream.
		value, e := f.readFullUint64(b)
		if e != nil {
			E.Ln(e)
			if e == io.EOF {
				return false, nil
			}
			return false, e
		}
		// Add the previous value to it.
		value += lastValue
		if value == term {
			return true, nil
		}
		lastValue = value
	}
	return false, nil
}

// MatchAny returns checks whether any []byte value is likely (within collision probability) to be a member of the set
// represented by the filter faster than calling Match() for each value individually.
func (f *Filter) MatchAny(key [KeySize]byte, data [][]byte) (bool, error) {
	// Basic sanity check.
	if len(data) == 0 {
		return false, nil
	}
	// Create a filter bitstream.
	filterData, e := f.Bytes()
	if e != nil {
		E.Ln(e)
		return false, e
	}
	b := bstream.NewBStreamReader(filterData)
	// Create an uncompressed filter of the search values.
	values := make(uint64Slice, 0, len(data))
	// First, we cache the high and low bits of modulusNP for the multiplication of 2 64-bit integers into a 128-bit
	// integer.
	nphi := f.modulusNP >> 32
	nplo := uint64(uint32(f.modulusNP))
	for _, d := range data {
		// For each datum, we assign the initial hash to a uint64.
		v := siphash.Sum64(d, &key)
		// We'll then reduce the value down to the range of our modulus.
		v = fastReduction(v, nphi, nplo)
		values = append(values, v)
	}
	sort.Sort(values)
	// Zip down the filters, comparing values until we either run out of values to compare in one of the filters or we
	// reach a matching value.
	var lastValue1, lastValue2 uint64
	lastValue2 = values[0]
	i := 1
	for lastValue1 != lastValue2 {
		// Chk which filter to advance to make sure we're comparing the right values.
		switch {
		case lastValue1 > lastValue2:
			// Advance filter created from search terms or return false if we're at the end because nothing matched.
			if i < len(values) {
				lastValue2 = values[i]
				i++
			} else {
				return false, nil
			}
		case lastValue2 > lastValue1:
			// Advance filter we're searching or return false if we're at the end because nothing matched.
			value, e := f.readFullUint64(b)
			if e != nil {
				// E.Ln(e)
				if e == io.EOF {
					return false, nil
				}
				return false, e
			}
			lastValue1 += value
		}
	}
	// If we've made it this far, an element matched between filters so we return true.
	return true, nil
}

// readFullUint64 reads a value represented by the sum of a unary multiple of the filter's P modulus (`2**P`) and a
// big-endian P-bit remainder.
func (f *Filter) readFullUint64(b *bstream.BStream) (rv uint64, e error) {
	var quotient uint64
	// Count the 1s until we reach a 0.
	c, e := b.ReadBit()
	if e != nil {
		T.Ln(e)
		return 0, e
	}
	for c {
		quotient++
		c, e = b.ReadBit()
		if e != nil {
			D.Ln(e)
			return 0, e
		}
	}
	// Read P bits.
	var remainder uint64
	if remainder, e = b.ReadBits(int(f.p)); T.Chk(e) {
		D.Ln(e)
		return 0, e
	}
	// Add the multiple and the remainder.
	v := (quotient << f.p) + remainder
	return v, nil
}
