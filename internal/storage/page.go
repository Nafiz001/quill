// Package storage is the on-disk half of the database: fixed-size pages, a
// buffer pool that caches them, and the B+tree built on top.
//
// Everything here exists because a database is bigger than memory. The unit of
// I/O is a page rather than a row, because disks and operating systems move
// data in blocks and reading one byte costs the same as reading four thousand.
// Every structure above this layer is shaped by that fact.
package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PageSize is the size of every page, in bytes.
//
// 4KB matches the page size of essentially every filesystem and the memory
// page size of every mainstream CPU, so a read never straddles two blocks and
// a write is one operation the whole way down the stack. It is the same size
// SQLite and PostgreSQL default to, for the same reason.
const PageSize = 4096

// PageID identifies a page by its position in the file. Page 0 is the header,
// so a valid data page is always >= 1 and 0 doubles as "no page".
type PageID uint32

// InvalidPageID marks an absent link, such as the sibling of an edge leaf.
const InvalidPageID PageID = 0

// PageType tags what a page holds.
type PageType uint8

const (
	PageTypeFree     PageType = 0
	PageTypeMeta     PageType = 1
	PageTypeLeaf     PageType = 2
	PageTypeInternal PageType = 3
	// PageTypeOverflow holds the tail of a value too large for one page.
	PageTypeOverflow PageType = 4
)

func (t PageType) String() string {
	switch t {
	case PageTypeMeta:
		return "meta"
	case PageTypeLeaf:
		return "leaf"
	case PageTypeInternal:
		return "internal"
	case PageTypeOverflow:
		return "overflow"
	default:
		return "free"
	}
}

// Page header layout. The offsets are fixed because they are part of the file
// format: changing one silently misreads every existing database.
const (
	offPageType  = 0  // uint8
	offNumSlots  = 2  // uint16
	offFreeStart = 4  // uint16, where the slot array ends
	offFreeEnd   = 6  // uint16, where tuple data begins
	offNextPage  = 8  // uint32
	offPrevPage  = 12 // uint32
	offLSN       = 16 // uint64, the last log record that touched this page
	// offRightChild is the extra child pointer an internal node needs: n keys
	// separate n+1 children, so one child has no key to live beside.
	offRightChild = 24 // uint32

	PageHeaderSize = 28

	slotSize = 4 // uint16 offset + uint16 length
)

var (
	// ErrPageFull means the tuple does not fit and the caller must split.
	ErrPageFull = errors.New("page is full")
	// ErrSlotNotFound means the slot index is out of range or was deleted.
	ErrSlotNotFound = errors.New("slot not found")
)

// Page is one fixed-size block, laid out as a slotted page.
//
//	+--------------------------------------------------+
//	| header | slot array →     ...     ← tuple data   |
//	+--------------------------------------------------+
//
// Slots grow forward from the header while tuple data grows backward from the
// end, and the free space between them is what remains. The indirection is the
// point: a slot is a fixed-size (offset, length) pair, so tuples can be
// variable-length and can be moved during compaction without any outside
// reference having to change -- a row is addressed by its slot number, not by
// where its bytes happen to sit.
type Page struct {
	data []byte
	id   PageID
}

// NewPage wraps a byte slice as a page. The slice is retained, not copied,
// because it is owned by the buffer pool frame this page lives in.
func NewPage(id PageID, data []byte) *Page {
	if len(data) != PageSize {
		panic(fmt.Sprintf("quill: page data is %d bytes, want %d", len(data), PageSize))
	}
	return &Page{data: data, id: id}
}

// Init prepares a freshly allocated page of the given type.
func (p *Page) Init(t PageType) {
	for i := range p.data {
		p.data[i] = 0
	}
	p.SetType(t)
	p.setUint16(offFreeStart, PageHeaderSize)
	p.setUint16(offFreeEnd, PageSize)
	p.SetNext(InvalidPageID)
	p.SetPrev(InvalidPageID)
	p.SetRightChild(InvalidPageID)
}

func (p *Page) ID() PageID     { return p.id }
func (p *Page) Data() []byte   { return p.data }
func (p *Page) Type() PageType { return PageType(p.data[offPageType]) }

func (p *Page) SetType(t PageType) { p.data[offPageType] = byte(t) }

func (p *Page) NumSlots() int { return int(p.getUint16(offNumSlots)) }

func (p *Page) Next() PageID      { return PageID(p.getUint32(offNextPage)) }
func (p *Page) SetNext(id PageID) { p.setUint32(offNextPage, uint32(id)) }

func (p *Page) Prev() PageID      { return PageID(p.getUint32(offPrevPage)) }
func (p *Page) SetPrev(id PageID) { p.setUint32(offPrevPage, uint32(id)) }

// RightChild is the rightmost child of an internal node, the one with no
// separator key to its right.
func (p *Page) RightChild() PageID      { return PageID(p.getUint32(offRightChild)) }
func (p *Page) SetRightChild(id PageID) { p.setUint32(offRightChild, uint32(id)) }

// LSN is the log sequence number of the last record that modified this page.
//
// Recovery compares it against the log to decide whether a change has already
// reached the page, which is what makes replay idempotent: a page written to
// disk before the crash must not have its changes applied twice.
func (p *Page) LSN() uint64       { return p.getUint64(offLSN) }
func (p *Page) SetLSN(lsn uint64) { p.setUint64(offLSN, lsn) }

func (p *Page) freeStart() int     { return int(p.getUint16(offFreeStart)) }
func (p *Page) freeEnd() int       { return int(p.getUint16(offFreeEnd)) }
func (p *Page) setFreeStart(v int) { p.setUint16(offFreeStart, uint16(v)) }
func (p *Page) setFreeEnd(v int)   { p.setUint16(offFreeEnd, uint16(v)) }
func (p *Page) setNumSlots(n int)  { p.setUint16(offNumSlots, uint16(n)) }

// FreeSpace is how many bytes remain between the slot array and the tuple data.
func (p *Page) FreeSpace() int {
	free := p.freeEnd() - p.freeStart()
	if free < 0 {
		return 0
	}
	return free
}

// slotOffset returns where slot i's descriptor lives.
func slotOffset(i int) int { return PageHeaderSize + i*slotSize }

// slot reads the (offset, length) pair for slot i.
func (p *Page) slot(i int) (offset, length int) {
	base := slotOffset(i)
	return int(binary.LittleEndian.Uint16(p.data[base:])),
		int(binary.LittleEndian.Uint16(p.data[base+2:]))
}

func (p *Page) setSlot(i, offset, length int) {
	base := slotOffset(i)
	binary.LittleEndian.PutUint16(p.data[base:], uint16(offset))
	binary.LittleEndian.PutUint16(p.data[base+2:], uint16(length))
}

// Get returns the bytes of slot i.
//
// The result aliases the page buffer, so it is only valid until the page is
// modified or unpinned. Copying here would allocate on every read of every
// row, which is the hottest path in the system; callers that need to retain a
// value copy it themselves.
func (p *Page) Get(i int) ([]byte, error) {
	if i < 0 || i >= p.NumSlots() {
		return nil, ErrSlotNotFound
	}
	offset, length := p.slot(i)
	if offset == 0 {
		return nil, ErrSlotNotFound // deleted
	}
	return p.data[offset : offset+length], nil
}

// Insert places a tuple at slot index i, shifting later slots along.
//
// Insertion is by position rather than append-only because a B+tree node keeps
// its entries sorted, and the sort order is what makes a search a binary search
// instead of a scan.
func (p *Page) Insert(i int, tuple []byte) error {
	n := p.NumSlots()
	if i < 0 || i > n {
		return fmt.Errorf("slot index %d out of range for %d slots", i, n)
	}
	need := len(tuple) + slotSize
	if need > p.FreeSpace() {
		// Deleted tuples leave holes, so try reclaiming before giving up.
		p.Compact()
		if need > p.FreeSpace() {
			return ErrPageFull
		}
	}

	// Tuple data grows backward from the end of the page.
	end := p.freeEnd() - len(tuple)
	copy(p.data[end:], tuple)
	p.setFreeEnd(end)

	// Open a gap in the slot array. The array is small and this keeps slots in
	// key order, which is what the binary search depends on.
	for j := n; j > i; j-- {
		off, length := p.slot(j - 1)
		p.setSlot(j, off, length)
	}
	p.setSlot(i, end, len(tuple))
	p.setNumSlots(n + 1)
	p.setFreeStart(slotOffset(n + 1))
	return nil
}

// Append adds a tuple after the last slot.
func (p *Page) Append(tuple []byte) error { return p.Insert(p.NumSlots(), tuple) }

// Delete removes slot i, closing the gap in the slot array.
//
// The tuple's bytes are left behind as dead space and reclaimed later by
// Compact. Moving them here would cost O(page) on every delete for a benefit
// that only matters when the page is nearly full.
func (p *Page) Delete(i int) error {
	n := p.NumSlots()
	if i < 0 || i >= n {
		return ErrSlotNotFound
	}
	for j := i; j < n-1; j++ {
		off, length := p.slot(j + 1)
		p.setSlot(j, off, length)
	}
	p.setNumSlots(n - 1)
	p.setFreeStart(slotOffset(n - 1))
	return nil
}

// Replace overwrites slot i.
//
// A same-size or smaller value is written in place. A larger one cannot be, so
// it becomes a delete plus an insert, which is why the caller may still get
// ErrPageFull from what looks like an update.
func (p *Page) Replace(i int, tuple []byte) error {
	if i < 0 || i >= p.NumSlots() {
		return ErrSlotNotFound
	}
	offset, length := p.slot(i)
	if len(tuple) <= length {
		copy(p.data[offset:], tuple)
		p.setSlot(i, offset, len(tuple))
		return nil
	}

	if err := p.Delete(i); err != nil {
		return err
	}
	if err := p.Insert(i, tuple); err != nil {
		// Compaction inside Insert may still not free enough; the caller
		// splits. The old value is already gone, so the caller must treat a
		// failure here as needing a retry against a different page.
		return err
	}
	return nil
}

// Compact rewrites the tuple area with the dead space removed.
//
// Tuples are copied into a scratch buffer in slot order and written back
// contiguously. Because every reference is a slot index rather than an offset,
// nothing outside the page has to be told the bytes moved -- which is the whole
// reason for the indirection.
func (p *Page) Compact() {
	n := p.NumSlots()
	scratch := make([]byte, 0, PageSize-PageHeaderSize)
	offsets := make([]int, n)
	lengths := make([]int, n)

	// Walk backward so the first slot ends up highest in the page, matching
	// the layout a sequence of Inserts would have produced.
	end := PageSize
	for i := n - 1; i >= 0; i-- {
		offset, length := p.slot(i)
		if offset == 0 {
			lengths[i] = 0
			continue
		}
		scratch = append(scratch, p.data[offset:offset+length]...)
		end -= length
		offsets[i] = end
		lengths[i] = length
	}

	// scratch holds the tuples in reverse slot order; write them back at their
	// new offsets.
	cursor := 0
	for i := n - 1; i >= 0; i-- {
		if lengths[i] == 0 {
			continue
		}
		copy(p.data[offsets[i]:], scratch[cursor:cursor+lengths[i]])
		cursor += lengths[i]
		p.setSlot(i, offsets[i], lengths[i])
	}
	p.setFreeEnd(end)
}

// --- little-endian helpers ---
//
// Little-endian throughout, because every machine this will realistically run
// on is little-endian and it makes the encoding a straight memory copy.

func (p *Page) getUint16(off int) uint16    { return binary.LittleEndian.Uint16(p.data[off:]) }
func (p *Page) setUint16(off int, v uint16) { binary.LittleEndian.PutUint16(p.data[off:], v) }
func (p *Page) getUint32(off int) uint32    { return binary.LittleEndian.Uint32(p.data[off:]) }
func (p *Page) setUint32(off int, v uint32) { binary.LittleEndian.PutUint32(p.data[off:], v) }
func (p *Page) getUint64(off int) uint64    { return binary.LittleEndian.Uint64(p.data[off:]) }
func (p *Page) setUint64(off int, v uint64) { binary.LittleEndian.PutUint64(p.data[off:], v) }
