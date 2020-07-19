// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shadowsocks

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
)

// payloadSizeMask is the maximum size of payload in bytes.
const payloadSizeMask = 0x3FFF // 16*1024 - 1

// Writer is an io.Writer that also implements io.ReaderFrom to
// allow for piping the data without extra allocations and copies.
// The LazyWrite and Flush methods allow a header to be
// added but delayed until the first write, for concatenation.
// All methods except Flush must be called from a single thread.
type Writer interface {
	io.Writer
	io.ReaderFrom
	// LazyWrite queues p to be written, but doesn't send it until
	// Flush() is called, a non-lazy Write() is made, or the buffer
	// is filled.
	LazyWrite(p []byte) (int, error)
	// Flush sends the pending lazy data, if any.  This method is
	// thread-safe with respect to Write and ReadFrom, but must not
	// be concurrent with LazyWrite.
	Flush() error
}

type shadowsocksWriter struct {
	writer   io.Writer
	ssCipher shadowaead.Cipher
	// Wrapper for input that arrives as a slice.
	byteWrapper bytes.Reader
	// Action to flush a pending lazy write.
	flush sync.Once
	// Number of bytes that are currently buffered.
	pending int
	// These are populated by init():
	buf  []byte
	aead cipher.AEAD
	// Index of the next encrypted chunk to write.
	counter []byte
}

// NewShadowsocksWriter creates a Writer that encrypts the given Writer using
// the shadowsocks protocol with the given shadowsocks cipher.
func NewShadowsocksWriter(writer io.Writer, ssCipher shadowaead.Cipher) Writer {
	return &shadowsocksWriter{writer: writer, ssCipher: ssCipher}
}

// init generates a random salt, sets up the AEAD object and writes
// the salt to the inner Writer.
func (sw *shadowsocksWriter) init() (err error) {
	if sw.aead == nil {
		salt := make([]byte, sw.ssCipher.SaltSize())
		if _, err := io.ReadFull(rand.Reader, salt); err != nil {
			return fmt.Errorf("failed to generate salt: %v", err)
		}
		sw.aead, err = sw.ssCipher.Encrypter(salt)
		if err != nil {
			return fmt.Errorf("failed to create AEAD: %v", err)
		}
		sw.counter = make([]byte, sw.aead.NonceSize())
		// The maximum length message is the salt (first message only), length, length tag,
		// payload, and payload tag.
		sizeBufSize := 2 + sw.aead.Overhead()
		maxPayloadBufSize := payloadSizeMask + sw.aead.Overhead()
		sw.buf = make([]byte, len(salt)+sizeBufSize+maxPayloadBufSize)
		// Store the salt at the start of sw.buf.
		copy(sw.buf, salt)
	}
	return nil
}

// encryptBlock encrypts `plaintext` in-place.  The slice must have enough capacity
// for the tag. Returns the total ciphertext length.
func (sw *shadowsocksWriter) encryptBlock(plaintext []byte) int {
	out := sw.aead.Seal(plaintext[:0], sw.counter, plaintext, nil)
	increment(sw.counter)
	return len(out)
}

// Write modes
const (
	modeNormal = iota // Normal write
	modeLazy          // Lazy write
	modeFlush         // Write during a flush
)

func (sw *shadowsocksWriter) Write(p []byte) (int, error) {
	return sw.write(p, modeNormal)
}

func (sw *shadowsocksWriter) write(p []byte, mode int) (int, error) {
	sw.byteWrapper.Reset(p)
	n, err := sw.readFrom(&sw.byteWrapper, mode)
	return int(n), err
}

func (sw *shadowsocksWriter) LazyWrite(p []byte) (int, error) {
	sw.flush = sync.Once{} // Reset flush action.
	return sw.write(p, modeLazy)
}

func (sw *shadowsocksWriter) Flush() error {
	var err error
	sw.flush.Do(func() {
		_, err = sw.write(nil, modeFlush)
	})
	return err
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func (sw *shadowsocksWriter) ReadFrom(r io.Reader) (int64, error) {
	return sw.readFrom(r, modeNormal)
}

func (sw *shadowsocksWriter) readFrom(r io.Reader, mode int) (int64, error) {
	if err := sw.init(); err != nil {
		return 0, err
	}

	if mode == modeNormal {
		// Prevent a concurrent or subsequent flush.  A normal write will
		// automatically flush any pending data, and a concurrent flush
		// would create a race condition.
		sw.flush.Do(func() {})
	}

	var written int64

	// sw.buf starts with the salt.
	saltSize := sw.ssCipher.SaltSize()
	// Normally we ignore the salt at the beginning of sw.buf.
	start := saltSize
	if isZero(sw.counter) {
		// For the first message, include the salt.  Compared to writing the salt
		// separately, this saves one packet during TCP slow-start and potentially
		// avoids having a distinctive size for the first packet.
		start = 0
	}

	// Each Shadowsocks-TCP message consists of a fixed-length size block, followed by
	// a variable-length payload block.
	sizeBuf := sw.buf[saltSize : saltSize+2+sw.aead.Overhead()]
	payloadBuf := sw.buf[saltSize+len(sizeBuf):]
	for {
		plaintextSize, err := r.Read(payloadBuf[sw.pending:payloadSizeMask])
		sw.pending += plaintextSize
		written += int64(plaintextSize) // Buffered data counts as written
		if sw.pending == payloadSizeMask || (mode != modeLazy && sw.pending > 0) {
			binary.BigEndian.PutUint16(sizeBuf, uint16(sw.pending))
			sw.encryptBlock(sizeBuf[:2])
			payloadSize := sw.encryptBlock(payloadBuf[:sw.pending])
			_, err = sw.writer.Write(sw.buf[start : saltSize+len(sizeBuf)+payloadSize])
			sw.pending = 0
			start = saltSize // Skip the salt for all writes except the first.
		}
		if err != nil {
			if err == io.EOF { // ignore EOF as per io.ReaderFrom contract
				return written, nil
			}
			return written, fmt.Errorf("Failed to read payload: %v", err)
		}
	}
}

// ChunkReader is similar to io.Reader, except that it controls its own
// buffer granularity.
type ChunkReader interface {
	// ReadChunk reads the next chunk and returns its payload.  The caller must
	// complete its use of the returned buffer before the next call.
	// The buffer is nil iff there is an error.  io.EOF indicates a close.
	ReadChunk() ([]byte, error)
}

type chunkReader struct {
	reader   io.Reader
	ssCipher shadowaead.Cipher
	// These are lazily initialized:
	aead cipher.AEAD
	// Index of the next encrypted chunk to read.
	counter []byte
	buf     []byte
}

// Reader is an io.Reader that also implements io.WriterTo to
// allow for piping the data without extra allocations and copies.
type Reader interface {
	io.Reader
	io.WriterTo
}

// NewShadowsocksReader creates a Reader that decrypts the given Reader using
// the shadowsocks protocol with the given shadowsocks cipher.
func NewShadowsocksReader(reader io.Reader, ssCipher shadowaead.Cipher) Reader {
	return &readConverter{
		cr: &chunkReader{reader: reader, ssCipher: ssCipher},
	}
}

// init reads the salt from the inner Reader and sets up the AEAD object
func (cr *chunkReader) init() (err error) {
	if cr.aead == nil {
		// For chacha20-poly1305, SaltSize is 32, NonceSize is 12 and Overhead is 16.
		salt := make([]byte, cr.ssCipher.SaltSize())
		if _, err := io.ReadFull(cr.reader, salt); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				err = fmt.Errorf("failed to read salt: %v", err)
			}
			return err
		}
		cr.aead, err = cr.ssCipher.Decrypter(salt)
		if err != nil {
			return fmt.Errorf("failed to create AEAD: %v", err)
		}
		cr.counter = make([]byte, cr.aead.NonceSize())
		cr.buf = make([]byte, payloadSizeMask+cr.aead.Overhead())
	}
	return nil
}

// readMessage reads, decrypts, and verifies a single AEAD ciphertext.
// The ciphertext and tag (i.e. "overhead") must exactly fill `buf`,
// and the decrypted message will be placed in buf[:len(buf)-overhead].
// Returns an error only if the block could not be read.
func (cr *chunkReader) readMessage(buf []byte) error {
	_, err := io.ReadFull(cr.reader, buf)
	if err != nil {
		return err
	}
	_, err = cr.aead.Open(buf[:0], cr.counter, buf, nil)
	increment(cr.counter)
	if err != nil {
		return fmt.Errorf("failed to decrypt: %v", err)
	}
	return nil
}

func (cr *chunkReader) ReadChunk() ([]byte, error) {
	if err := cr.init(); err != nil {
		return nil, err
	}
	// In Shadowsocks-AEAD, each chunk consists of two
	// encrypted messages.  The first message contains the payload length,
	// and the second message is the payload.
	sizeBuf := cr.buf[:2+cr.aead.Overhead()]
	if err := cr.readMessage(sizeBuf); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			err = fmt.Errorf("failed to read payload size: %v", err)
		}
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(sizeBuf) & payloadSizeMask)
	sizeWithTag := size + cr.aead.Overhead()
	if cap(cr.buf) < sizeWithTag {
		// This code is unreachable.
		return nil, io.ErrShortBuffer
	}
	payloadBuf := cr.buf[:sizeWithTag]
	if err := cr.readMessage(payloadBuf); err != nil {
		if err == io.EOF { // EOF is not expected mid-chunk.
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payloadBuf[:size], nil
}

// readConverter adapts from ChunkReader, with source-controlled
// chunk sizes, to Go-style IO.
type readConverter struct {
	cr       ChunkReader
	leftover []byte
}

func (c *readConverter) Read(b []byte) (int, error) {
	if err := c.ensureLeftover(); err != nil {
		return 0, err
	}
	n := copy(b, c.leftover)
	c.leftover = c.leftover[n:]
	return n, nil
}

func (c *readConverter) WriteTo(w io.Writer) (written int64, err error) {
	for {
		if err = c.ensureLeftover(); err != nil {
			if err == io.EOF {
				err = nil
			}
			return written, err
		}
		n, err := w.Write(c.leftover)
		written += int64(n)
		c.leftover = c.leftover[n:]
		if err != nil {
			return written, err
		}
	}
}

// Ensures that c.leftover is nonempty.  If leftover is empty, this method
// waits for incoming data and decrypts it.
// Returns an error only if c.leftover could not be populated.
func (c *readConverter) ensureLeftover() error {
	if len(c.leftover) > 0 {
		return nil
	}
	payload, err := c.cr.ReadChunk()
	if err != nil {
		return err
	}
	c.leftover = payload
	return nil
}

// increment little-endian encoded unsigned integer b. Wrap around on overflow.
func increment(b []byte) {
	for i := range b {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}
