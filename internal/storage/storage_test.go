package storage

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func newTestPage(t *testing.T) *Page {
	t.Helper()
	p := NewPage(1, make([]byte, PageSize))
	p.Init(PageTypeLeaf)
	return p
}

func TestPageInitAndHeader(t *testing.T) {
	p := newTestPage(t)

	if p.Type() != PageTypeLeaf {
		t.Errorf("type: got %v, want leaf", p.Type())
	}
	if p.NumSlots() != 0 {
		t.Errorf("a fresh page has %d slots, want 0", p.NumSlots())
	}
	if got, want := p.FreeSpace(), PageSize-PageHeaderSize; got != want {
		t.Errorf("free space: got %d, want %d", got, want)
	}
	if p.Next() != InvalidPageID || p.Prev() != InvalidPageID {
		t.Error("a fresh page should have no sibling links")
	}

	p.SetNext(42)
	p.SetPrev(7)
	p.SetRightChild(99)
	p.SetLSN(1234567890)
	if p.Next() != 42 || p.Prev() != 7 || p.RightChild() != 99 || p.LSN() != 1234567890 {
		t.Error("header fields did not round-trip")
	}
	// Header fields must not collide with each other.
	if p.Next() == p.Prev() {
		t.Error("next and prev share storage")
	}
}

func TestPageInsertAndGet(t *testing.T) {
	p := newTestPage(t)

	tuples := [][]byte{
		[]byte("first"),
		[]byte("second value"),
		[]byte(""),
		[]byte("binary\x00\xff data"),
	}
	for i, tuple := range tuples {
		if err := p.Append(tuple); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if p.NumSlots() != len(tuples) {
		t.Fatalf("got %d slots, want %d", p.NumSlots(), len(tuples))
	}
	for i, want := range tuples {
		got, err := p.Get(i)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("slot %d: got %q, want %q", i, got, want)
		}
	}

	if _, err := p.Get(-1); err != ErrSlotNotFound {
		t.Errorf("negative index: got %v", err)
	}
	if _, err := p.Get(len(tuples)); err != ErrSlotNotFound {
		t.Errorf("index past the end: got %v", err)
	}
}

// Inserting in the middle must shift later slots, which is what keeps a node's
// entries in key order.
func TestPageInsertInMiddle(t *testing.T) {
	p := newTestPage(t)
	p.Append([]byte("a"))
	p.Append([]byte("c"))

	if err := p.Insert(1, []byte("b")); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		got, _ := p.Get(i)
		if string(got) != w {
			t.Errorf("slot %d: got %q, want %q", i, got, w)
		}
	}

	// Inserting at the front too.
	if err := p.Insert(0, []byte("start")); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(0); string(got) != "start" {
		t.Errorf("slot 0: got %q", got)
	}
	if got, _ := p.Get(3); string(got) != "c" {
		t.Errorf("slot 3: got %q", got)
	}
}

func TestPageDelete(t *testing.T) {
	p := newTestPage(t)
	for _, s := range []string{"a", "b", "c", "d"} {
		p.Append([]byte(s))
	}

	if err := p.Delete(1); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 3 {
		t.Errorf("got %d slots after delete, want 3", p.NumSlots())
	}
	want := []string{"a", "c", "d"}
	for i, w := range want {
		got, _ := p.Get(i)
		if string(got) != w {
			t.Errorf("slot %d: got %q, want %q", i, got, w)
		}
	}

	if err := p.Delete(99); err != ErrSlotNotFound {
		t.Errorf("delete out of range: got %v", err)
	}
}

func TestPageReplace(t *testing.T) {
	p := newTestPage(t)
	p.Append([]byte("original"))
	p.Append([]byte("second"))

	// Shorter replacement is written in place.
	if err := p.Replace(0, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(0); string(got) != "short" {
		t.Errorf("after shrink: got %q", got)
	}
	if got, _ := p.Get(1); string(got) != "second" {
		t.Errorf("neighbour was disturbed: got %q", got)
	}

	// Longer replacement has to move.
	if err := p.Replace(0, []byte("a much longer value than before")); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(0); string(got) != "a much longer value than before" {
		t.Errorf("after grow: got %q", got)
	}
	if got, _ := p.Get(1); string(got) != "second" {
		t.Errorf("neighbour was disturbed by the grow: got %q", got)
	}
	if p.NumSlots() != 2 {
		t.Errorf("replace changed the slot count: %d", p.NumSlots())
	}
}

func TestPageFull(t *testing.T) {
	p := newTestPage(t)

	// Fill until it refuses.
	big := bytes.Repeat([]byte("x"), 512)
	count := 0
	for {
		if err := p.Append(big); err != nil {
			if err != ErrPageFull {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
		count++
		if count > 100 {
			t.Fatal("page never filled up")
		}
	}
	if count == 0 {
		t.Fatal("nothing fit in an empty page")
	}

	// Everything that did fit must still be readable and correct.
	for i := 0; i < count; i++ {
		got, err := p.Get(i)
		if err != nil {
			t.Fatalf("slot %d after filling: %v", i, err)
		}
		if !bytes.Equal(got, big) {
			t.Fatalf("slot %d is corrupted", i)
		}
	}
}

// Deleting leaves dead space; compaction must reclaim it without changing what
// any slot index refers to.
func TestPageCompact(t *testing.T) {
	p := newTestPage(t)
	payload := bytes.Repeat([]byte("y"), 400)

	for i := 0; i < 8; i++ {
		if err := p.Append(payload); err != nil {
			t.Fatalf("setup append %d: %v", i, err)
		}
	}
	// Remove every other one.
	for i := 6; i >= 0; i -= 2 {
		if err := p.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	if p.NumSlots() != 4 {
		t.Fatalf("got %d slots, want 4", p.NumSlots())
	}

	beforeFree := p.FreeSpace()
	p.Compact()
	afterFree := p.FreeSpace()
	if afterFree <= beforeFree {
		t.Errorf("compaction reclaimed nothing: %d -> %d", beforeFree, afterFree)
	}

	// Contents survive, at the same indexes.
	for i := 0; i < 4; i++ {
		got, err := p.Get(i)
		if err != nil {
			t.Fatalf("slot %d after compaction: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("slot %d is corrupted after compaction", i)
		}
	}
	// And the reclaimed space is usable.
	if err := p.Append(payload); err != nil {
		t.Errorf("cannot use reclaimed space: %v", err)
	}
}

// --- pager ---

func newTestPager(t *testing.T, poolSize int) *Pager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path, poolSize)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestPagerCreateAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	p, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	id := page.ID()
	if err := page.Append([]byte("persisted")); err != nil {
		t.Fatal(err)
	}
	if err := p.Unpin(id, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and read it back.
	p2, err := Open(path, 16)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()

	got, err := p2.Fetch(id)
	if err != nil {
		t.Fatal(err)
	}
	value, err := got.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "persisted" {
		t.Errorf("after reopen: got %q, want persisted", value)
	}
	if got.Type() != PageTypeLeaf {
		t.Errorf("page type did not survive: %v", got.Type())
	}
	p2.Unpin(id, false)
}

func TestPagerRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 16); err == nil {
		t.Error("a non-database file was accepted")
	}
}

func TestPagerPinPreventsEviction(t *testing.T) {
	// A pool of three frames, so eviction is easy to force.
	p := newTestPager(t, 3)

	var ids []PageID
	for i := 0; i < 3; i++ {
		page, err := p.Allocate(PageTypeLeaf)
		if err != nil {
			t.Fatal(err)
		}
		page.Append([]byte(fmt.Sprintf("page-%d", i)))
		ids = append(ids, page.ID())
	}

	// All three are still pinned, so there is nowhere to put a fourth.
	if _, err := p.Allocate(PageTypeLeaf); err == nil {
		t.Error("allocated a page with every frame pinned")
	}

	// Release one and it works.
	if err := p.Unpin(ids[0], true); err != nil {
		t.Fatal(err)
	}
	fourth, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatalf("allocate after unpinning: %v", err)
	}
	p.Unpin(fourth.ID(), true)

	// The evicted page's contents must have been written, not dropped.
	p.Unpin(ids[1], true)
	p.Unpin(ids[2], true)
	page, err := p.Fetch(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := page.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "page-0" {
		t.Errorf("evicted page came back as %q", got)
	}
	p.Unpin(ids[0], false)

	if p.Stats().Evictions == 0 {
		t.Error("no eviction was recorded")
	}
}

// Working on far more pages than the pool holds is the whole point of having
// one, so it must stay correct throughout.
func TestPagerWorkingSetLargerThanPool(t *testing.T) {
	const (
		poolSize = 8
		numPages = 200
	)
	p := newTestPager(t, poolSize)

	ids := make([]PageID, 0, numPages)
	for i := 0; i < numPages; i++ {
		page, err := p.Allocate(PageTypeLeaf)
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if err := page.Append([]byte(fmt.Sprintf("value-%d", i))); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, page.ID())
		if err := p.Unpin(page.ID(), true); err != nil {
			t.Fatal(err)
		}
	}

	// Read them all back in a different order, so the cache cannot simply be
	// riding a sequential pattern.
	for i := numPages - 1; i >= 0; i-- {
		page, err := p.Fetch(ids[i])
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		got, err := page.Get(0)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		want := fmt.Sprintf("value-%d", i)
		if string(got) != want {
			t.Fatalf("page %d: got %q, want %q", i, got, want)
		}
		p.Unpin(ids[i], false)
	}

	stats := p.Stats()
	if stats.Evictions == 0 {
		t.Error("a 200-page working set in an 8-frame pool evicted nothing")
	}
	if stats.Reads == 0 {
		t.Error("nothing was ever read back from disk")
	}
}

// A freed page should be reused before the file grows.
func TestPagerFreeListReusesPages(t *testing.T) {
	p := newTestPager(t, 16)

	first, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	freedID := first.ID()
	p.Unpin(freedID, true)

	countBefore := p.PageCount()
	if err := p.Free(freedID); err != nil {
		t.Fatal(err)
	}

	reused, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID() != freedID {
		t.Errorf("allocated page %d, want the freed page %d", reused.ID(), freedID)
	}
	if p.PageCount() != countBefore {
		t.Errorf("the file grew from %d to %d despite a free page", countBefore, p.PageCount())
	}
	if reused.NumSlots() != 0 {
		t.Error("a reused page still holds its old contents")
	}
	p.Unpin(reused.ID(), true)
}

// The write-ahead rule: a dirty page must not reach disk before its log does.
func TestPagerFlushesLogBeforePage(t *testing.T) {
	p := newTestPager(t, 2)

	var loggedUpTo []uint64
	p.SetFlushLog(func(upTo uint64) error {
		loggedUpTo = append(loggedUpTo, upTo)
		return nil
	})

	page, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	page.SetLSN(99)
	page.Append([]byte("change"))
	id := page.ID()
	p.Unpin(id, true)

	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(loggedUpTo) == 0 {
		t.Fatal("the page was written without flushing the log first")
	}
	if loggedUpTo[0] != 99 {
		t.Errorf("log flushed up to %d, want the page's LSN 99", loggedUpTo[0])
	}
}

func TestPagerUnpinErrors(t *testing.T) {
	p := newTestPager(t, 4)

	if err := p.Unpin(1, false); err == nil {
		t.Error("unpinning a page that is not in the pool should fail")
	}

	page, err := p.Allocate(PageTypeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	id := page.ID()
	if err := p.Unpin(id, false); err != nil {
		t.Fatal(err)
	}
	// A second unpin is a bug in the caller and must be reported, not ignored:
	// silently allowing it would let a page be evicted while still in use.
	if err := p.Unpin(id, false); err == nil {
		t.Error("double unpin was accepted")
	}
}

func TestPagerCatalogRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	p, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	if p.CatalogRoot() != InvalidPageID {
		t.Error("a fresh database should have no catalog root")
	}
	if err := p.SetCatalogRoot(7); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p2, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if p2.CatalogRoot() != 7 {
		t.Errorf("catalog root did not persist: got %d", p2.CatalogRoot())
	}
}

func BenchmarkPageInsert(b *testing.B) {
	tuple := []byte("a representative row value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewPage(1, make([]byte, PageSize))
		p.Init(PageTypeLeaf)
		for p.Append(tuple) == nil {
		}
	}
}

func BenchmarkPagerFetchCached(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	p, err := Open(path, 128)
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()

	page, _ := p.Allocate(PageTypeLeaf)
	id := page.ID()
	p.Unpin(id, true)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Fetch(id); err != nil {
			b.Fatal(err)
		}
		p.Unpin(id, false)
	}
}
