// Package wire speaks the PostgreSQL frontend/backend protocol, so psql and
// any PostgreSQL client library can connect to quill without modification.
//
// Implementing someone else's protocol is the only honest test of a database's
// interface. A bespoke client can be made to agree with whatever the server
// happens to do; psql cannot, and it will notice a wrong message length, a
// missing ReadyForQuery, or a field count that does not match the row
// description.
//
// Message framing, after the startup exchange, is uniform: one type byte, an
// int32 length that includes itself but not the type byte, then the payload.
// Everything is big-endian, which is the one place a network protocol still
// insists on it.
package wire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Message types the backend sends.
const (
	msgAuthentication  = 'R'
	msgBackendKeyData  = 'K'
	msgParameterStatus = 'S'
	msgReadyForQuery   = 'Z'
	msgRowDescription  = 'T'
	msgDataRow         = 'D'
	msgCommandComplete = 'C'
	msgErrorResponse   = 'E'
	msgEmptyQuery      = 'I'
	msgNoticeResponse  = 'N'
	msgParseComplete   = '1'
	msgBindComplete    = '2'
	msgNoData          = 'n'
	msgParameterDesc   = 't'
	msgCloseComplete   = '3'
)

// Message types the frontend sends.
const (
	msgQuery       = 'Q'
	msgTerminate   = 'X'
	msgParse       = 'P'
	msgBind        = 'B'
	msgDescribe    = 'D'
	msgExecute     = 'E'
	msgSync        = 'S'
	msgClose       = 'C'
	msgFlush       = 'H'
	msgPasswordMsg = 'p'
)

// Transaction status, reported in every ReadyForQuery.
//
// psql shows this in its prompt: `=>` when idle, `*>` inside a transaction,
// `!>` when the transaction has failed. Getting it wrong makes the client
// display a lie about the server's state.
const (
	statusIdle   = 'I'
	statusInTxn  = 'T'
	statusFailed = 'E'
)

// PostgreSQL object identifiers for the types quill can return.
//
// The client uses these to decide how to parse a value, so they have to be the
// real numbers from pg_type, not invented ones.
const (
	oidBool   = 16
	oidInt8   = 20
	oidText   = 25
	oidFloat8 = 701
)

// protocolVersion is 3.0, encoded as major<<16 | minor. Version 3 has been the
// wire protocol since PostgreSQL 7.4 and is what every current client speaks.
const protocolVersion = 196608

// sslRequestCode and cancelRequestCode are sentinel "versions" a client sends
// in place of a real startup packet.
const (
	sslRequestCode    = 80877103
	cancelRequestCode = 80877102
	gssEncRequestCode = 80877104
)

// reader reads protocol messages.
type reader struct {
	r   io.Reader
	buf []byte
}

func newReader(r io.Reader) *reader {
	return &reader{r: r, buf: make([]byte, 0, 8192)}
}

// message is one decoded frontend message.
type message struct {
	typ     byte
	payload []byte
}

// maxMessageSize bounds a single message, so a hostile length prefix cannot
// make the server allocate arbitrarily.
const maxMessageSize = 64 * 1024 * 1024

// next reads one message.
func (r *reader) next() (message, error) {
	var header [5]byte
	if _, err := io.ReadFull(r.r, header[:]); err != nil {
		return message{}, err
	}
	typ := header[0]
	length := int(binary.BigEndian.Uint32(header[1:]))
	// The length counts itself but not the type byte.
	if length < 4 || length > maxMessageSize {
		return message{}, fmt.Errorf("invalid message length %d", length)
	}

	payload := make([]byte, length-4)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return message{}, err
	}
	return message{typ: typ, payload: payload}, nil
}

// readStartup reads the startup packet, which has no type byte -- it is the
// one message that does not, because at that point the client has not yet
// established which protocol it is speaking.
func (r *reader) readStartup() (map[string]string, int32, error) {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r.r, lengthBuf[:]); err != nil {
		return nil, 0, err
	}
	length := int(binary.BigEndian.Uint32(lengthBuf[:]))
	if length < 8 || length > maxMessageSize {
		return nil, 0, fmt.Errorf("invalid startup packet length %d", length)
	}

	payload := make([]byte, length-4)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return nil, 0, err
	}

	version := int32(binary.BigEndian.Uint32(payload))
	if version != protocolVersion {
		// SSL and cancel requests arrive here too, and the caller handles them.
		return nil, version, nil
	}

	// The rest is a run of NUL-terminated key/value strings, ended by an empty
	// key.
	params := make(map[string]string)
	rest := payload[4:]
	for len(rest) > 0 {
		key, remainder, ok := readCString(rest)
		if !ok || key == "" {
			break
		}
		value, remainder2, ok := readCString(remainder)
		if !ok {
			break
		}
		params[key] = value
		rest = remainder2
	}
	return params, version, nil
}

func readCString(b []byte) (string, []byte, bool) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], true
		}
	}
	return "", nil, false
}

// writer builds and sends protocol messages.
type writer struct {
	w   io.Writer
	buf []byte
}

func newWriter(w io.Writer) *writer {
	return &writer{w: w, buf: make([]byte, 0, 8192)}
}

// begin starts a message of the given type.
//
// The length is filled in by end, once the payload is known -- which is why
// messages are built in a buffer rather than streamed.
func (w *writer) begin(typ byte) int {
	w.buf = append(w.buf, typ)
	start := len(w.buf)
	w.buf = append(w.buf, 0, 0, 0, 0) // length placeholder
	return start
}

func (w *writer) end(start int) {
	length := len(w.buf) - start
	binary.BigEndian.PutUint32(w.buf[start:], uint32(length))
}

func (w *writer) int16(v int16) {
	w.buf = binary.BigEndian.AppendUint16(w.buf, uint16(v))
}

func (w *writer) int32(v int32) {
	w.buf = binary.BigEndian.AppendUint32(w.buf, uint32(v))
}

// cstring writes a NUL-terminated string.
func (w *writer) cstring(s string) {
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

func (w *writer) bytes(b []byte) { w.buf = append(w.buf, b...) }

// flush sends everything buffered.
func (w *writer) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.w.Write(w.buf)
	w.buf = w.buf[:0]
	return err
}

// --- backend messages ---

// authenticationOK tells the client it is through.
//
// quill accepts every connection. Adding real authentication would mean
// implementing SCRAM-SHA-256, which is a genuine piece of work and is listed
// as future work rather than stubbed with something weaker.
func (w *writer) authenticationOK() {
	start := w.begin(msgAuthentication)
	w.int32(0) // 0 means "authentication successful"
	w.end(start)
}

// parameterStatus reports a server setting.
//
// psql reads server_version to decide which features to use, and
// client_encoding to decode text. Omitting them makes the client guess.
func (w *writer) parameterStatus(name, value string) {
	start := w.begin(msgParameterStatus)
	w.cstring(name)
	w.cstring(value)
	w.end(start)
}

// backendKeyData carries the process id and secret used to cancel a query.
// Cancellation is not implemented, but the message is expected.
func (w *writer) backendKeyData(pid, secret int32) {
	start := w.begin(msgBackendKeyData)
	w.int32(pid)
	w.int32(secret)
	w.end(start)
}

// readyForQuery ends every exchange and reports transaction status.
//
// A client blocks until it arrives, so a path that returns without sending one
// hangs the connection -- which is the single easiest way to get this protocol
// wrong.
func (w *writer) readyForQuery(status byte) {
	start := w.begin(msgReadyForQuery)
	w.buf = append(w.buf, status)
	w.end(start)
}

// field describes one output column.
type field struct {
	name string
	oid  int32
}

// rowDescription announces the shape of the rows that follow.
func (w *writer) rowDescription(fields []field) {
	start := w.begin(msgRowDescription)
	w.int16(int16(len(fields)))
	for _, f := range fields {
		w.cstring(f.name)
		w.int32(0) // table oid: unknown
		w.int16(0) // column attribute number
		w.int32(f.oid)
		w.int16(-1) // type size: variable
		w.int32(-1) // type modifier
		w.int16(0)  // format code: 0 is text
	}
	w.end(start)
}

// dataRow sends one row. A nil element is SQL NULL, encoded as length -1,
// which is how the protocol distinguishes it from an empty string.
func (w *writer) dataRow(values [][]byte) {
	start := w.begin(msgDataRow)
	w.int16(int16(len(values)))
	for _, v := range values {
		if v == nil {
			w.int32(-1)
			continue
		}
		w.int32(int32(len(v)))
		w.bytes(v)
	}
	w.end(start)
}

// commandComplete reports the tag: "SELECT 3", "INSERT 0 1".
func (w *writer) commandComplete(tag string) {
	start := w.begin(msgCommandComplete)
	w.cstring(tag)
	w.end(start)
}

func (w *writer) emptyQueryResponse() {
	start := w.begin(msgEmptyQuery)
	w.end(start)
}

// errorResponse reports a failure.
//
// The fields are a series of tagged strings: S is severity, C is the SQLSTATE
// code, M is the message. psql shows M and switches on C, so a plausible code
// matters more than it looks -- 42601 for a syntax error, 42P01 for a missing
// table.
func (w *writer) errorResponse(severity, code, message string) {
	start := w.begin(msgErrorResponse)
	w.buf = append(w.buf, 'S')
	w.cstring(severity)
	w.buf = append(w.buf, 'V')
	w.cstring(severity)
	w.buf = append(w.buf, 'C')
	w.cstring(code)
	w.buf = append(w.buf, 'M')
	w.cstring(message)
	w.buf = append(w.buf, 0) // terminator
	w.end(start)
}

func (w *writer) parseComplete() {
	start := w.begin(msgParseComplete)
	w.end(start)
}

func (w *writer) bindComplete() {
	start := w.begin(msgBindComplete)
	w.end(start)
}

func (w *writer) closeComplete() {
	start := w.begin(msgCloseComplete)
	w.end(start)
}

func (w *writer) noData() {
	start := w.begin(msgNoData)
	w.end(start)
}

// parameterDescription announces the parameter types of a prepared statement.
func (w *writer) parameterDescription(oids []int32) {
	start := w.begin(msgParameterDesc)
	w.int16(int16(len(oids)))
	for _, oid := range oids {
		w.int32(oid)
	}
	w.end(start)
}
