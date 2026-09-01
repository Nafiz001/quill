package storage

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

// The buffer pool is what lets a database work on files larger than memory.
//
// It holds a fixed number of page-sized frames and decides which pages get to
// live in them. Three rules make it correct:
//
//   - A pinned page is in use by somebody and must not be evicted. Pins nest,
//     so a caller that fetches a page must release it exactly once.
//   - A dirty page must be written before its frame is reused, or the change
//     is lost.
//   - A dirty page must not reach disk before the log records describing it.
//     That is write-ahead logging, and it is the reason recovery can work at
//     all: the log always knows at least as much as the data file does.
//
// Eviction is LRU, over the unpinned pages only.

// File header layout, page 0. It is deliberately tiny and fixed.
const (
	magicString    = "QUILLDB1"
	offMagic       = 0
	offVersion     = 8  // uint32
	offPageCount   = 12 // uint32, pages allocated so far
	offFreeList    = 16 // uint32, head of the free page chain
	offCatalogRoot = 20 // uint32, root page of the catalog B+tree
	fileVersion    = 1
)

// DefaultPoolSize is how many frames the pool keeps.
//
// 1024 frames is 4MB, which is small enough to prove the eviction paths get
// exercised in testing and large enough that a realistic working set fits.
// A production value would be a large fraction of machine memory.
const DefaultPoolSize = 1024

// frame is one slot in the pool.
type frame struct {
	page  *Page
	pins  int
	dirty bool
	// element is this frame's position in the LRU list, or nil when pinned.
	element *list.Element
}

// Pager owns the database file and the pool of pages cached from it.
type Pager struct {
	file     *os.File
	path     string
	poolSize int

	mu     sync.Mutex
	frames map[PageID]*frame
	// lru holds unpinned frames, least recently used at the front.
	lru *list.List

	pageCount    uint32
	freeListHead PageID
	catalogRoot  PageID

	// flushLog is called before a dirty page is written, so the log for that
	// page reaches disk first. Set by the WAL; nil means no logging.
	flushLog func(upTo uint64) error

	stats PagerStats
}

// PagerStats counts pool activity, for EXPLAIN and for tests that need to
// prove a page really came from disk.
type PagerStats struct {
	Hits      int64
	Misses    int64
	Reads     int64
	Writes    int64
	Evictions int64
}

// Open opens or creates a database file.
func Open(path string, poolSize int) (*Pager, error) {
	if poolSize <= 0 {
		poolSize = DefaultPoolSize
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}

	p := &Pager{
		file:     f,
		path:     path,
		poolSize: poolSize,
		frames:   make(map[PageID]*frame, poolSize),
		lru:      list.New(),
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() == 0 {
		if err := p.initNewFile(); err != nil {
			f.Close()
			return nil, err
		}
	} else if err := p.readFileHeader(); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

// initNewFile writes page 0 of a fresh database.
func (p *Pager) initNewFile() error {
	header := make([]byte, PageSize)
	copy(header[offMagic:], magicString)
	binary.LittleEndian.PutUint32(header[offVersion:], fileVersion)
	// Page 0 is the header itself, so allocation starts at 1.
	binary.LittleEndian.PutUint32(header[offPageCount:], 1)
	binary.LittleEndian.PutUint32(header[offFreeList:], uint32(InvalidPageID))
	binary.LittleEndian.PutUint32(header[offCatalogRoot:], uint32(InvalidPageID))

	if _, err := p.file.WriteAt(header, 0); err != nil {
		return err
	}
	p.pageCount = 1
	p.freeListHead = InvalidPageID
	p.catalogRoot = InvalidPageID
	return p.file.Sync()
}

func (p *Pager) readFileHeader() error {
	header := make([]byte, PageSize)
	if _, err := p.file.ReadAt(header, 0); err != nil && err != io.EOF {
		return fmt.Errorf("read file header: %w", err)
	}
	if string(header[offMagic:offMagic+len(magicString)]) != magicString {
		return fmt.Errorf("not a quill database file")
	}
	if v := binary.LittleEndian.Uint32(header[offVersion:]); v != fileVersion {
		return fmt.Errorf("unsupported file version %d", v)
	}
	p.pageCount = binary.LittleEndian.Uint32(header[offPageCount:])
	p.freeListHead = PageID(binary.LittleEndian.Uint32(header[offFreeList:]))
	p.catalogRoot = PageID(binary.LittleEndian.Uint32(header[offCatalogRoot:]))
	return nil
}

// writeFileHeader persists the allocation bookkeeping.
func (p *Pager) writeFileHeader() error {
	header := make([]byte, PageSize)
	if _, err := p.file.ReadAt(header, 0); err != nil && err != io.EOF {
		return err
	}
	copy(header[offMagic:], magicString)
	binary.LittleEndian.PutUint32(header[offVersion:], fileVersion)
	binary.LittleEndian.PutUint32(header[offPageCount:], p.pageCount)
	binary.LittleEndian.PutUint32(header[offFreeList:], uint32(p.freeListHead))
	binary.LittleEndian.PutUint32(header[offCatalogRoot:], uint32(p.catalogRoot))
	_, err := p.file.WriteAt(header, 0)
	return err
}

// SetFlushLog installs the write-ahead hook the WAL uses.
func (p *Pager) SetFlushLog(fn func(upTo uint64) error) {
	p.mu.Lock()
	p.flushLog = fn
	p.mu.Unlock()
}

// CatalogRoot returns the root page of the catalog, or InvalidPageID when the
// database has no tables yet.
func (p *Pager) CatalogRoot() PageID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalogRoot
}

func (p *Pager) SetCatalogRoot(id PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalogRoot = id
	return p.writeFileHeader()
}

// Fetch loads a page and pins it. The caller must call Unpin exactly once.
func (p *Pager) Fetch(id PageID) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetchLocked(id)
}

func (p *Pager) fetchLocked(id PageID) (*Page, error) {
	if id == InvalidPageID || uint32(id) >= p.pageCount {
		return nil, fmt.Errorf("page %d is out of range (%d allocated)", id, p.pageCount)
	}

	if f, ok := p.frames[id]; ok {
		p.stats.Hits++
		p.pinLocked(f)
		return f.page, nil
	}
	p.stats.Misses++

	if len(p.frames) >= p.poolSize {
		if err := p.evictLocked(); err != nil {
			return nil, err
		}
	}

	data := make([]byte, PageSize)
	if _, err := p.file.ReadAt(data, int64(id)*PageSize); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read page %d: %w", id, err)
	}
	p.stats.Reads++

	f := &frame{page: NewPage(id, data), pins: 1}
	p.frames[id] = f
	return f.page, nil
}

// pinLocked takes a reference, removing the frame from the eviction list.
func (p *Pager) pinLocked(f *frame) {
	f.pins++
	if f.element != nil {
		p.lru.Remove(f.element)
		f.element = nil
	}
}

// Unpin releases a reference. dirty marks the page as modified.
//
// The dirty flag is sticky: once any caller reports a change, the page stays
// dirty until it is written, even if later callers pass false.
func (p *Pager) Unpin(id PageID, dirty bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	f, ok := p.frames[id]
	if !ok {
		return fmt.Errorf("unpin of page %d, which is not in the pool", id)
	}
	if dirty {
		f.dirty = true
	}
	if f.pins <= 0 {
		return fmt.Errorf("unpin of page %d, which is not pinned", id)
	}
	f.pins--
	if f.pins == 0 {
		// Newly unpinned pages go to the back of the LRU list as the most
		// recently used.
		f.element = p.lru.PushBack(id)
	}
	return nil
}

// evictLocked frees one frame, writing it back first if it is dirty.
func (p *Pager) evictLocked() error {
	elem := p.lru.Front()
	if elem == nil {
		// Every frame is pinned. This is a real resource exhaustion, not a
		// transient condition, so it is reported rather than waited on: a
		// query that needs more pages at once than the pool holds will never
		// succeed by retrying.
		return fmt.Errorf("buffer pool exhausted: all %d frames are pinned", p.poolSize)
	}
	id := elem.Value.(PageID)
	f := p.frames[id]

	if f.dirty {
		if err := p.writePageLocked(f); err != nil {
			return err
		}
	}
	p.lru.Remove(elem)
	delete(p.frames, id)
	p.stats.Evictions++
	return nil
}

// writePageLocked flushes one frame to disk, honouring the write-ahead rule.
func (p *Pager) writePageLocked(f *frame) error {
	// The log records describing this page must be durable before the page
	// itself. Otherwise a crash could leave a data file that reflects a change
	// the log has no record of, and recovery would have no way to undo it.
	if p.flushLog != nil {
		if err := p.flushLog(f.page.LSN()); err != nil {
			return fmt.Errorf("flush log before writing page %d: %w", f.page.ID(), err)
		}
	}
	if _, err := p.file.WriteAt(f.page.Data(), int64(f.page.ID())*PageSize); err != nil {
		return fmt.Errorf("write page %d: %w", f.page.ID(), err)
	}
	f.dirty = false
	p.stats.Writes++
	return nil
}

// Allocate returns a new page, pinned and initialised.
//
// Freed pages are reused before the file grows, which keeps a table that is
// repeatedly filled and emptied from expanding without bound.
func (p *Pager) Allocate(t PageType) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.freeListHead != InvalidPageID {
		id := p.freeListHead
		page, err := p.fetchLocked(id)
		if err != nil {
			return nil, err
		}
		// The free list threads through the pages themselves, so no separate
		// structure has to be kept in sync.
		p.freeListHead = page.Next()
		page.Init(t)
		if err := p.writeFileHeader(); err != nil {
			return nil, err
		}
		p.frames[id].dirty = true
		return page, nil
	}

	id := PageID(p.pageCount)
	p.pageCount++
	if err := p.writeFileHeader(); err != nil {
		return nil, err
	}

	if len(p.frames) >= p.poolSize {
		if err := p.evictLocked(); err != nil {
			return nil, err
		}
	}
	data := make([]byte, PageSize)
	page := NewPage(id, data)
	page.Init(t)
	p.frames[id] = &frame{page: page, pins: 1, dirty: true}
	return page, nil
}

// Free returns a page to the free list. The caller must have unpinned it.
func (p *Pager) Free(id PageID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	page, err := p.fetchLocked(id)
	if err != nil {
		return err
	}
	page.Init(PageTypeFree)
	page.SetNext(p.freeListHead)
	p.freeListHead = id
	p.frames[id].dirty = true

	f := p.frames[id]
	f.pins--
	if f.pins == 0 {
		f.element = p.lru.PushBack(id)
	}
	return p.writeFileHeader()
}

// Flush writes every dirty page and syncs the file.
func (p *Pager) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushLocked()
}

func (p *Pager) flushLocked() error {
	for _, f := range p.frames {
		if f.dirty {
			if err := p.writePageLocked(f); err != nil {
				return err
			}
		}
	}
	if err := p.writeFileHeader(); err != nil {
		return err
	}
	return p.file.Sync()
}

// PageCount returns how many pages the file holds, header included.
func (p *Pager) PageCount() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pageCount
}

// Stats returns a snapshot of pool activity.
func (p *Pager) Stats() PagerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Path returns the database file path.
func (p *Pager) Path() string { return p.path }

// Close flushes everything and releases the file.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.flushLocked(); err != nil {
		p.file.Close()
		return err
	}
	return p.file.Close()
}

// ForEachDirty calls fn for every modified page still in the pool.
//
// Commit uses it to write a log image of each change before the commit record
// goes down. fn must not call back into the pager.
func (p *Pager) ForEachDirty(fn func(*Page) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, f := range p.frames {
		if !f.dirty {
			continue
		}
		if err := fn(f.page); err != nil {
			return err
		}
	}
	return nil
}

// DiscardDirty throws away every unwritten change and reloads those pages from
// disk.
//
// This is how a rollback works without undo records: nothing uncommitted was
// ever written to the data file, so forgetting the in-memory copy is enough to
// erase the transaction. A pinned page cannot be discarded, and its presence
// means a statement is still running -- which would be a bug in the caller, so
// it is reported rather than skipped.
func (p *Pager) DiscardDirty() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, f := range p.frames {
		if !f.dirty {
			continue
		}
		if f.pins > 0 {
			return fmt.Errorf("cannot roll back: page %d is still pinned", id)
		}
		if f.element != nil {
			p.lru.Remove(f.element)
		}
		delete(p.frames, id)
	}
	// The header holds allocation bookkeeping that a rolled-back transaction
	// may have advanced. Re-reading it puts the free list and page count back
	// to what is actually on disk.
	return p.readFileHeader()
}

// Abandon closes the file without writing anything still buffered.
//
// It exists to simulate a crash in tests: the data file is left exactly as it
// was at the last real write, with every dirty page lost, which is what a power
// cut produces. Calling it in ordinary operation would discard committed data,
// so it is deliberately not part of the normal lifecycle.
func (p *Pager) Abandon() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.frames = make(map[PageID]*frame)
	p.lru.Init()
	return p.file.Close()
}
