package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The write-ahead log is what makes a crash survivable.
//
// The rule it enforces is one sentence: a change must be described in the log,
// on disk, before the page holding that change is written. Given that, a crash
// can leave the data file in any state at all and recovery can still repair it,
// because the log is guaranteed to know at least as much as the data file does.
//
// Recovery is physiological redo, the same approach ARIES takes: the log
// records the full after-image of each modified page, and replay writes those
// images back. Full-page images cost more space than a description of what
// changed, and buy something worth having -- replay becomes idempotent and
// order-independent within a page, so it does not matter how many times a
// record is applied or whether the page had already been written before the
// crash.
//
// Record format:
//
//	uint32 length | uint64 lsn | uint8 type | uint64 txnID | payload | uint32 crc
//
// The CRC covers everything before it, and a record whose CRC does not match is
// the crash tear: replay stops there rather than trying to interpret a
// half-written record.

// RecordType tags a log record.
type RecordType uint8

const (
	// RecordPageImage carries the full contents of one page after a change.
	RecordPageImage RecordType = 1
	// RecordCommit marks a transaction as durable.
	RecordCommit RecordType = 2
	// RecordAbort marks a transaction as rolled back.
	RecordAbort RecordType = 3
	// RecordCheckpoint marks a point where every dirty page had been written,
	// so replay can start there instead of at the beginning of time.
	RecordCheckpoint RecordType = 4
)

const walRecordHeaderSize = 4 + 8 + 1 + 8 // length, lsn, type, txn

// WAL is the write-ahead log.
type WAL struct {
	path string

	mu sync.Mutex
	f  *os.File
	// nextLSN is the sequence number the next record will get. It only ever
	// increases, which is what lets a page's LSN be compared against it.
	nextLSN uint64
	// flushedLSN is how far the log has actually reached disk.
	flushedLSN uint64
	// buf accumulates records so a burst of small changes costs one write.
	buf    []byte
	closed bool
	// syncOnCommit forces the log to disk before a commit is acknowledged.
	// Turning it off makes commits far faster and means a crash can lose the
	// last few of them -- the same trade every database exposes.
	syncOnCommit bool
}

// OpenWAL opens or creates the log beside a database file.
func OpenWAL(dbPath string, syncOnCommit bool) (*WAL, error) {
	path := dbPath + ".wal"
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open write-ahead log: %w", err)
	}
	return &WAL{path: path, f: f, nextLSN: 1, syncOnCommit: syncOnCommit}, nil
}

// Path returns the log file path.
func (w *WAL) Path() string { return w.path }

// append writes one record and returns its LSN.
func (w *WAL) append(t RecordType, txnID uint64, payload []byte) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("write-ahead log is closed")
	}

	lsn := w.nextLSN
	w.nextLSN++

	total := walRecordHeaderSize + len(payload) + 4
	rec := make([]byte, 0, total)
	rec = binary.LittleEndian.AppendUint32(rec, uint32(total))
	rec = binary.LittleEndian.AppendUint64(rec, lsn)
	rec = append(rec, byte(t))
	rec = binary.LittleEndian.AppendUint64(rec, txnID)
	rec = append(rec, payload...)
	rec = binary.LittleEndian.AppendUint32(rec, crc32.ChecksumIEEE(rec))

	w.buf = append(w.buf, rec...)
	return lsn, nil
}

// LogPage records the full image of a modified page.
func (w *WAL) LogPage(txnID uint64, page *Page) (uint64, error) {
	payload := make([]byte, 4+PageSize)
	binary.LittleEndian.PutUint32(payload, uint32(page.ID()))
	copy(payload[4:], page.Data())
	return w.append(RecordPageImage, txnID, payload)
}

// LogCommit records that a transaction committed, and makes it durable.
//
// This is the moment a transaction becomes real. Everything before it can be
// thrown away by a crash; everything up to it survives.
func (w *WAL) LogCommit(txnID uint64) (uint64, error) {
	lsn, err := w.append(RecordCommit, txnID, nil)
	if err != nil {
		return 0, err
	}
	if w.syncOnCommit {
		if err := w.Flush(lsn); err != nil {
			return 0, err
		}
	}
	return lsn, nil
}

// LogAbort records a rollback.
func (w *WAL) LogAbort(txnID uint64) (uint64, error) {
	return w.append(RecordAbort, txnID, nil)
}

// LogCheckpoint records that every dirty page has been written.
//
// Without checkpoints the log grows forever and recovery replays from the
// beginning of the database's life. A checkpoint says "the data file is
// current as of here", so everything before it can be discarded.
func (w *WAL) LogCheckpoint() (uint64, error) {
	lsn, err := w.append(RecordCheckpoint, 0, nil)
	if err != nil {
		return 0, err
	}
	return lsn, w.Flush(lsn)
}

// Flush pushes the log to disk up to at least upTo.
//
// This is the hook the pager calls before writing a dirty page, and it is
// where the write-ahead rule is actually enforced.
func (w *WAL) Flush(upTo uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if upTo != 0 && w.flushedLSN >= upTo {
		return nil // already durable
	}
	return w.flushLocked()
}

func (w *WAL) flushLocked() error {
	if len(w.buf) > 0 {
		if _, err := w.f.Write(w.buf); err != nil {
			return fmt.Errorf("write to the log: %w", err)
		}
		w.buf = w.buf[:0]
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("sync the log: %w", err)
	}
	w.flushedLSN = w.nextLSN - 1
	return nil
}

// Truncate empties the log, which is safe only after a checkpoint has written
// every dirty page.
//
// The handle is closed and reopened rather than truncated in place. Windows
// refuses to truncate a file opened for append -- the append mode fixes the
// write position, and shrinking the file underneath it is not allowed -- so
// the portable move is to let go of the handle first.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}

	w.buf = w.buf[:0]
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Truncate(w.path, 0); err != nil {
		return err
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("reopen the log after truncating: %w", err)
	}
	w.f = f
	w.flushedLSN = w.nextLSN - 1
	return nil
}

// Size returns the log's size on disk.
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.f.Stat()
	if err != nil {
		return 0
	}
	return info.Size() + int64(len(w.buf))
}

// Close flushes and closes the log.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.flushLocked(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// --- recovery ---

// RecoveryStats reports what recovery did, for the startup log.
type RecoveryStats struct {
	RecordsScanned int
	PagesRestored  int
	Committed      int
	Aborted        int
	TornTail       bool
}

// Recover replays the log into the data file.
//
// It is two passes. The first reads the whole log and notes which transactions
// committed; the second replays the page images belonging to those, and only
// those. A single pass cannot work, because a transaction's changes appear in
// the log before its commit record does -- so at the moment a page image is
// read, whether to apply it is not yet known.
//
// Uncommitted changes need no undo here, because a page image is only ever
// logged for a page whose transaction went on to commit or abort, and an
// aborted transaction's images are simply never replayed. That is only sound
// because this database applies a transaction's writes to the buffer pool and
// discards them on rollback rather than writing them through; a design that
// wrote dirty pages of in-flight transactions would need real undo.
func Recover(dbPath string, syncOnCommit bool) (*RecoveryStats, error) {
	walPath := dbPath + ".wal"
	f, err := os.Open(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &RecoveryStats{}, nil
		}
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &RecoveryStats{}, nil
	}

	stats := &RecoveryStats{}

	// Pass one: which transactions committed, and where does the log end?
	committed := make(map[uint64]bool)
	aborted := make(map[uint64]bool)
	checkpointAt := -1

	pos := 0
	for pos < len(data) {
		rec, next, ok := readRecord(data, pos)
		if !ok {
			// A record that does not checksum is the crash tear. Everything
			// after it is unreadable by definition, so scanning stops.
			stats.TornTail = true
			break
		}
		stats.RecordsScanned++
		switch rec.typ {
		case RecordCommit:
			committed[rec.txnID] = true
		case RecordAbort:
			aborted[rec.txnID] = true
		case RecordCheckpoint:
			checkpointAt = pos
		}
		pos = next
	}
	stats.Committed = len(committed)
	stats.Aborted = len(aborted)
	logEnd := pos

	// Pass two: replay the committed page images, starting from the last
	// checkpoint since everything before it is already in the data file.
	db, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	start := 0
	if checkpointAt >= 0 {
		start = checkpointAt
	}
	pos = start
	for pos < logEnd {
		rec, next, ok := readRecord(data, pos)
		if !ok {
			break
		}
		if rec.typ == RecordPageImage && committed[rec.txnID] {
			if len(rec.payload) != 4+PageSize {
				return nil, fmt.Errorf("corrupt page image at lsn %d", rec.lsn)
			}
			pageID := binary.LittleEndian.Uint32(rec.payload)
			if _, err := db.WriteAt(rec.payload[4:], int64(pageID)*PageSize); err != nil {
				return nil, fmt.Errorf("restore page %d: %w", pageID, err)
			}
			stats.PagesRestored++
		}
		pos = next
	}

	if err := db.Sync(); err != nil {
		return nil, err
	}

	// The log has been folded into the data file, so it can start again. A
	// torn tail is discarded with it: those records described a transaction
	// that never committed.
	if err := os.Truncate(walPath, 0); err != nil {
		return nil, err
	}
	return stats, nil
}

// walRecord is one decoded log record.
type walRecord struct {
	lsn     uint64
	typ     RecordType
	txnID   uint64
	payload []byte
}

// readRecord decodes the record at pos, reporting false if it is damaged or
// truncated.
func readRecord(data []byte, pos int) (walRecord, int, bool) {
	if pos+4 > len(data) {
		return walRecord{}, 0, false
	}
	total := int(binary.LittleEndian.Uint32(data[pos:]))
	if total < walRecordHeaderSize+4 || pos+total > len(data) {
		return walRecord{}, 0, false
	}

	raw := data[pos : pos+total]
	want := binary.LittleEndian.Uint32(raw[total-4:])
	if crc32.ChecksumIEEE(raw[:total-4]) != want {
		return walRecord{}, 0, false
	}

	rec := walRecord{
		lsn:     binary.LittleEndian.Uint64(raw[4:]),
		typ:     RecordType(raw[12]),
		txnID:   binary.LittleEndian.Uint64(raw[13:]),
		payload: raw[walRecordHeaderSize : total-4],
	}
	return rec, pos + total, true
}
