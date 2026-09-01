// Package types is the database's value system: the types a column can hold,
// the values that flow through query execution, and the two encodings that put
// them on disk.
//
// Two encodings, because they answer different questions. A row encoding just
// has to round-trip, so it is compact and self-describing. A key encoding has
// to sort: the B+tree compares keys with bytes.Compare and knows nothing about
// integers or strings, so the bytes themselves must already be in the right
// order. Getting that wrong is subtle -- everything works until you insert a
// negative number and the index silently orders it after every positive one.
package types

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Type is a column's declared type.
type Type uint8

const (
	TypeNull Type = iota
	TypeBool
	TypeInt
	TypeFloat
	TypeText
)

func (t Type) String() string {
	switch t {
	case TypeBool:
		return "BOOLEAN"
	case TypeInt:
		return "INTEGER"
	case TypeFloat:
		return "FLOAT"
	case TypeText:
		return "TEXT"
	default:
		return "NULL"
	}
}

// ParseType reads a type name from a CREATE TABLE statement, accepting the
// common SQL spellings so a schema written for another database mostly works.
func ParseType(name string) (Type, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INT", "INTEGER", "BIGINT", "SMALLINT", "SERIAL":
		return TypeInt, true
	case "FLOAT", "REAL", "DOUBLE", "DOUBLE PRECISION", "NUMERIC", "DECIMAL":
		return TypeFloat, true
	case "TEXT", "VARCHAR", "CHAR", "STRING":
		return TypeText, true
	case "BOOL", "BOOLEAN":
		return TypeBool, true
	default:
		return TypeNull, false
	}
}

// Value is one datum.
//
// It is a struct rather than an interface because values are the unit of work
// during execution -- every row of every scan produces one per column -- and an
// interface would put each of them on the heap.
type Value struct {
	Type  Type
	Int   int64
	Float float64
	Str   string
	Bool  bool
}

// Constructors.

func Null() Value              { return Value{Type: TypeNull} }
func NewInt(v int64) Value     { return Value{Type: TypeInt, Int: v} }
func NewFloat(v float64) Value { return Value{Type: TypeFloat, Float: v} }
func NewText(v string) Value   { return Value{Type: TypeText, Str: v} }
func NewBool(v bool) Value     { return Value{Type: TypeBool, Bool: v} }

func (v Value) IsNull() bool { return v.Type == TypeNull }

// String renders a value for display.
func (v Value) String() string {
	switch v.Type {
	case TypeInt:
		return strconv.FormatInt(v.Int, 10)
	case TypeFloat:
		// The shortest representation that round-trips, so 1.5 prints as "1.5"
		// and not "1.500000".
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case TypeText:
		return v.Str
	case TypeBool:
		if v.Bool {
			return "true"
		}
		return "false"
	default:
		return "NULL"
	}
}

// Truthy reports whether a value counts as true in a WHERE clause.
//
// A NULL is not true -- but it is not false either, which is why callers that
// care about three-valued logic check IsNull separately. For filtering, "not
// true" is the right answer: a row whose predicate is unknown is not returned.
func (v Value) Truthy() bool {
	switch v.Type {
	case TypeBool:
		return v.Bool
	case TypeInt:
		return v.Int != 0
	case TypeFloat:
		return v.Float != 0
	case TypeText:
		return v.Str != ""
	default:
		return false
	}
}

// Compare orders two values, returning -1, 0 or 1.
//
// NULL sorts before everything, which is a choice: the SQL standard leaves it
// to the implementation, PostgreSQL sorts NULL last by default and SQLite
// sorts it first. Sorting it first means the key encoding can give NULL the
// lowest tag byte and get index ordering for free.
//
// The second result is false when the values are not comparable at all.
func Compare(a, b Value) (int, bool) {
	if a.IsNull() || b.IsNull() {
		switch {
		case a.IsNull() && b.IsNull():
			return 0, true
		case a.IsNull():
			return -1, true
		default:
			return 1, true
		}
	}

	// Mixed numeric comparison promotes to float, so `WHERE price > 10`
	// works whether price is stored as an integer or not.
	if isNumeric(a.Type) && isNumeric(b.Type) {
		if a.Type == TypeInt && b.Type == TypeInt {
			switch {
			case a.Int < b.Int:
				return -1, true
			case a.Int > b.Int:
				return 1, true
			default:
				return 0, true
			}
		}
		af, bf := a.asFloat(), b.asFloat()
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}

	if a.Type != b.Type {
		return 0, false
	}
	switch a.Type {
	case TypeText:
		return strings.Compare(a.Str, b.Str), true
	case TypeBool:
		switch {
		case a.Bool == b.Bool:
			return 0, true
		case !a.Bool:
			return -1, true
		default:
			return 1, true
		}
	default:
		return 0, false
	}
}

func isNumeric(t Type) bool { return t == TypeInt || t == TypeFloat }

func (v Value) asFloat() float64 {
	if v.Type == TypeInt {
		return float64(v.Int)
	}
	return v.Float
}

// Cast converts a value to a target type, as an INSERT does when the literal
// does not match the column.
func (v Value) Cast(target Type) (Value, error) {
	if v.IsNull() || v.Type == target {
		return v, nil
	}
	switch target {
	case TypeInt:
		switch v.Type {
		case TypeFloat:
			// Truncation towards zero, and only when it loses nothing:
			// silently turning 1.7 into 1 inside an integer column would be a
			// data-loss bug wearing a conversion's clothes.
			if v.Float != math.Trunc(v.Float) {
				return Null(), fmt.Errorf("cannot store %g in an INTEGER column without losing precision", v.Float)
			}
			return NewInt(int64(v.Float)), nil
		case TypeBool:
			if v.Bool {
				return NewInt(1), nil
			}
			return NewInt(0), nil
		case TypeText:
			n, err := strconv.ParseInt(strings.TrimSpace(v.Str), 10, 64)
			if err != nil {
				return Null(), fmt.Errorf("%q is not a valid INTEGER", v.Str)
			}
			return NewInt(n), nil
		}
	case TypeFloat:
		switch v.Type {
		case TypeInt:
			return NewFloat(float64(v.Int)), nil
		case TypeText:
			f, err := strconv.ParseFloat(strings.TrimSpace(v.Str), 64)
			if err != nil {
				return Null(), fmt.Errorf("%q is not a valid FLOAT", v.Str)
			}
			return NewFloat(f), nil
		}
	case TypeText:
		return NewText(v.String()), nil
	case TypeBool:
		switch v.Type {
		case TypeInt:
			return NewBool(v.Int != 0), nil
		case TypeText:
			switch strings.ToLower(strings.TrimSpace(v.Str)) {
			case "true", "t", "yes", "y", "1":
				return NewBool(true), nil
			case "false", "f", "no", "n", "0":
				return NewBool(false), nil
			}
			return Null(), fmt.Errorf("%q is not a valid BOOLEAN", v.Str)
		}
	}
	return Null(), fmt.Errorf("cannot convert %s to %s", v.Type, target)
}

// --- row encoding ---
//
// Self-describing and compact: each field is a type tag and a payload, so a
// row can be decoded without consulting the schema. That costs one byte per
// column and buys the ability to read a row even if the catalog is damaged.

// EncodeRow serialises a row of values.
func EncodeRow(values []Value) []byte {
	buf := make([]byte, 0, 16*len(values))
	buf = binary.AppendUvarint(buf, uint64(len(values)))
	for _, v := range values {
		buf = append(buf, byte(v.Type))
		switch v.Type {
		case TypeInt:
			// Zigzag, so a small negative number stays a short varint rather
			// than becoming ten bytes of leading ones.
			buf = binary.AppendVarint(buf, v.Int)
		case TypeFloat:
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v.Float))
		case TypeText:
			buf = binary.AppendUvarint(buf, uint64(len(v.Str)))
			buf = append(buf, v.Str...)
		case TypeBool:
			if v.Bool {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		}
	}
	return buf
}

// DecodeRow reverses EncodeRow.
func DecodeRow(data []byte) ([]Value, error) {
	count, read := binary.Uvarint(data)
	if read <= 0 {
		return nil, fmt.Errorf("corrupt row: bad column count")
	}
	pos := read

	values := make([]Value, 0, count)
	for i := uint64(0); i < count; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("corrupt row: truncated at column %d", i)
		}
		t := Type(data[pos])
		pos++

		switch t {
		case TypeNull:
			values = append(values, Null())
		case TypeInt:
			n, read := binary.Varint(data[pos:])
			if read <= 0 {
				return nil, fmt.Errorf("corrupt row: bad integer at column %d", i)
			}
			pos += read
			values = append(values, NewInt(n))
		case TypeFloat:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("corrupt row: truncated float at column %d", i)
			}
			values = append(values, NewFloat(math.Float64frombits(binary.LittleEndian.Uint64(data[pos:]))))
			pos += 8
		case TypeText:
			n, read := binary.Uvarint(data[pos:])
			if read <= 0 || pos+read+int(n) > len(data) {
				return nil, fmt.Errorf("corrupt row: bad text at column %d", i)
			}
			pos += read
			values = append(values, NewText(string(data[pos:pos+int(n)])))
			pos += int(n)
		case TypeBool:
			if pos >= len(data) {
				return nil, fmt.Errorf("corrupt row: truncated boolean at column %d", i)
			}
			values = append(values, NewBool(data[pos] == 1))
			pos++
		default:
			return nil, fmt.Errorf("corrupt row: unknown type tag %d at column %d", t, i)
		}
	}
	return values, nil
}

// --- key encoding ---
//
// Order-preserving: the B+tree compares keys with bytes.Compare and knows
// nothing about types, so the encoding has to place the bytes in semantic
// order by itself.

// Key type tags. They are ordered so that NULL < numbers < text < boolean,
// which fixes how mixed-type keys sort.
const (
	keyTagNull  byte = 0x00
	keyTagInt   byte = 0x01
	keyTagFloat byte = 0x02
	keyTagText  byte = 0x03
	keyTagBool  byte = 0x04
)

// EncodeKey produces a byte string that sorts in the same order as the values.
func EncodeKey(values []Value) []byte {
	buf := make([]byte, 0, 16*len(values))
	for _, v := range values {
		switch v.Type {
		case TypeNull:
			buf = append(buf, keyTagNull)

		case TypeInt:
			buf = append(buf, keyTagInt)
			// Big-endian, so the most significant byte compares first, with
			// the sign bit flipped so negatives sort below positives. Without
			// the flip, -1 is 0xFFFF... and would sort above every positive
			// number -- the index would be silently, plausibly wrong.
			buf = binary.BigEndian.AppendUint64(buf, uint64(v.Int)^(1<<63))

		case TypeFloat:
			buf = append(buf, keyTagFloat)
			bits := math.Float64bits(v.Float)
			// IEEE-754 floats are already ordered as integers when positive.
			// Negative ones are ordered backwards, so their bits are inverted;
			// positives just get the sign bit set to sort above them.
			if bits&(1<<63) != 0 {
				bits = ^bits
			} else {
				bits |= 1 << 63
			}
			buf = binary.BigEndian.AppendUint64(buf, bits)

		case TypeText:
			buf = append(buf, keyTagText)
			// Escape the terminator so a value containing 0x00 cannot end its
			// own field and let "a\x00b" collide with a two-column key.
			for i := 0; i < len(v.Str); i++ {
				if v.Str[i] == 0x00 {
					buf = append(buf, 0x00, 0xFF)
				} else {
					buf = append(buf, v.Str[i])
				}
			}
			buf = append(buf, 0x00, 0x00)

		case TypeBool:
			buf = append(buf, keyTagBool)
			if v.Bool {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		}
	}
	return buf
}

// DecodeKey reverses EncodeKey.
func DecodeKey(data []byte) ([]Value, error) {
	var values []Value
	pos := 0
	for pos < len(data) {
		tag := data[pos]
		pos++
		switch tag {
		case keyTagNull:
			values = append(values, Null())
		case keyTagInt:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("corrupt key: truncated integer")
			}
			values = append(values, NewInt(int64(binary.BigEndian.Uint64(data[pos:])^(1<<63))))
			pos += 8
		case keyTagFloat:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("corrupt key: truncated float")
			}
			bits := binary.BigEndian.Uint64(data[pos:])
			if bits&(1<<63) != 0 {
				bits &^= 1 << 63
			} else {
				bits = ^bits
			}
			values = append(values, NewFloat(math.Float64frombits(bits)))
			pos += 8
		case keyTagText:
			var sb strings.Builder
			for {
				if pos+1 >= len(data) {
					return nil, fmt.Errorf("corrupt key: unterminated text")
				}
				if data[pos] == 0x00 {
					if data[pos+1] == 0x00 {
						pos += 2
						break
					}
					// 0x00 0xFF is an escaped null byte.
					sb.WriteByte(0x00)
					pos += 2
					continue
				}
				sb.WriteByte(data[pos])
				pos++
			}
			values = append(values, NewText(sb.String()))
		case keyTagBool:
			if pos >= len(data) {
				return nil, fmt.Errorf("corrupt key: truncated boolean")
			}
			values = append(values, NewBool(data[pos] == 1))
			pos++
		default:
			return nil, fmt.Errorf("corrupt key: unknown tag %d", tag)
		}
	}
	return values, nil
}

// EncodeRowID encodes a row id as a sortable key, which is how a table's
// primary B+tree is ordered when no primary key was declared.
func EncodeRowID(id int64) []byte {
	return EncodeKey([]Value{NewInt(id)})
}
