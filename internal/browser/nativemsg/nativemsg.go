// Package nativemsg reads and writes WebExtension native messaging frames:
// a 32-bit length in native byte order (little-endian on every platform
// talaria supports) followed by that many bytes of UTF-8 JSON.
package nativemsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxToBrowser is the largest message Firefox accepts from a native host.
const MaxToBrowser = 1 << 20

// MaxFromBrowser is the largest message this package reads. Firefox allows
// up to 4 GiB towards the host; screenshots are the largest real messages.
const MaxFromBrowser = 64 << 20

// ErrTooLarge is returned for a frame over the limit.
var ErrTooLarge = errors.New("native message too large")

// Read reads one frame of at most limit bytes.
func Read(r io.Reader, limit int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if int64(n) > int64(limit) {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, n, limit)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// Write writes msg as one frame of at most limit bytes.
func Write(w io.Writer, msg []byte, limit int) error {
	if len(msg) > limit {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(msg), limit)
	}
	buf := make([]byte, 4+len(msg))
	binary.LittleEndian.PutUint32(buf, uint32(len(msg))) //nolint:gosec // bounded by limit above
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	return err
}
