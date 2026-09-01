// Package sql turns SQL text into a syntax tree: a hand-written lexer and a
// recursive-descent parser.
//
// Hand-written rather than generated, because SQL's grammar is small enough at
// this scope that a parser generator would add a build step and a layer of
// indirection without removing much work -- and because the error messages a
// hand-written parser produces ("expected FROM, found WHERE at position 24")
// are the ones a person can act on.
package sql

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenType classifies a token.
type TokenType uint8

const (
	TokenEOF TokenType = iota
	TokenIdent
	TokenKeyword
	TokenNumber
	TokenString
	TokenOperator
	TokenPunct
)

func (t TokenType) String() string {
	switch t {
	case TokenIdent:
		return "identifier"
	case TokenKeyword:
		return "keyword"
	case TokenNumber:
		return "number"
	case TokenString:
		return "string"
	case TokenOperator:
		return "operator"
	case TokenPunct:
		return "punctuation"
	default:
		return "end of input"
	}
}

// Token is one lexical unit.
type Token struct {
	Type TokenType
	// Literal is the token's text. Keywords are upper-cased so the parser can
	// compare them directly; identifiers keep the case the user typed, because
	// it is what gets displayed back in result headers.
	Literal string
	Pos     int
}

func (t Token) String() string {
	if t.Type == TokenEOF {
		return "end of input"
	}
	return fmt.Sprintf("%q", t.Literal)
}

// keywords is the reserved word set.
//
// A word is only a keyword where the grammar expects one: `SELECT count FROM t`
// has to work, because "count" is a perfectly reasonable column name. The
// parser therefore accepts a keyword token wherever an identifier is allowed,
// rather than the lexer trying to decide.
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true, "INTO": true,
	"VALUES": true, "UPDATE": true, "SET": true, "DELETE": true, "CREATE": true,
	"TABLE": true, "DROP": true, "INDEX": true, "ON": true, "AND": true,
	"OR": true, "NOT": true, "NULL": true, "IS": true, "IN": true,
	"ORDER": true, "BY": true, "ASC": true, "DESC": true, "LIMIT": true,
	"OFFSET": true, "GROUP": true, "HAVING": true, "AS": true, "JOIN": true,
	"INNER": true, "LEFT": true, "RIGHT": true, "OUTER": true, "CROSS": true,
	"PRIMARY": true, "KEY": true, "UNIQUE": true, "DEFAULT": true,
	"TRUE": true, "FALSE": true, "DISTINCT": true, "COUNT": true, "SUM": true,
	"AVG": true, "MIN": true, "MAX": true, "BEGIN": true, "COMMIT": true,
	"ROLLBACK": true, "TRANSACTION": true, "EXPLAIN": true, "IF": true,
	"EXISTS": true, "LIKE": true, "BETWEEN": true, "INT": true, "INTEGER": true,
	"TEXT": true, "VARCHAR": true, "FLOAT": true, "REAL": true, "BOOLEAN": true,
	"BOOL": true, "BIGINT": true, "DOUBLE": true, "CHAR": true, "SERIAL": true,
}

// Lexer turns SQL text into tokens.
type Lexer struct {
	input string
	pos   int
}

func NewLexer(input string) *Lexer { return &Lexer{input: input} }

// Tokenize reads the whole input.
func Tokenize(input string) ([]Token, error) {
	l := NewLexer(input)
	var out []Token
	for {
		tok, err := l.Next()
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		if tok.Type == TokenEOF {
			return out, nil
		}
	}
}

func (l *Lexer) skipWhitespaceAndComments() {
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if unicode.IsSpace(rune(c)) {
			l.pos++
			continue
		}
		// -- runs to the end of the line.
		if c == '-' && l.pos+1 < len(l.input) && l.input[l.pos+1] == '-' {
			for l.pos < len(l.input) && l.input[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		// /* ... */ blocks.
		if c == '/' && l.pos+1 < len(l.input) && l.input[l.pos+1] == '*' {
			l.pos += 2
			for l.pos+1 < len(l.input) && !(l.input[l.pos] == '*' && l.input[l.pos+1] == '/') {
				l.pos++
			}
			l.pos += 2
			if l.pos > len(l.input) {
				l.pos = len(l.input)
			}
			continue
		}
		return
	}
}

// Next produces the next token.
func (l *Lexer) Next() (Token, error) {
	l.skipWhitespaceAndComments()
	if l.pos >= len(l.input) {
		return Token{Type: TokenEOF, Pos: l.pos}, nil
	}

	start := l.pos
	c := l.input[l.pos]

	switch {
	case isIdentStart(c):
		for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
			l.pos++
		}
		word := l.input[start:l.pos]
		upper := strings.ToUpper(word)
		if keywords[upper] {
			return Token{Type: TokenKeyword, Literal: upper, Pos: start}, nil
		}
		return Token{Type: TokenIdent, Literal: word, Pos: start}, nil

	case c >= '0' && c <= '9':
		return l.lexNumber(start)

	case c == '\'':
		return l.lexString(start)

	case c == '"':
		// A double-quoted identifier, which is how SQL escapes a name that
		// collides with a keyword or contains punctuation.
		l.pos++
		var sb strings.Builder
		for {
			if l.pos >= len(l.input) {
				return Token{}, fmt.Errorf("unterminated quoted identifier at position %d", start)
			}
			if l.input[l.pos] == '"' {
				if l.pos+1 < len(l.input) && l.input[l.pos+1] == '"' {
					sb.WriteByte('"')
					l.pos += 2
					continue
				}
				l.pos++
				break
			}
			sb.WriteByte(l.input[l.pos])
			l.pos++
		}
		return Token{Type: TokenIdent, Literal: sb.String(), Pos: start}, nil

	default:
		return l.lexOperator(start)
	}
}

func (l *Lexer) lexNumber(start int) (Token, error) {
	seenDot := false
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c >= '0' && c <= '9' {
			l.pos++
			continue
		}
		if c == '.' && !seenDot {
			seenDot = true
			l.pos++
			continue
		}
		// Exponent form: 1e10, 1.5E-3.
		if (c == 'e' || c == 'E') && l.pos+1 < len(l.input) {
			next := l.input[l.pos+1]
			if next >= '0' && next <= '9' || next == '+' || next == '-' {
				l.pos += 2
				for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
					l.pos++
				}
				break
			}
		}
		break
	}
	return Token{Type: TokenNumber, Literal: l.input[start:l.pos], Pos: start}, nil
}

func (l *Lexer) lexString(start int) (Token, error) {
	l.pos++ // opening quote
	var sb strings.Builder
	for {
		if l.pos >= len(l.input) {
			return Token{}, fmt.Errorf("unterminated string starting at position %d", start)
		}
		c := l.input[l.pos]
		if c == '\'' {
			// SQL escapes a quote by doubling it: 'it''s'.
			if l.pos+1 < len(l.input) && l.input[l.pos+1] == '\'' {
				sb.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++
			break
		}
		sb.WriteByte(c)
		l.pos++
	}
	return Token{Type: TokenString, Literal: sb.String(), Pos: start}, nil
}

func (l *Lexer) lexOperator(start int) (Token, error) {
	// Two-character operators must be tried first, or "<=" lexes as "<" then
	// "=" and the parser sees a comparison against an assignment.
	if l.pos+1 < len(l.input) {
		two := l.input[l.pos : l.pos+2]
		switch two {
		case "<=", ">=", "<>", "!=", "||":
			l.pos += 2
			return Token{Type: TokenOperator, Literal: two, Pos: start}, nil
		}
	}

	c := l.input[l.pos]
	switch c {
	case '=', '<', '>', '+', '-', '*', '/', '%':
		l.pos++
		return Token{Type: TokenOperator, Literal: string(c), Pos: start}, nil
	case '(', ')', ',', ';', '.':
		l.pos++
		return Token{Type: TokenPunct, Literal: string(c), Pos: start}, nil
	default:
		return Token{}, fmt.Errorf("unexpected character %q at position %d", string(c), start)
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}
