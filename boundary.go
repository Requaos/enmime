package enmime

import (
	"bufio"
	"bytes"
	stderrors "errors"
	"io"
	"io/ioutil"
	"unicode"

	"github.com/pkg/errors"
)

// This constant needs to be at least 76 for this package to work correctly.  This is because
// \r\n--separator_of_len_70- would fill the buffer and it wouldn't be safe to consume a single byte
// from it.
const peekBufferSize = 4096

var errNoBoundaryTerminator = stderrors.New("expected boundary not present")

type boundaryReader struct {
	finished         bool          // No parts remain when finished
	partsRead        int           // Number of parts read thus far
	r                *bufio.Reader // Source reader
	nlPrefix         []byte        // NL + MIME boundary prefix
	prefix           []byte        // MIME boundary prefix
	final            []byte        // Final boundary prefix
	buffer           *bytes.Buffer // Content waiting to be read
	crBoundaryPrefix bool          // Flag for CR in CRLF + MIME boundary
	unbounded        bool          // Flag to throw errNoBoundaryTerminator
}

// newBoundaryReader returns an initialized boundaryReader
func newBoundaryReader(reader *bufio.Reader, boundary string) *boundaryReader {
	fullBoundary := []byte("\n--" + boundary + "--")
	return &boundaryReader{
		r:        reader,
		nlPrefix: fullBoundary[:len(fullBoundary)-2],
		prefix:   fullBoundary[1 : len(fullBoundary)-2],
		final:    fullBoundary[1:],
		buffer:   new(bytes.Buffer),
	}
}

// Read returns a buffer containing the content up until boundary
//
//   Excerpt from io package on io.Reader implementations:
//
//     type Reader interface {
//        Read(p []byte) (n int, err error)
//     }
//
//     Read reads up to len(p) bytes into p. It returns the number of
//     bytes read (0 <= n <= len(p)) and any error encountered. Even
//     if Read returns n < len(p), it may use all of p as scratch space
//     during the call. If some data is available but not len(p) bytes,
//     Read conventionally returns what is available instead of waiting
//     for more.
//
//     When Read encounters an error or end-of-file condition after
//     successfully reading n > 0 bytes, it returns the number of bytes
//     read. It may return the (non-nil) error from the same call or
//     return the error (and n == 0) from a subsequent call. An instance
//     of this general case is that a Reader returning a non-zero number
//     of bytes at the end of the input stream may return either err == EOF
//     or err == nil. The next Read should return 0, EOF.
//
//     Callers should always process the n > 0 bytes returned before
//     considering the error err. Doing so correctly handles I/O errors
//     that happen after reading some bytes and also both of the allowed
//     EOF behaviors.
func (b *boundaryReader) Read(dest []byte) (n int, err error) {
	if b.buffer.Len() >= len(dest) {
		// This read request can be satisfied entirely by the buffer.
		return b.buffer.Read(dest)
	}

	for i := 0; i < cap(dest); i++ {
		c, err := b.r.Peek(1)
		if err != nil && err != io.EOF {
			return 0, errors.WithStack(err)
		}
		// Ensure that we can switch on the first byte of 'c' without panic.
		if len(c) > 0 {
			switch c[0] {
			// Check for line feed as potential LF boundary prefix.
			case '\n':
				peek, err := b.r.Peek(len(b.nlPrefix) + 2)
				switch err {
				case nil:
					// Check the whitespace at the head of the peek to avoid checking for a boundary early.
					if bytes.HasPrefix(peek, []byte("\n\n")) ||
						bytes.HasPrefix(peek, []byte("\n\r")) {
						break
					}
					// Check the peek buffer for a boundary delimiter or terminator.
					if b.isDelimiter(peek[1:]) || b.isTerminator(peek[1:]) {
						// Check if we stored a carriage return.
						if b.crBoundaryPrefix {
							b.crBoundaryPrefix = false
							// Let us now unread that back onto the io.Reader, since
							// we have found what we are looking for and this byte
							// belongs to the bounded block we are reading.
							err = b.r.UnreadByte()
							switch err {
							case nil:
								// Carry on.
							case bufio.ErrInvalidUnreadByte:
								// Carriage return boundary prefix bit already unread.
							default:
								return 0, errors.WithStack(err)
							}
						}
						// We have found our boundary terminator, lets write out the final bytes
						// and return io.EOF to indicate that this section read is complete.
						n, err = b.buffer.Read(dest)
						switch err {
						case nil, io.EOF:
							return n, io.EOF
						default:
							return 0, errors.WithStack(err)
						}
					}
				case io.EOF:
					// We have reached the end without finding a boundary,
					// so we flag the boundary reader to add an error to
					// the errors slice and write what we have to the buffer.
					b.unbounded = true
				default:
					continue
				}
				// Checked '\n' was not prefix to a boundary.
				if b.crBoundaryPrefix {
					b.crBoundaryPrefix = false
					// Stored '\r' should be written to the buffer now.
					err = b.buffer.WriteByte('\r')
					if err != nil {
						return 0, errors.WithStack(err)
					}
				}
			// Check for carriage return as potential CRLF boundary prefix.
			case '\r':
				_, err := b.r.ReadByte()
				if err != nil {
					return 0, errors.WithStack(err)
				}
				// Flag the boundary reader to indicate that we
				// have stored a '\r' as a potential CRLF prefix.
				b.crBoundaryPrefix = true
				continue
			}
		}

		_, err = io.CopyN(b.buffer, b.r, 1)
		if err != nil {
			// EOF is not fatal, it just means that we have drained the reader.
			if errors.Cause(err) == io.EOF {
				break
			}
			return 0, err
		}
	}

	// Read the contents of the buffer into the destination slice.
	n, err = b.buffer.Read(dest)
	return n, err
}

// Next moves over the boundary to the next part, returns true if there is another part to be read.
func (b *boundaryReader) Next() (bool, error) {
	if b.finished {
		return false, nil
	}
	if b.partsRead > 0 {
		// Exhaust the current part to prevent errors when moving to the next part.
		_, _ = io.Copy(ioutil.Discard, b)
	}
	for {
		line, err := b.r.ReadSlice('\n')
		if err != nil && err != io.EOF {
			return false, errors.WithStack(err)
		}
		if len(line) > 0 && (line[0] == '\r' || line[0] == '\n') {
			// Blank line
			continue
		}
		if b.isTerminator(line) {
			b.finished = true
			return false, nil
		}
		if err != io.EOF && b.isDelimiter(line) {
			// Start of a new part.
			b.partsRead++
			return true, nil
		}
		if err == io.EOF {
			// Intentionally not wrapping with stack.
			return false, io.EOF
		}
		if b.partsRead == 0 {
			// The first part didn't find the starting delimiter, burn off any preamble in front of
			// the boundary.
			continue
		}
		b.finished = true
		return false, errors.WithMessagef(errNoBoundaryTerminator, "expecting boundary %q, got %q", string(b.prefix), string(line))
	}
}

// isDelimiter returns true for --BOUNDARY\r\n but not --BOUNDARY--
func (b *boundaryReader) isDelimiter(buf []byte) bool {
	idx := bytes.Index(buf, b.prefix)
	if idx == -1 {
		return false
	}

	// Fast forward to the end of the boundary prefix.
	buf = buf[idx+len(b.prefix):]
	if len(buf) > 0 {
		if unicode.IsSpace(rune(buf[0])) {
			return true
		}
	}

	return false
}

// isTerminator returns true for --BOUNDARY--
func (b *boundaryReader) isTerminator(buf []byte) bool {
	idx := bytes.Index(buf, b.final)
	return idx != -1
}
