package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// A B+tree is the index every table and every secondary index is built on.
//
// The shape matters. A binary tree of a million rows is twenty pointer chases,
// and on disk each one is a separate read. A B+tree fans out by hundreds per
// node instead, so the same million rows are three levels deep and a lookup is
// three page reads. That ratio -- fanout against tree height -- is the entire
// reason databases use this structure and not a balanced binary tree.
//
// The "+" is the second half of it: values live only in leaves, and the leaves
// are linked left to right. Internal nodes hold nothing but separator keys, so
// they pack more keys per page and the tree gets shallower still; and a range
// scan walks the leaf chain sequentially instead of re-descending the tree for
// every row. `SELECT ... WHERE id BETWEEN 100 AND 200` is one descent followed
// by a linear walk, which is what makes ordered scans cheap.
//
// Entry layout inside a page:
//
//	leaf:     varint(len(key)) | key | value
//	internal: varint(len(key)) | key | uint32 childPageID
//
// An internal node with n separator keys has n+1 children, so the extra child
// lives in the page header as RightChild.

// BTree is one tree: an index, or the heap of a table keyed by row id.
type BTree struct {
	pager *Pager
	root  PageID
	// onRootChange persists a new root page id. The root moves whenever the
	// tree grows or shrinks a level, and whoever owns the tree -- the catalog,
	// or the file header -- has to be told.
	onRootChange func(PageID) error
}

// NewBTree opens an existing tree at root.
func NewBTree(pager *Pager, root PageID, onRootChange func(PageID) error) *BTree {
	return &BTree{pager: pager, root: root, onRootChange: onRootChange}
}

// CreateBTree builds an empty tree and returns it with its root page.
func CreateBTree(pager *Pager, onRootChange func(PageID) error) (*BTree, error) {
	page, err := pager.Allocate(PageTypeLeaf)
	if err != nil {
		return nil, err
	}
	root := page.ID()
	if err := pager.Unpin(root, true); err != nil {
		return nil, err
	}
	return &BTree{pager: pager, root: root, onRootChange: onRootChange}, nil
}

// Root returns the current root page id.
func (t *BTree) Root() PageID { return t.root }

func (t *BTree) setRoot(id PageID) error {
	t.root = id
	if t.onRootChange != nil {
		return t.onRootChange(id)
	}
	return nil
}

// --- entry encoding ---

func encodeLeafEntry(key, value []byte) []byte {
	buf := make([]byte, 0, binary.MaxVarintLen64+len(key)+len(value))
	buf = binary.AppendUvarint(buf, uint64(len(key)))
	buf = append(buf, key...)
	buf = append(buf, value...)
	return buf
}

func decodeLeafEntry(entry []byte) (key, value []byte, err error) {
	n, read := binary.Uvarint(entry)
	if read <= 0 || int(n)+read > len(entry) {
		return nil, nil, fmt.Errorf("corrupt leaf entry")
	}
	return entry[read : read+int(n)], entry[read+int(n):], nil
}

func encodeInternalEntry(key []byte, child PageID) []byte {
	buf := make([]byte, 0, binary.MaxVarintLen64+len(key)+4)
	buf = binary.AppendUvarint(buf, uint64(len(key)))
	buf = append(buf, key...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(child))
	return buf
}

func decodeInternalEntry(entry []byte) (key []byte, child PageID, err error) {
	n, read := binary.Uvarint(entry)
	if read <= 0 || read+int(n)+4 != len(entry) {
		return nil, 0, fmt.Errorf("corrupt internal entry")
	}
	key = entry[read : read+int(n)]
	child = PageID(binary.LittleEndian.Uint32(entry[read+int(n):]))
	return key, child, nil
}

// entryKey reads just the key of an entry, for comparisons during search.
func entryKey(entry []byte) ([]byte, error) {
	n, read := binary.Uvarint(entry)
	if read <= 0 || read+int(n) > len(entry) {
		return nil, fmt.Errorf("corrupt entry")
	}
	return entry[read : read+int(n)], nil
}

// --- search ---

// searchLeaf finds where key belongs in a leaf: the index of the first entry
// whose key is greater than or equal to the target, and whether it is an exact
// match.
//
// Binary search rather than a scan is the reason entries are kept sorted:
// with a few hundred entries per page it is eight comparisons instead of a
// few hundred, on every level of every lookup.
func searchLeaf(p *Page, key []byte) (index int, exact bool, err error) {
	lo, hi := 0, p.NumSlots()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		entry, err := p.Get(mid)
		if err != nil {
			return 0, false, err
		}
		k, err := entryKey(entry)
		if err != nil {
			return 0, false, err
		}
		switch bytes.Compare(k, key) {
		case 0:
			return mid, true, nil
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false, nil
}

// childFor returns the child of an internal node that a key belongs under.
//
// Entry i holds separator key s_i and child c_i, and c_i covers keys strictly
// below s_i. A key at or above every separator belongs to RightChild, which is
// the child with no separator to its right.
func childFor(p *Page, key []byte) (PageID, int, error) {
	n := p.NumSlots()
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		entry, err := p.Get(mid)
		if err != nil {
			return 0, 0, err
		}
		sep, err := entryKey(entry)
		if err != nil {
			return 0, 0, err
		}
		if bytes.Compare(key, sep) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo == n {
		return p.RightChild(), n, nil
	}
	entry, err := p.Get(lo)
	if err != nil {
		return 0, 0, err
	}
	_, child, err := decodeInternalEntry(entry)
	return child, lo, err
}

// Get looks up a key.
func (t *BTree) Get(key []byte) ([]byte, bool, error) {
	pid := t.root
	for {
		page, err := t.pager.Fetch(pid)
		if err != nil {
			return nil, false, err
		}

		if page.Type() == PageTypeLeaf {
			index, exact, err := searchLeaf(page, key)
			if err != nil {
				t.pager.Unpin(pid, false)
				return nil, false, err
			}
			if !exact {
				t.pager.Unpin(pid, false)
				return nil, false, nil
			}
			entry, err := page.Get(index)
			if err != nil {
				t.pager.Unpin(pid, false)
				return nil, false, err
			}
			_, value, err := decodeLeafEntry(entry)
			// The entry aliases the page buffer, which is only valid while the
			// page is pinned, so the value is copied before letting go.
			out := append([]byte(nil), value...)
			t.pager.Unpin(pid, false)
			return out, err == nil, err
		}

		child, _, err := childFor(page, key)
		t.pager.Unpin(pid, false)
		if err != nil {
			return nil, false, err
		}
		if child == InvalidPageID {
			return nil, false, fmt.Errorf("internal node %d has no child for the key", pid)
		}
		pid = child
	}
}

// --- insertion ---

// Put inserts or replaces a key.
func (t *BTree) Put(key, value []byte) error {
	promoteKey, newRight, err := t.insert(t.root, key, value)
	if err != nil {
		return err
	}
	if promoteKey == nil {
		return nil
	}

	// The root split, so the tree grows a level. This is the only way a
	// B+tree gets taller, and it is why every leaf stays at the same depth --
	// growth happens at the top, not the bottom.
	newRoot, err := t.pager.Allocate(PageTypeInternal)
	if err != nil {
		return err
	}
	if err := newRoot.Append(encodeInternalEntry(promoteKey, t.root)); err != nil {
		t.pager.Unpin(newRoot.ID(), true)
		return err
	}
	newRoot.SetRightChild(newRight)
	rootID := newRoot.ID()
	if err := t.pager.Unpin(rootID, true); err != nil {
		return err
	}
	return t.setRoot(rootID)
}

// insert descends to the right leaf and inserts, splitting on the way back up.
//
// It returns a separator key and a new right sibling when the page it was
// given had to split; the caller inserts that pair into the parent, which may
// split in turn. Recursion carries the split upward exactly as far as it needs
// to go.
func (t *BTree) insert(pid PageID, key, value []byte) ([]byte, PageID, error) {
	page, err := t.pager.Fetch(pid)
	if err != nil {
		return nil, 0, err
	}

	if page.Type() == PageTypeLeaf {
		return t.insertIntoLeaf(page, key, value)
	}

	child, _, err := childFor(page, key)
	if err != nil {
		t.pager.Unpin(pid, false)
		return nil, 0, err
	}
	// The parent is unpinned during the descent so a deep tree does not need
	// one frame per level held simultaneously.
	t.pager.Unpin(pid, false)

	promoteKey, newRight, err := t.insert(child, key, value)
	if err != nil {
		return nil, 0, err
	}
	if promoteKey == nil {
		return nil, 0, nil
	}

	page, err = t.pager.Fetch(pid)
	if err != nil {
		return nil, 0, err
	}
	return t.insertIntoInternal(page, promoteKey, newRight)
}

func (t *BTree) insertIntoLeaf(page *Page, key, value []byte) ([]byte, PageID, error) {
	pid := page.ID()
	index, exact, err := searchLeaf(page, key)
	if err != nil {
		t.pager.Unpin(pid, false)
		return nil, 0, err
	}

	entry := encodeLeafEntry(key, value)
	if exact {
		err = page.Replace(index, entry)
	} else {
		err = page.Insert(index, entry)
	}
	if err == nil {
		t.pager.Unpin(pid, true)
		return nil, 0, nil
	}
	if err != ErrPageFull {
		t.pager.Unpin(pid, true)
		return nil, 0, err
	}

	// The leaf is full, so it splits and the caller learns about the new half.
	return t.splitLeaf(page, index, entry, exact)
}

// splitLeaf divides a full leaf and inserts the pending entry into whichever
// half it belongs to.
func (t *BTree) splitLeaf(page *Page, index int, entry []byte, replacing bool) ([]byte, PageID, error) {
	pid := page.ID()

	// Collect the current entries plus the new one, in key order. Working from
	// a materialised list keeps the split logic simple; a page holds at most a
	// few hundred entries, so the copy is bounded.
	var entries [][]byte
	for i := 0; i < page.NumSlots(); i++ {
		e, err := page.Get(i)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		entries = append(entries, append([]byte(nil), e...))
	}
	if replacing {
		entries[index] = entry
	} else {
		entries = append(entries, nil)
		copy(entries[index+1:], entries[index:])
		entries[index] = entry
	}

	right, err := t.pager.Allocate(PageTypeLeaf)
	if err != nil {
		t.pager.Unpin(pid, false)
		return nil, 0, err
	}
	rightID := right.ID()

	// Split at the midpoint. A split at the median keeps both halves at least
	// half full, which is the invariant that bounds the tree's height.
	mid := len(entries) / 2

	// Capture the sibling links before Init clears the header. Reading them
	// afterwards loses the chain, and a broken chain is invisible to point
	// lookups -- they descend the tree -- while silently truncating every
	// range scan.
	oldNext := page.Next()
	oldPrev := page.Prev()

	page.Init(PageTypeLeaf)
	page.SetPrev(oldPrev)
	for _, e := range entries[:mid] {
		if err := page.Append(e); err != nil {
			t.pager.Unpin(pid, true)
			t.pager.Unpin(rightID, true)
			return nil, 0, fmt.Errorf("left half of a leaf split does not fit: %w", err)
		}
	}
	for _, e := range entries[mid:] {
		if err := right.Append(e); err != nil {
			t.pager.Unpin(pid, true)
			t.pager.Unpin(rightID, true)
			return nil, 0, fmt.Errorf("right half of a leaf split does not fit: %w", err)
		}
	}

	// Relink the leaf chain, so range scans still walk straight through.
	right.SetNext(oldNext)
	right.SetPrev(pid)
	page.SetNext(rightID)

	// The separator is the first key of the right half. In a B+tree it is
	// copied up rather than moved, because the value still has to live in a
	// leaf for the scan chain to reach it.
	promoteKey, err := entryKey(entries[mid])
	if err != nil {
		t.pager.Unpin(pid, true)
		t.pager.Unpin(rightID, true)
		return nil, 0, err
	}
	promote := append([]byte(nil), promoteKey...)

	t.pager.Unpin(pid, true)
	t.pager.Unpin(rightID, true)

	// Fix the old successor's back pointer, now that both halves are written.
	if oldNext != InvalidPageID {
		nextPage, err := t.pager.Fetch(oldNext)
		if err != nil {
			return nil, 0, err
		}
		nextPage.SetPrev(rightID)
		if err := t.pager.Unpin(oldNext, true); err != nil {
			return nil, 0, err
		}
	}
	return promote, rightID, nil
}

func (t *BTree) insertIntoInternal(page *Page, key []byte, child PageID) ([]byte, PageID, error) {
	pid := page.ID()

	// Find where the separator belongs: the first slot whose key is greater.
	index := page.NumSlots()
	for i := 0; i < page.NumSlots(); i++ {
		e, err := page.Get(i)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		sep, err := entryKey(e)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		if bytes.Compare(key, sep) < 0 {
			index = i
			break
		}
	}

	// The new page becomes the child to the right of the new separator, so it
	// takes over whatever pointer used to sit there.
	var displaced PageID
	if index == page.NumSlots() {
		displaced = page.RightChild()
		page.SetRightChild(child)
	} else {
		e, err := page.Get(index)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		_, existing, err := decodeInternalEntry(e)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		displaced = existing
		// Rewrite the displaced entry to point at the new page; the old child
		// becomes the one the new separator sits above.
		sepKey, err := entryKey(e)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		if err := page.Replace(index, encodeInternalEntry(append([]byte(nil), sepKey...), child)); err != nil {
			t.pager.Unpin(pid, true)
			return nil, 0, err
		}
	}

	newEntry := encodeInternalEntry(key, displaced)
	if err := page.Insert(index, newEntry); err == nil {
		t.pager.Unpin(pid, true)
		return nil, 0, nil
	} else if err != ErrPageFull {
		t.pager.Unpin(pid, true)
		return nil, 0, err
	}

	return t.splitInternal(page, index, newEntry)
}

// splitInternal divides a full internal node.
//
// Unlike a leaf split, the median key moves up rather than being copied: an
// internal node holds separators only, so once the key is in the parent there
// is nothing left for it to do here.
func (t *BTree) splitInternal(page *Page, index int, entry []byte) ([]byte, PageID, error) {
	pid := page.ID()

	var entries [][]byte
	for i := 0; i < page.NumSlots(); i++ {
		e, err := page.Get(i)
		if err != nil {
			t.pager.Unpin(pid, false)
			return nil, 0, err
		}
		entries = append(entries, append([]byte(nil), e...))
	}
	entries = append(entries, nil)
	copy(entries[index+1:], entries[index:])
	entries[index] = entry

	right, err := t.pager.Allocate(PageTypeInternal)
	if err != nil {
		t.pager.Unpin(pid, false)
		return nil, 0, err
	}
	rightID := right.ID()

	mid := len(entries) / 2
	medianKey, medianChild, err := decodeInternalEntry(entries[mid])
	if err != nil {
		t.pager.Unpin(pid, true)
		t.pager.Unpin(rightID, true)
		return nil, 0, err
	}
	promote := append([]byte(nil), medianKey...)

	oldRight := page.RightChild()

	page.Init(PageTypeInternal)
	for _, e := range entries[:mid] {
		if err := page.Append(e); err != nil {
			t.pager.Unpin(pid, true)
			t.pager.Unpin(rightID, true)
			return nil, 0, fmt.Errorf("left half of an internal split does not fit: %w", err)
		}
	}
	// The median's child becomes the left node's rightmost, since the median
	// key itself is leaving for the parent.
	page.SetRightChild(medianChild)

	for _, e := range entries[mid+1:] {
		if err := right.Append(e); err != nil {
			t.pager.Unpin(pid, true)
			t.pager.Unpin(rightID, true)
			return nil, 0, fmt.Errorf("right half of an internal split does not fit: %w", err)
		}
	}
	right.SetRightChild(oldRight)

	t.pager.Unpin(pid, true)
	t.pager.Unpin(rightID, true)
	return promote, rightID, nil
}

// --- deletion ---

// Delete removes a key, reporting whether it was there.
//
// Underflowing nodes are not merged. A B+tree that only ever deletes will leave
// pages sparser than necessary, which costs space and makes scans read more
// pages than they need to; rebalancing on delete is the fix, and it is listed
// as future work rather than half-implemented. What is guaranteed here is that
// the tree stays correct and search stays O(log n) -- an underfull node is
// still a sorted node.
func (t *BTree) Delete(key []byte) (bool, error) {
	pid := t.root
	for {
		page, err := t.pager.Fetch(pid)
		if err != nil {
			return false, err
		}

		if page.Type() == PageTypeLeaf {
			index, exact, err := searchLeaf(page, key)
			if err != nil {
				t.pager.Unpin(pid, false)
				return false, err
			}
			if !exact {
				t.pager.Unpin(pid, false)
				return false, nil
			}
			err = page.Delete(index)
			t.pager.Unpin(pid, true)
			return err == nil, err
		}

		child, _, err := childFor(page, key)
		t.pager.Unpin(pid, false)
		if err != nil {
			return false, err
		}
		pid = child
	}
}

// --- iteration ---

// Cursor walks entries in key order.
//
// It follows the leaf chain rather than re-descending the tree, which is the
// difference between a range scan costing one descent plus a linear walk and
// costing a descent per row.
type Cursor struct {
	tree *BTree
	pid  PageID
	slot int
	// end bounds the scan; nil means run to the end of the tree.
	end       []byte
	endExcl   bool
	key, val  []byte
	valid     bool
	err       error
	pinnedPID PageID
}

// Scan returns a cursor over [start, end). A nil start begins at the first
// key; a nil end runs to the last.
func (t *BTree) Scan(start, end []byte) (*Cursor, error) {
	c := &Cursor{tree: t, end: end, endExcl: true, pinnedPID: InvalidPageID}

	pid := t.root
	for {
		page, err := t.pager.Fetch(pid)
		if err != nil {
			return nil, err
		}
		if page.Type() == PageTypeLeaf {
			index := 0
			if start != nil {
				index, _, err = searchLeaf(page, start)
				if err != nil {
					t.pager.Unpin(pid, false)
					return nil, err
				}
			}
			t.pager.Unpin(pid, false)
			c.pid = pid
			c.slot = index
			c.advanceToValid()
			return c, nil
		}
		var child PageID
		if start == nil {
			// Leftmost descent, for a scan with no lower bound.
			e, err := page.Get(0)
			if err != nil {
				t.pager.Unpin(pid, false)
				return nil, err
			}
			_, child, err = decodeInternalEntry(e)
			if err != nil {
				t.pager.Unpin(pid, false)
				return nil, err
			}
		} else {
			child, _, err = childFor(page, start)
			if err != nil {
				t.pager.Unpin(pid, false)
				return nil, err
			}
		}
		t.pager.Unpin(pid, false)
		pid = child
	}
}

// advanceToValid positions the cursor on the next live entry, crossing into
// the following leaf when the current one runs out.
func (c *Cursor) advanceToValid() {
	for {
		if c.pid == InvalidPageID {
			c.valid = false
			return
		}
		page, err := c.tree.pager.Fetch(c.pid)
		if err != nil {
			c.err = err
			c.valid = false
			return
		}
		if c.slot >= page.NumSlots() {
			next := page.Next()
			c.tree.pager.Unpin(c.pid, false)
			c.pid = next
			c.slot = 0
			continue
		}

		entry, err := page.Get(c.slot)
		if err != nil {
			c.tree.pager.Unpin(c.pid, false)
			c.err = err
			c.valid = false
			return
		}
		key, value, err := decodeLeafEntry(entry)
		if err != nil {
			c.tree.pager.Unpin(c.pid, false)
			c.err = err
			c.valid = false
			return
		}

		if c.end != nil {
			cmp := bytes.Compare(key, c.end)
			if cmp > 0 || (c.endExcl && cmp == 0) {
				c.tree.pager.Unpin(c.pid, false)
				c.valid = false
				return
			}
		}

		// Copied out, because the page is unpinned as soon as this returns.
		c.key = append([]byte(nil), key...)
		c.val = append([]byte(nil), value...)
		c.tree.pager.Unpin(c.pid, false)
		c.valid = true
		return
	}
}

// Next advances to the following entry, reporting whether one exists.
func (c *Cursor) Next() bool {
	if !c.valid {
		return false
	}
	current := c.valid
	c.slot++
	held := current
	_ = held
	c.advanceToValid()
	return c.valid
}

// Valid reports whether the cursor is positioned on an entry.
func (c *Cursor) Valid() bool { return c.valid }

// Key and Value return the current entry. They are only meaningful while
// Valid reports true.
func (c *Cursor) Key() []byte   { return c.key }
func (c *Cursor) Value() []byte { return c.val }
func (c *Cursor) Err() error    { return c.err }

// --- diagnostics ---

// Height returns how many levels the tree has, for tests and EXPLAIN.
func (t *BTree) Height() (int, error) {
	height := 1
	pid := t.root
	for {
		page, err := t.pager.Fetch(pid)
		if err != nil {
			return 0, err
		}
		if page.Type() == PageTypeLeaf {
			t.pager.Unpin(pid, false)
			return height, nil
		}
		e, err := page.Get(0)
		if err != nil {
			t.pager.Unpin(pid, false)
			return 0, err
		}
		_, child, err := decodeInternalEntry(e)
		t.pager.Unpin(pid, false)
		if err != nil {
			return 0, err
		}
		pid = child
		height++
	}
}

// Verify walks the whole tree and checks its invariants: keys sorted within
// every node, every key in a child within the range its separators promise,
// and all leaves at the same depth.
//
// Split logic is the kind of code where an off-by-one produces a tree that
// still answers most queries correctly, so the tests check the structure
// directly rather than trusting the results.
func (t *BTree) Verify() error {
	depth := -1
	return t.verifyNode(t.root, nil, nil, 0, &depth)
}

func (t *BTree) verifyNode(pid PageID, lower, upper []byte, level int, leafDepth *int) error {
	page, err := t.pager.Fetch(pid)
	if err != nil {
		return err
	}
	defer t.pager.Unpin(pid, false)

	n := page.NumSlots()
	var prev []byte
	for i := 0; i < n; i++ {
		e, err := page.Get(i)
		if err != nil {
			return err
		}
		key, err := entryKey(e)
		if err != nil {
			return err
		}
		if prev != nil && bytes.Compare(prev, key) >= 0 {
			return fmt.Errorf("page %d: keys out of order at slot %d", pid, i)
		}
		if lower != nil && bytes.Compare(key, lower) < 0 {
			return fmt.Errorf("page %d: key %q is below the lower bound %q", pid, key, lower)
		}
		if upper != nil && bytes.Compare(key, upper) >= 0 {
			return fmt.Errorf("page %d: key %q is at or above the upper bound %q", pid, key, upper)
		}
		prev = append([]byte(nil), key...)
	}

	if page.Type() == PageTypeLeaf {
		if *leafDepth == -1 {
			*leafDepth = level
		} else if *leafDepth != level {
			return fmt.Errorf("leaf %d is at depth %d, but another leaf is at %d", pid, level, *leafDepth)
		}
		return nil
	}

	if n == 0 {
		return fmt.Errorf("internal page %d has no separators", pid)
	}

	// Each child is bounded by the separators on either side of it.
	childLower := lower
	for i := 0; i < n; i++ {
		e, err := page.Get(i)
		if err != nil {
			return err
		}
		sep, child, err := decodeInternalEntry(e)
		if err != nil {
			return err
		}
		if err := t.verifyNode(child, childLower, sep, level+1, leafDepth); err != nil {
			return err
		}
		childLower = sep
	}
	if page.RightChild() == InvalidPageID {
		return fmt.Errorf("internal page %d has no right child", pid)
	}
	return t.verifyNode(page.RightChild(), childLower, upper, level+1, leafDepth)
}
