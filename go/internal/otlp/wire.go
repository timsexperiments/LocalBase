// Package otlp exports traces and logs over OTLP/HTTP protobuf, the only
// protocol TS supports, without the OpenTelemetry SDK: the few messages it
// needs are encoded by hand (opentelemetry-proto v1, the fields the JS
// transformer writes).
package otlp

import (
	"encoding/binary"
	"errors"
	"math"
)

// buf is a protobuf message being written.
type buf []byte

func (b *buf) varint(v uint64) {
	for v >= 0x80 {
		*b = append(*b, byte(v)|0x80)
		v >>= 7
	}
	*b = append(*b, byte(v))
}

func (b *buf) tag(field int, wire int) { b.varint(uint64(field<<3 | wire)) }

// Fields are written only when set, as protobufjs does for proto3 scalars.

func (b *buf) uint(field int, v uint64) {
	if v != 0 {
		b.tag(field, 0)
		b.varint(v)
	}
}

func (b *buf) int64(field int, v int64) {
	if v != 0 {
		b.tag(field, 0)
		b.varint(uint64(v))
	}
}

func (b *buf) boolean(field int, v bool) {
	if v {
		b.tag(field, 0)
		b.varint(1)
	}
}

func (b *buf) fixed64(field int, v uint64) {
	if v != 0 {
		b.tag(field, 1)
		*b = binary.LittleEndian.AppendUint64(*b, v)
	}
}

func (b *buf) fixed32(field int, v uint32) {
	if v != 0 {
		b.tag(field, 5)
		*b = binary.LittleEndian.AppendUint32(*b, v)
	}
}

func (b *buf) double(field int, v float64) {
	b.tag(field, 1)
	*b = binary.LittleEndian.AppendUint64(*b, math.Float64bits(v))
}

func (b *buf) bytes(field int, v []byte) {
	if len(v) > 0 {
		b.tag(field, 2)
		b.varint(uint64(len(v)))
		*b = append(*b, v...)
	}
}

func (b *buf) str(field int, v string) {
	if v != "" {
		b.tag(field, 2)
		b.varint(uint64(len(v)))
		*b = append(*b, v...)
	}
}

// message writes a nested message, even when it is empty.
func (b *buf) message(field int, m buf) {
	b.tag(field, 2)
	b.varint(uint64(len(m)))
	*b = append(*b, m...)
}

// Field is one decoded protobuf field.
type Field struct {
	Num   int
	Wire  int
	Value uint64 // varint, fixed64, fixed32
	Bytes []byte // length-delimited
}

// Fields decodes one message level.
func Fields(b []byte) ([]Field, error) {
	var out []Field
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad tag")
		}
		b = b[n:]
		f := Field{Num: int(key >> 3), Wire: int(key & 7)}
		switch f.Wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			f.Value, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("short fixed64")
			}
			f.Value, b = binary.LittleEndian.Uint64(b), b[8:]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("short fixed32")
			}
			f.Value, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("bad length")
			}
			f.Bytes, b = b[n:n+int(l)], b[n+int(l):]
		default:
			return nil, errors.New("unsupported wire type")
		}
		out = append(out, f)
	}
	return out, nil
}

// uintAlways writes a varint field even when it is zero, as protobufjs does
// for a field the JS transformer sets explicitly.
func (b *buf) uintAlways(field int, v uint64) {
	b.tag(field, 0)
	b.varint(v)
}
