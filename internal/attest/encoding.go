package attest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// b64 is a protobuf-JSON bytes field: a base64 string, standard or URL
// alphabet, with or without padding.
type b64 []byte

func (b *b64) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	out, err := decodeBase64(s)
	if err != nil {
		return err
	}
	*b = out
	return nil
}

func decodeBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("base64 with a line break")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if out, err := enc.Strict().DecodeString(s); err == nil {
			return out, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// int64s is a protobuf-JSON int64 field: a decimal string or a number.
type int64s int64

func (n *int64s) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid int64 %s", data)
	}
	*n = int64s(v)
	return nil
}

const maxJSONDepth = 64

// checkJSON checks that data is one JSON value, nested at most
// maxJSONDepth deep, with no object that has two keys that encoding/json
// would take for the same field. encoding/json keeps the last of two
// equal keys and matches keys case-insensitively, so a document with such
// keys could mean different things to different parsers.
func checkJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	type frame struct {
		obj       bool
		expectKey bool
		keys      map[string]bool
	}
	var stack []*frame
	values := 0
	valueDone := func() {
		if len(stack) == 0 {
			values++
			return
		}
		if top := stack[len(stack)-1]; top.obj {
			top.expectKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if len(stack) == 0 && values > 0 {
			return errors.New("more than one JSON value")
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				if len(stack) > 0 {
					if top := stack[len(stack)-1]; top.obj {
						top.expectKey = false
					}
				}
				if len(stack) >= maxJSONDepth {
					return errors.New("JSON nested too deep")
				}
				stack = append(stack, &frame{obj: t == '{', expectKey: t == '{', keys: map[string]bool{}})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		default:
			if len(stack) > 0 {
				if top := stack[len(stack)-1]; top.obj && top.expectKey {
					k, _ := t.(string)
					fold := strings.ToLower(strings.ToUpper(k))
					if top.keys[fold] {
						return fmt.Errorf("duplicate JSON key %q", k)
					}
					top.keys[fold] = true
					top.expectKey = false
					continue
				}
			}
			valueDone()
		}
	}
	if values != 1 || len(stack) != 0 {
		return errors.New("not one JSON value")
	}
	return nil
}

// decodeSnappy decodes the snappy block format, which GitHub uses for
// bundles at bundle_url, into at most limit bytes.
func decodeSnappy(src []byte, limit int) ([]byte, error) {
	length64, k := binary.Uvarint(src)
	if k <= 0 || limit < 0 || length64 > uint64(limit) {
		return nil, fmt.Errorf("snappy: bad length or more than %d bytes", limit)
	}
	n := int(min(length64, math.MaxInt))
	src = src[k:]
	dst := make([]byte, 0, n)
	for len(src) > 0 {
		tag := src[0]
		var length, offset int
		switch tag & 3 {
		case 0:
			l := int(tag >> 2)
			src = src[1:]
			if l >= 60 {
				nb := l - 59
				if len(src) < nb {
					return nil, errors.New("snappy: truncated literal length")
				}
				l = 0
				for i := nb - 1; i >= 0; i-- {
					l = l<<8 | int(src[i])
				}
				src = src[nb:]
			}
			l++
			if l <= 0 || l > len(src) || len(dst)+l > n {
				return nil, errors.New("snappy: bad literal")
			}
			dst = append(dst, src[:l]...)
			src = src[l:]
			continue
		case 1:
			if len(src) < 2 {
				return nil, errors.New("snappy: truncated copy")
			}
			length = 4 + int(tag>>2)&7
			offset = int(tag&0xe0)<<3 | int(src[1])
			src = src[2:]
		case 2:
			if len(src) < 3 {
				return nil, errors.New("snappy: truncated copy")
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint16(src[1:3]))
			src = src[3:]
		default:
			if len(src) < 5 {
				return nil, errors.New("snappy: truncated copy")
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint32(src[1:5]))
			src = src[5:]
		}
		if offset <= 0 || offset > len(dst) || len(dst)+length > n {
			return nil, errors.New("snappy: bad copy")
		}
		start := len(dst) - offset
		for i := range length {
			dst = append(dst, dst[start+i])
		}
	}
	if len(dst) != n {
		return nil, errors.New("snappy: length mismatch")
	}
	return dst, nil
}
