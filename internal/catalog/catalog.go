// Package catalog holds the database's schema: what tables exist, what
// columns they have, and which pages their B+trees start on.
//
// The catalog is itself stored in a B+tree, keyed by table name. That is not
// just tidiness -- it means schema changes go through the same pages, the same
// buffer pool and eventually the same write-ahead log as ordinary data, so a
// CREATE TABLE is as crash-safe as an INSERT without needing a second
// mechanism to make it so. PostgreSQL and SQLite both do the same thing, and
// SQLite's is even queryable as `sqlite_master`.
package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Nafiz001/quill/internal/storage"
	"github.com/Nafiz001/quill/internal/types"
)

// Column is one column definition.
type Column struct {
	Name       string     `json:"name"`
	Type       types.Type `json:"type"`
	NotNull    bool       `json:"not_null,omitempty"`
	PrimaryKey bool       `json:"primary_key,omitempty"`
	Unique     bool       `json:"unique,omitempty"`
}

// Index is a secondary index over one or more columns.
type Index struct {
	Name string `json:"name"`
	// Columns are positions into the table's column list.
	Columns  []int          `json:"columns"`
	Unique   bool           `json:"unique,omitempty"`
	RootPage storage.PageID `json:"root_page"`
}

// Table is one table's schema and storage location.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	// RootPage is where the table's primary B+tree starts.
	RootPage storage.PageID `json:"root_page"`
	// NextRowID is the counter behind implicit row ids. Rows are keyed by it
	// when no primary key is declared, which keeps every table a B+tree
	// regardless of what the user asked for.
	NextRowID int64   `json:"next_row_id"`
	Indexes   []Index `json:"indexes,omitempty"`
}

// ColumnIndex returns a column's position, or -1.
//
// Column names are matched case-insensitively, because unquoted identifiers in
// SQL are case-insensitive and users type `SELECT ID` for a column declared
// as `id`.
func (t *Table) ColumnIndex(name string) int {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			return i
		}
	}
	return -1
}

// Column returns a column by name.
func (t *Table) Column(name string) (*Column, int) {
	if i := t.ColumnIndex(name); i >= 0 {
		return &t.Columns[i], i
	}
	return nil, -1
}

// PrimaryKeyIndex returns the position of the primary key column, or -1 when
// the table has none and rows are keyed by an implicit row id.
func (t *Table) PrimaryKeyIndex() int {
	for i := range t.Columns {
		if t.Columns[i].PrimaryKey {
			return i
		}
	}
	return -1
}

// Catalog is the set of tables in a database.
type Catalog struct {
	pager *storage.Pager

	mu sync.RWMutex
	// tables is an in-memory cache of the whole catalog. A database has tens
	// of tables, not millions, and every query needs the schema, so it is read
	// once at startup and kept.
	tables map[string]*Table
	tree   *storage.BTree
}

// Open loads the catalog, creating it if the database is new.
func Open(pager *storage.Pager) (*Catalog, error) {
	c := &Catalog{pager: pager, tables: make(map[string]*Table)}

	root := pager.CatalogRoot()
	if root == storage.InvalidPageID {
		tree, err := storage.CreateBTree(pager, pager.SetCatalogRoot)
		if err != nil {
			return nil, fmt.Errorf("create catalog: %w", err)
		}
		c.tree = tree
		if err := pager.SetCatalogRoot(tree.Root()); err != nil {
			return nil, err
		}
		return c, nil
	}

	c.tree = storage.NewBTree(pager, root, pager.SetCatalogRoot)
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// load reads every table definition into the cache.
func (c *Catalog) load() error {
	cursor, err := c.tree.Scan(nil, nil)
	if err != nil {
		return err
	}
	for cursor.Valid() {
		var t Table
		if err := json.Unmarshal(cursor.Value(), &t); err != nil {
			return fmt.Errorf("corrupt catalog entry %q: %w", cursor.Key(), err)
		}
		c.tables[strings.ToLower(t.Name)] = &t
		cursor.Next()
	}
	return cursor.Err()
}

// persist writes one table definition back to the catalog tree.
//
// JSON, deliberately. The catalog is written on DDL and read at startup, so
// it is nowhere near a hot path, and a schema you can read with `strings` on
// the data file is worth more during debugging than the bytes it saves.
func (c *Catalog) persist(t *Table) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return c.tree.Put([]byte(strings.ToLower(t.Name)), data)
}

// CreateTable adds a table, giving it its own B+tree.
func (c *Catalog) CreateTable(name string, columns []Column) (*Table, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	lower := strings.ToLower(name)
	if _, exists := c.tables[lower]; exists {
		return nil, fmt.Errorf("table %q already exists", name)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q has no columns", name)
	}

	seen := make(map[string]bool, len(columns))
	primaryKeys := 0
	for _, col := range columns {
		key := strings.ToLower(col.Name)
		if seen[key] {
			return nil, fmt.Errorf("column %q is declared twice", col.Name)
		}
		seen[key] = true
		if col.PrimaryKey {
			primaryKeys++
		}
	}
	if primaryKeys > 1 {
		return nil, fmt.Errorf("table %q declares %d primary keys", name, primaryKeys)
	}

	tree, err := storage.CreateBTree(c.pager, nil)
	if err != nil {
		return nil, err
	}

	t := &Table{
		Name:      name,
		Columns:   columns,
		RootPage:  tree.Root(),
		NextRowID: 1,
	}
	if err := c.persist(t); err != nil {
		return nil, err
	}
	c.tables[lower] = t
	return t, nil
}

// DropTable removes a table's definition.
//
// The pages its tree occupied are not reclaimed yet, so dropping and recreating
// a large table leaks space in the file until a compaction exists. That is a
// real cost and is recorded rather than glossed over.
func (c *Catalog) DropTable(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	lower := strings.ToLower(name)
	if _, exists := c.tables[lower]; !exists {
		return fmt.Errorf("table %q does not exist", name)
	}
	if _, err := c.tree.Delete([]byte(lower)); err != nil {
		return err
	}
	delete(c.tables, lower)
	return nil
}

// Table looks up a table by name.
func (c *Catalog) Table(name string) (*Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[strings.ToLower(name)]
	return t, ok
}

// Tables lists every table, in name order.
func (c *Catalog) Tables() []*Table {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Save writes a modified table definition back.
//
// It is called after a row id is handed out or an index is created, both of
// which change the definition rather than the data.
func (c *Catalog) Save(t *Table) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persist(t)
}

// NextRowID hands out the next implicit row id and persists the counter.
//
// Persisting on every insert is the safe end of a real trade: it costs a
// catalog write per row, but a counter that only lives in memory would restart
// after a crash and hand out ids that already exist. Batching the allocation
// -- reserving a block of ids at a time -- is the usual fix and is noted as
// future work.
func (c *Catalog) NextRowID(t *Table) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := t.NextRowID
	t.NextRowID++
	if err := c.persist(t); err != nil {
		t.NextRowID--
		return 0, err
	}
	return id, nil
}

// CreateIndex adds a secondary index to a table.
func (c *Catalog) CreateIndex(tableName, indexName string, columns []int, unique bool) (*Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	t, ok := c.tables[strings.ToLower(tableName)]
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", tableName)
	}
	for _, existing := range t.Indexes {
		if strings.EqualFold(existing.Name, indexName) {
			return nil, fmt.Errorf("index %q already exists", indexName)
		}
	}
	for _, col := range columns {
		if col < 0 || col >= len(t.Columns) {
			return nil, fmt.Errorf("index %q references a column that does not exist", indexName)
		}
	}

	tree, err := storage.CreateBTree(c.pager, nil)
	if err != nil {
		return nil, err
	}
	idx := Index{Name: indexName, Columns: columns, Unique: unique, RootPage: tree.Root()}
	t.Indexes = append(t.Indexes, idx)
	if err := c.persist(t); err != nil {
		t.Indexes = t.Indexes[:len(t.Indexes)-1]
		return nil, err
	}
	return &t.Indexes[len(t.Indexes)-1], nil
}

// TableTree opens the B+tree holding a table's rows.
//
// The root-change callback matters: when the tree grows a level the root moves,
// and the catalog entry has to follow or the table becomes unreachable on the
// next open.
func (c *Catalog) TableTree(t *Table) *storage.BTree {
	return storage.NewBTree(c.pager, t.RootPage, func(newRoot storage.PageID) error {
		c.mu.Lock()
		t.RootPage = newRoot
		err := c.persist(t)
		c.mu.Unlock()
		return err
	})
}

// IndexTree opens the B+tree behind a secondary index.
func (c *Catalog) IndexTree(t *Table, idx *Index) *storage.BTree {
	return storage.NewBTree(c.pager, idx.RootPage, func(newRoot storage.PageID) error {
		c.mu.Lock()
		idx.RootPage = newRoot
		err := c.persist(t)
		c.mu.Unlock()
		return err
	})
}
