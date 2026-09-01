package types

import (
	"bytes"
	"math"
	"sort"
	"testing"
)

func TestRowEncodingRoundTrip(t *testing.T) {
	rows := [][]Value{
		{},
		{Null()},
		{NewInt(0), NewInt(1), NewInt(-1)},
		{NewInt(math.MaxInt64), NewInt(math.MinInt64)},
		{NewFloat(0), NewFloat(1.5), NewFloat(-1.5)},
		{NewFloat(math.Inf(1)), NewFloat(math.Inf(-1))},
		{NewText(""), NewText("hello"), NewText("with\x00null\r\nbytes")},
		{NewBool(true), NewBool(false)},
		{NewInt(1), NewText("mixed"), NewFloat(2.5), NewBool(true), Null()},
	}

	for _, want := range rows {
		encoded := EncodeRow(want)
		got, err := DecodeRow(encoded)
		if err != nil {
			t.Fatalf("decode %v: %v", want, err)
		}
		if len(got) != len(want) {
			t.Fatalf("got %d values, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].Type != want[i].Type {
				t.Errorf("value %d: type %v, want %v", i, got[i].Type, want[i].Type)
				continue
			}
			if got[i].String() != want[i].String() {
				t.Errorf("value %d: got %q, want %q", i, got[i], want[i])
			}
		}
	}
}

func TestRowDecodeRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		{},
		{0xFF},
		{2, byte(TypeInt)},        // claims two columns, provides part of one
		{1, byte(TypeText), 0xFF}, // text length runs past the end
		{1, 99},                   // unknown type tag
	}
	for i, data := range cases {
		if _, err := DecodeRow(data); err == nil {
			t.Errorf("case %d: corrupt input was accepted", i)
		}
	}
}

// The key encoding exists so that bytes.Compare agrees with value order. If it
// does not, every index is silently, plausibly wrong -- so this is the most
// important test in the package.
func TestKeyEncodingPreservesOrder(t *testing.T) {
	groups := []struct {
		name   string
		values []Value
	}{
		{
			// Negative integers are the case a naive encoding gets wrong: as
			// raw bytes, -1 is 0xFFFF... and would sort above every positive.
			"integers",
			[]Value{
				NewInt(math.MinInt64), NewInt(-1000000), NewInt(-1), NewInt(0),
				NewInt(1), NewInt(1000000), NewInt(math.MaxInt64),
			},
		},
		{
			"floats",
			[]Value{
				NewFloat(math.Inf(-1)), NewFloat(-1e10), NewFloat(-1.5),
				NewFloat(-0.001), NewFloat(0), NewFloat(0.001),
				NewFloat(1.5), NewFloat(1e10), NewFloat(math.Inf(1)),
			},
		},
		{
			"text",
			[]Value{
				NewText(""), NewText("a"), NewText("aa"), NewText("ab"),
				NewText("b"), NewText("z"), NewText("zz"),
			},
		},
		{
			"booleans",
			[]Value{NewBool(false), NewBool(true)},
		},
	}

	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for i := 0; i < len(g.values)-1; i++ {
				a := EncodeKey([]Value{g.values[i]})
				b := EncodeKey([]Value{g.values[i+1]})
				if bytes.Compare(a, b) >= 0 {
					t.Errorf("%v encodes at or above %v, breaking index order",
						g.values[i], g.values[i+1])
				}
			}
		})
	}
}

// NULL sorts before every value, which is the choice the encoding's tag bytes
// bake in.
func TestKeyEncodingNullSortsFirst(t *testing.T) {
	null := EncodeKey([]Value{Null()})
	for _, v := range []Value{
		NewInt(math.MinInt64), NewFloat(math.Inf(-1)), NewText(""), NewBool(false),
	} {
		if bytes.Compare(null, EncodeKey([]Value{v})) >= 0 {
			t.Errorf("NULL does not sort before %v", v)
		}
	}
}

// A random permutation, encoded and sorted as bytes, must come back in value
// order. This is the property the B+tree actually depends on.
func TestKeyEncodingSortsLikeValues(t *testing.T) {
	values := []Value{
		NewInt(-500), NewInt(3), NewInt(0), NewInt(-1), NewInt(999999),
		NewInt(-999999), NewInt(1), NewInt(-2), NewInt(42),
	}

	encoded := make([][]byte, len(values))
	for i, v := range values {
		encoded[i] = EncodeKey([]Value{v})
	}
	sort.Slice(encoded, func(i, j int) bool {
		return bytes.Compare(encoded[i], encoded[j]) < 0
	})

	want := append([]Value(nil), values...)
	sort.Slice(want, func(i, j int) bool {
		cmp, _ := Compare(want[i], want[j])
		return cmp < 0
	})

	for i := range encoded {
		decoded, err := DecodeKey(encoded[i])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded[0].Int != want[i].Int {
			t.Fatalf("position %d: byte order gave %v, value order gives %v",
				i, decoded[0], want[i])
		}
	}
}

func TestKeyEncodingRoundTrip(t *testing.T) {
	keys := [][]Value{
		{NewInt(42)},
		{NewInt(-42)},
		{NewFloat(3.14159)},
		{NewFloat(math.Inf(-1))},
		{NewText("hello")},
		{NewText("")},
		{NewBool(true)},
		{Null()},
		// A composite key, which is what a multi-column index uses.
		{NewInt(1), NewText("compound"), NewBool(false)},
	}
	for _, want := range keys {
		got, err := DecodeKey(EncodeKey(want))
		if err != nil {
			t.Fatalf("decode %v: %v", want, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%v: got %d values, want %d", want, len(got), len(want))
		}
		for i := range want {
			if got[i].String() != want[i].String() {
				t.Errorf("%v: value %d is %q, want %q", want, i, got[i], want[i])
			}
		}
	}
}

// A text value containing a null byte must not be able to end its own field,
// or "a\x00b" in one column could collide with a two-column key.
func TestKeyEncodingEscapesNullBytes(t *testing.T) {
	tricky := NewText("a\x00b")
	plain := NewText("ab")

	if bytes.Equal(EncodeKey([]Value{tricky}), EncodeKey([]Value{plain})) {
		t.Fatal("a null byte in text collides with a different value")
	}

	got, err := DecodeKey(EncodeKey([]Value{tricky}))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Str != "a\x00b" {
		t.Errorf("escaped text did not round-trip: %q", got[0].Str)
	}

	// A composite key must still split into the right number of fields.
	composite := []Value{NewText("a\x00b"), NewText("c")}
	decoded, err := DecodeKey(EncodeKey(composite))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Str != "a\x00b" || decoded[1].Str != "c" {
		t.Errorf("composite key with an embedded null decoded as %v", decoded)
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b Value
		want int
		ok   bool
	}{
		{NewInt(1), NewInt(2), -1, true},
		{NewInt(2), NewInt(1), 1, true},
		{NewInt(1), NewInt(1), 0, true},
		// A number is a number, whichever type it is stored as.
		{NewInt(1), NewFloat(1.5), -1, true},
		{NewFloat(1.5), NewInt(2), -1, true},
		{NewInt(2), NewFloat(2.0), 0, true},
		{NewText("a"), NewText("b"), -1, true},
		{NewBool(false), NewBool(true), -1, true},
		{Null(), NewInt(1), -1, true},
		{NewInt(1), Null(), 1, true},
		{Null(), Null(), 0, true},
		// Text and numbers are not comparable.
		{NewInt(1), NewText("1"), 0, false},
	}
	for _, tc := range tests {
		got, ok := Compare(tc.a, tc.b)
		if ok != tc.ok {
			t.Errorf("Compare(%v, %v): comparable=%v, want %v", tc.a, tc.b, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCast(t *testing.T) {
	tests := []struct {
		from   Value
		to     Type
		want   string
		wantOK bool
	}{
		{NewInt(1), TypeFloat, "1", true},
		{NewFloat(2.0), TypeInt, "2", true},
		// A fractional float into an integer column is data loss, not a cast.
		{NewFloat(1.7), TypeInt, "", false},
		{NewText("42"), TypeInt, "42", true},
		{NewText("nope"), TypeInt, "", false},
		{NewText("1.5"), TypeFloat, "1.5", true},
		{NewInt(1), TypeText, "1", true},
		{NewInt(1), TypeBool, "true", true},
		{NewInt(0), TypeBool, "false", true},
		{NewText("yes"), TypeBool, "true", true},
		{NewText("maybe"), TypeBool, "", false},
		// NULL casts to anything and stays NULL.
		{Null(), TypeInt, "NULL", true},
	}
	for _, tc := range tests {
		got, err := tc.from.Cast(tc.to)
		if (err == nil) != tc.wantOK {
			t.Errorf("Cast(%v -> %v): err=%v, want ok=%v", tc.from, tc.to, err, tc.wantOK)
			continue
		}
		if tc.wantOK && got.String() != tc.want {
			t.Errorf("Cast(%v -> %v) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestParseType(t *testing.T) {
	tests := map[string]Type{
		"INT": TypeInt, "INTEGER": TypeInt, "BIGINT": TypeInt, "serial": TypeInt,
		"FLOAT": TypeFloat, "REAL": TypeFloat, "double precision": TypeFloat,
		"TEXT": TypeText, "VARCHAR": TypeText, "char": TypeText,
		"BOOL": TypeBool, "BOOLEAN": TypeBool,
	}
	for name, want := range tests {
		got, ok := ParseType(name)
		if !ok || got != want {
			t.Errorf("ParseType(%q) = %v, %v; want %v", name, got, ok, want)
		}
	}
	if _, ok := ParseType("NOSUCHTYPE"); ok {
		t.Error("an unknown type name was accepted")
	}
}

func BenchmarkEncodeRow(b *testing.B) {
	row := []Value{NewInt(12345), NewText("a representative value"), NewFloat(1.5), NewBool(true)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodeRow(row)
	}
}

func BenchmarkDecodeRow(b *testing.B) {
	data := EncodeRow([]Value{NewInt(12345), NewText("a representative value"), NewFloat(1.5), NewBool(true)})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeRow(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeKey(b *testing.B) {
	key := []Value{NewInt(12345)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodeKey(key)
	}
}
