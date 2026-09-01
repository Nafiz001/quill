package storage

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

func newTestTree(t *testing.T) (*BTree, *Pager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tree.db")
	pager, err := Open(path, 64)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pager.Close() })

	tree, err := CreateBTree(pager, nil)
	if err != nil {
		t.Fatalf("create tree: %v", err)
	}
	return tree, pager
}

// key formats an integer so byte order matches numeric order, which is what
// lets the tree's byte comparisons mean anything about the values.
func key(n int) []byte { return []byte(fmt.Sprintf("key%08d", n)) }

func TestBTreeEmptyGet(t *testing.T) {
	tree, _ := newTestTree(t)

	value, found, err := tree.Get(key(1))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Errorf("an empty tree returned %q", value)
	}
}

func TestBTreePutGet(t *testing.T) {
	tree, _ := newTestTree(t)

	pairs := map[string]string{
		"apple":  "red",
		"banana": "yellow",
		"cherry": "dark red",
		"date":   "brown",
		"":       "empty key",
	}
	for k, v := range pairs {
		if err := tree.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	for k, want := range pairs {
		got, found, err := tree.Get([]byte(k))
		if err != nil {
			t.Fatalf("get %q: %v", k, err)
		}
		if !found {
			t.Errorf("key %q is missing", k)
			continue
		}
		if string(got) != want {
			t.Errorf("key %q: got %q, want %q", k, got, want)
		}
	}

	if _, found, _ := tree.Get([]byte("nosuchkey")); found {
		t.Error("found a key that was never inserted")
	}
	if err := tree.Verify(); err != nil {
		t.Errorf("tree invariants broken: %v", err)
	}
}

func TestBTreeOverwrite(t *testing.T) {
	tree, _ := newTestTree(t)

	if err := tree.Put([]byte("k"), []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := tree.Put([]byte("k"), []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, found, err := tree.Get([]byte("k"))
	if err != nil || !found {
		t.Fatalf("get after overwrite: %v %v", found, err)
	}
	if string(got) != "second" {
		t.Errorf("got %q, want second", got)
	}

	// A longer replacement has to move within the page.
	if err := tree.Put([]byte("k"), []byte("a considerably longer value")); err != nil {
		t.Fatal(err)
	}
	got, _, _ = tree.Get([]byte("k"))
	if string(got) != "a considerably longer value" {
		t.Errorf("after growing: got %q", got)
	}
}

// Enough rows to force splits, which is where the interesting code is.
func TestBTreeSplitsAndStaysCorrect(t *testing.T) {
	tree, _ := newTestTree(t)

	const n = 5000
	for i := 0; i < n; i++ {
		if err := tree.Put(key(i), []byte(fmt.Sprintf("value-%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	height, err := tree.Height()
	if err != nil {
		t.Fatal(err)
	}
	if height < 2 {
		t.Errorf("%d rows produced a tree of height %d; it never split", n, height)
	}
	t.Logf("%d rows -> height %d", n, height)

	if err := tree.Verify(); err != nil {
		t.Fatalf("tree invariants broken after splits: %v", err)
	}

	for i := 0; i < n; i++ {
		got, found, err := tree.Get(key(i))
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if !found {
			t.Fatalf("key %d went missing after splitting", i)
		}
		want := fmt.Sprintf("value-%d", i)
		if string(got) != want {
			t.Fatalf("key %d: got %q, want %q", i, got, want)
		}
	}
}

// Descending insertion is the adversarial order for a naive split, because
// every new key lands at the front.
func TestBTreeDescendingInsertion(t *testing.T) {
	tree, _ := newTestTree(t)

	const n = 3000
	for i := n - 1; i >= 0; i-- {
		if err := tree.Put(key(i), []byte("v")); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := tree.Verify(); err != nil {
		t.Fatalf("invariants broken after descending insertion: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, found, _ := tree.Get(key(i)); !found {
			t.Fatalf("key %d is missing", i)
		}
	}
}

func TestBTreeRandomInsertion(t *testing.T) {
	tree, _ := newTestTree(t)
	rng := rand.New(rand.NewSource(42))

	const n = 4000
	order := rng.Perm(n)
	for _, i := range order {
		if err := tree.Put(key(i), []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := tree.Verify(); err != nil {
		t.Fatalf("invariants broken after random insertion: %v", err)
	}
	for i := 0; i < n; i++ {
		got, found, err := tree.Get(key(i))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("key %d is missing", i)
		}
		if want := fmt.Sprintf("v%d", i); string(got) != want {
			t.Fatalf("key %d: got %q, want %q", i, got, want)
		}
	}
}

// Variable-length values exercise the page's space accounting, since entries
// no longer pack uniformly.
func TestBTreeVariableLengthValues(t *testing.T) {
	tree, _ := newTestTree(t)

	const n = 1500
	for i := 0; i < n; i++ {
		value := bytes.Repeat([]byte("x"), i%400)
		if err := tree.Put(key(i), value); err != nil {
			t.Fatalf("put %d (%d bytes): %v", i, len(value), err)
		}
	}
	if err := tree.Verify(); err != nil {
		t.Fatalf("invariants broken: %v", err)
	}
	for i := 0; i < n; i++ {
		got, found, err := tree.Get(key(i))
		if err != nil || !found {
			t.Fatalf("key %d: found=%v err=%v", i, found, err)
		}
		if len(got) != i%400 {
			t.Fatalf("key %d: got %d bytes, want %d", i, len(got), i%400)
		}
	}
}

func TestBTreeScan(t *testing.T) {
	tree, _ := newTestTree(t)

	const n = 2000
	for i := 0; i < n; i++ {
		if err := tree.Put(key(i), []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	// A full scan must return everything, in order, exactly once.
	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for cursor.Valid() {
		seen = append(seen, string(cursor.Key()))
		cursor.Next()
	}
	if cursor.Err() != nil {
		t.Fatal(cursor.Err())
	}
	if len(seen) != n {
		t.Fatalf("full scan returned %d rows, want %d", len(seen), n)
	}
	if !sort.StringsAreSorted(seen) {
		t.Error("scan did not return keys in order")
	}
	for i := 0; i < n; i++ {
		if seen[i] != string(key(i)) {
			t.Fatalf("position %d: got %q, want %q", i, seen[i], key(i))
		}
	}
}

func TestBTreeRangeScan(t *testing.T) {
	tree, _ := newTestTree(t)
	for i := 0; i < 1000; i++ {
		if err := tree.Put(key(i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		start, end []byte
		wantFirst  string
		wantLast   string
		wantCount  int
	}{
		{"bounded", key(100), key(200), string(key(100)), string(key(199)), 100},
		{"open start", nil, key(50), string(key(0)), string(key(49)), 50},
		{"open end", key(950), nil, string(key(950)), string(key(999)), 50},
		{"single row", key(500), key(501), string(key(500)), string(key(500)), 1},
		{"empty range", key(500), key(500), "", "", 0},
		{"past the end", key(5000), nil, "", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cursor, err := tree.Scan(tc.start, tc.end)
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for cursor.Valid() {
				keys = append(keys, string(cursor.Key()))
				cursor.Next()
			}
			if len(keys) != tc.wantCount {
				t.Fatalf("got %d rows, want %d", len(keys), tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			if keys[0] != tc.wantFirst {
				t.Errorf("first: got %q, want %q", keys[0], tc.wantFirst)
			}
			if keys[len(keys)-1] != tc.wantLast {
				t.Errorf("last: got %q, want %q", keys[len(keys)-1], tc.wantLast)
			}
		})
	}
}

func TestBTreeDelete(t *testing.T) {
	tree, _ := newTestTree(t)

	const n = 2000
	for i := 0; i < n; i++ {
		if err := tree.Put(key(i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}

	// Remove every third key.
	deleted := map[int]bool{}
	for i := 0; i < n; i += 3 {
		ok, err := tree.Delete(key(i))
		if err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("delete %d reported the key was absent", i)
		}
		deleted[i] = true
	}

	if err := tree.Verify(); err != nil {
		t.Fatalf("invariants broken after deletion: %v", err)
	}

	for i := 0; i < n; i++ {
		_, found, err := tree.Get(key(i))
		if err != nil {
			t.Fatal(err)
		}
		if found == deleted[i] {
			t.Fatalf("key %d: found=%v but deleted=%v", i, found, deleted[i])
		}
	}

	// Deleting something absent is not an error, just a false.
	if ok, err := tree.Delete([]byte("nosuchkey")); err != nil || ok {
		t.Errorf("deleting a missing key: ok=%v err=%v", ok, err)
	}

	// A scan must skip the holes.
	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for cursor.Valid() {
		count++
		cursor.Next()
	}
	if want := n - len(deleted); count != want {
		t.Errorf("scan after deletion returned %d rows, want %d", count, want)
	}
}

// Interleaved writes and deletes are what a real workload looks like, and the
// order is where structural bugs surface.
func TestBTreeMixedWorkload(t *testing.T) {
	tree, _ := newTestTree(t)
	rng := rand.New(rand.NewSource(7))
	model := map[string]string{}

	for step := 0; step < 8000; step++ {
		k := key(rng.Intn(1500))
		switch {
		case rng.Float64() < 0.3 && len(model) > 0:
			if _, err := tree.Delete(k); err != nil {
				t.Fatalf("step %d delete: %v", step, err)
			}
			delete(model, string(k))
		default:
			v := fmt.Sprintf("v%d", step)
			if err := tree.Put(k, []byte(v)); err != nil {
				t.Fatalf("step %d put: %v", step, err)
			}
			model[string(k)] = v
		}

		if step%2000 == 0 {
			if err := tree.Verify(); err != nil {
				t.Fatalf("step %d: invariants broken: %v", step, err)
			}
		}
	}

	if err := tree.Verify(); err != nil {
		t.Fatalf("final invariants broken: %v", err)
	}

	// The tree must agree with a plain map on every key.
	for k, want := range model {
		got, found, err := tree.Get([]byte(k))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("key %q is missing from the tree", k)
		}
		if string(got) != want {
			t.Fatalf("key %q: tree has %q, model has %q", k, got, want)
		}
	}

	// And a scan must return exactly the model's keys, sorted.
	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var scanned []string
	for cursor.Valid() {
		scanned = append(scanned, string(cursor.Key()))
		cursor.Next()
	}
	if len(scanned) != len(model) {
		t.Fatalf("scan returned %d rows, model has %d", len(scanned), len(model))
	}
	want := make([]string, 0, len(model))
	for k := range model {
		want = append(want, k)
	}
	sort.Strings(want)
	for i := range want {
		if scanned[i] != want[i] {
			t.Fatalf("position %d: scanned %q, want %q", i, scanned[i], want[i])
		}
	}
}

// The tree has to survive being closed and reopened, since that is the whole
// point of putting it on disk.
func TestBTreePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tree.db")

	pager, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := CreateBTree(pager, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Root()

	const n = 3000
	for i := 0; i < n; i++ {
		if err := tree.Put(key(i), []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// The root moves when the tree grows, so record where it ended up.
	root = tree.Root()
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}

	pager2, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer pager2.Close()

	tree2 := NewBTree(pager2, root, nil)
	if err := tree2.Verify(); err != nil {
		t.Fatalf("invariants broken after reopening: %v", err)
	}
	for i := 0; i < n; i++ {
		got, found, err := tree2.Get(key(i))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("key %d is missing after reopening", i)
		}
		if want := fmt.Sprintf("v%d", i); string(got) != want {
			t.Fatalf("key %d: got %q, want %q", i, got, want)
		}
	}
}

// Fanout is the reason this structure exists, so it is worth asserting: a
// million rows should be a handful of levels, not twenty.
func TestBTreeStaysShallow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the large fanout check in short mode")
	}
	tree, _ := newTestTree(t)

	const n = 100000
	for i := 0; i < n; i++ {
		if err := tree.Put(key(i), []byte("v")); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	height, err := tree.Height()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows -> height %d", n, height)
	if height > 4 {
		t.Errorf("height %d for %d rows; the fanout is far lower than expected", height, n)
	}
}

func BenchmarkBTreePut(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	pager, _ := Open(path, 1024)
	defer pager.Close()
	tree, _ := CreateBTree(pager, nil)

	value := []byte("a representative row value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tree.Put(key(i), value); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBTreeGet(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	pager, _ := Open(path, 1024)
	defer pager.Close()
	tree, _ := CreateBTree(pager, nil)

	const n = 100000
	for i := 0; i < n; i++ {
		tree.Put(key(i), []byte("value"))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found, err := tree.Get(key(i % n)); err != nil || !found {
			b.Fatalf("get: found=%v err=%v", found, err)
		}
	}
}

func BenchmarkBTreeScan(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	pager, _ := Open(path, 1024)
	defer pager.Close()
	tree, _ := CreateBTree(pager, nil)

	for i := 0; i < 100000; i++ {
		tree.Put(key(i), []byte("value"))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cursor, err := tree.Scan(key(50000), key(50100))
		if err != nil {
			b.Fatal(err)
		}
		for cursor.Valid() {
			cursor.Next()
		}
	}
}
