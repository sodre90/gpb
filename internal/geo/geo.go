// Package geo reads where a photograph was taken out of the file itself. Google's listing says
// nothing about place, and the originals in the pool carry their GPS tags untouched — so the
// pool is the one source there is, and it is a source that outlives Google's interest in the
// item.
//
// Every reader here works from the file's own structure rather than its name: a JPEG's APP1
// segment, a TIFF's IFDs, the Exif item of a HEIC, the location string in a QuickTime or
// MP4 movie, the EXIF chunk of a WebP, the eXIf chunk of a PNG. They read headers and seek
// past bodies, because a movie's index usually sits behind half a gigabyte of frames.
package geo

import (
	"bytes"
	"encoding/binary"
	"io"
)

// Coordinates is a place on the earth, in signed decimal degrees.
type Coordinates struct {
	Latitude  float64
	Longitude float64
}

// Read returns where the file says it was taken. found is false when the file carries no
// location, and also when what it carries is unusable — a truncated tag, a 0°,0° a phone
// writes with its GPS off — because for the purpose of finding a photo those are the same
// answer. The error is for the file being unreadable, which is a different thing: an answer
// not yet given rather than an answer of nothing.
func Read(r io.ReadSeeker) (coordinates Coordinates, found bool, err error) {
	var head [12]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Coordinates{}, false, nil
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Coordinates{}, false, err
	}

	var tiff []byte
	switch {
	case bytes.HasPrefix(head[:], []byte{0xFF, 0xD8}):
		tiff, err = exifOfJPEG(r)
	case bytes.HasPrefix(head[:], []byte("II*\x00")) || bytes.HasPrefix(head[:], []byte("MM\x00*")):
		tiff, err = readUpTo(r, tiffReadLimit)
	case bytes.HasPrefix(head[:], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		tiff, err = exifOfWebP(r)
	case bytes.HasPrefix(head[:], []byte("\x89PNG\r\n\x1a\n")):
		tiff, err = exifOfPNG(r)
	case bytes.Equal(head[4:8], []byte("ftyp")):
		return readISOBMFF(r)
	default:
		return Coordinates{}, false, nil
	}
	if err != nil {
		return Coordinates{}, false, err
	}
	return fromTIFF(tiff)
}

// tiffReadLimit bounds what is read of a bare TIFF or DNG: the IFDs sit at the front and the
// image data behind them, and a raw file is tens of megabytes.
const tiffReadLimit = 4 << 20

func readUpTo(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ---- JPEG: the Exif lives in an APP1 segment near the front ----

func exifOfJPEG(r io.ReadSeeker) ([]byte, error) {
	if _, err := r.Seek(2, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		var marker [2]byte
		if _, err := io.ReadFull(r, marker[:]); err != nil {
			return nil, nil
		}
		if marker[0] != 0xFF {
			return nil, nil
		}
		switch marker[1] {
		case 0xFF:
			// Padding before a marker; step back one byte and read the marker again.
			if _, err := r.Seek(-1, io.SeekCurrent); err != nil {
				return nil, err
			}
			continue
		case 0xD8, 0x01, 0xD0, 0xD1, 0xD2, 0xD3, 0xD4, 0xD5, 0xD6, 0xD7:
			continue // markers without a payload
		case 0xD9, 0xDA:
			return nil, nil // the image data begins; no Exif came before it
		}

		var size [2]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return nil, nil
		}
		length := int64(binary.BigEndian.Uint16(size[:])) - 2
		if length < 0 {
			return nil, nil
		}
		if marker[1] == 0xE1 {
			payload, err := readUpTo(r, length)
			if err != nil {
				return nil, err
			}
			if bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				return payload[6:], nil
			}
			continue
		}
		if _, err := r.Seek(length, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
}

// ---- WebP: a RIFF file whose EXIF chunk is a TIFF, with or without the Exif prefix ----

func exifOfWebP(r io.ReadSeeker) ([]byte, error) {
	if _, err := r.Seek(12, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, nil
		}
		length := int64(binary.LittleEndian.Uint32(header[4:]))
		if bytes.Equal(header[:4], []byte("EXIF")) {
			payload, err := readUpTo(r, length)
			if err != nil {
				return nil, err
			}
			return bytes.TrimPrefix(payload, []byte("Exif\x00\x00")), nil
		}
		if _, err := r.Seek(length+length%2, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
}

// ---- PNG: an eXIf chunk, which is a bare TIFF ----

func exifOfPNG(r io.ReadSeeker) ([]byte, error) {
	if _, err := r.Seek(8, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		var header [8]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, nil
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		kind := header[4:8]
		if bytes.Equal(kind, []byte("eXIf")) {
			return readUpTo(r, length)
		}
		if bytes.Equal(kind, []byte("IEND")) || bytes.Equal(kind, []byte("IDAT")) {
			return nil, nil
		}
		if _, err := r.Seek(length+4, io.SeekCurrent); err != nil { // the body and its CRC
			return nil, err
		}
	}
}

// ---- TIFF: IFD0 points at the GPS IFD, which holds the four tags that matter ----

const (
	tagGPSInfo      = 0x8825
	tagLatitudeRef  = 1
	tagLatitude     = 2
	tagLongitudeRef = 3
	tagLongitude    = 4
)

type tiffReader struct {
	data  []byte
	order binary.ByteOrder
}

func fromTIFF(data []byte) (Coordinates, bool, error) {
	if len(data) < 8 {
		return Coordinates{}, false, nil
	}
	tiff := tiffReader{data: data}
	switch string(data[:2]) {
	case "II":
		tiff.order = binary.LittleEndian
	case "MM":
		tiff.order = binary.BigEndian
	default:
		return Coordinates{}, false, nil
	}
	if tiff.order.Uint16(data[2:4]) != 42 {
		return Coordinates{}, false, nil
	}

	gpsIFD, ok := tiff.pointerIn(tiff.order.Uint32(data[4:8]), tagGPSInfo)
	if !ok {
		return Coordinates{}, false, nil
	}
	latitude, ok := tiff.angleIn(gpsIFD, tagLatitude, tagLatitudeRef, "S")
	if !ok {
		return Coordinates{}, false, nil
	}
	longitude, ok := tiff.angleIn(gpsIFD, tagLongitude, tagLongitudeRef, "W")
	if !ok {
		return Coordinates{}, false, nil
	}
	return plausible(Coordinates{Latitude: latitude, Longitude: longitude})
}

// entry is one IFD entry: what tag, what type, how many, and where the value is — inline in
// the entry when it fits in four bytes, at an offset otherwise.
type entry struct {
	tag, kind uint16
	count     uint32
	value     []byte
}

func (t tiffReader) entries(ifd uint32) []entry {
	start := int(ifd)
	if start < 0 || start+2 > len(t.data) {
		return nil
	}
	count := int(t.order.Uint16(t.data[start:]))
	var entries []entry
	for i := range count {
		at := start + 2 + i*12
		if at+12 > len(t.data) {
			return entries
		}
		field := entry{
			tag:   t.order.Uint16(t.data[at:]),
			kind:  t.order.Uint16(t.data[at+2:]),
			count: t.order.Uint32(t.data[at+4:]),
		}
		size := typeSizes[field.kind] * int(field.count)
		if size <= 4 {
			field.value = t.data[at+8 : at+12]
		} else {
			offset := int(t.order.Uint32(t.data[at+8:]))
			if offset < 0 || offset+size > len(t.data) || size < 0 {
				continue
			}
			field.value = t.data[offset : offset+size]
		}
		entries = append(entries, field)
	}
	return entries
}

var typeSizes = map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}

func (t tiffReader) find(ifd uint32, tag uint16) (entry, bool) {
	for _, field := range t.entries(ifd) {
		if field.tag == tag {
			return field, true
		}
	}
	return entry{}, false
}

func (t tiffReader) pointerIn(ifd uint32, tag uint16) (uint32, bool) {
	field, ok := t.find(ifd, tag)
	if !ok || field.kind != 4 || field.count != 1 {
		return 0, false
	}
	return t.order.Uint32(field.value), true
}

// angleIn reads a degrees-minutes-seconds triple and its hemisphere, negated when the
// hemisphere is the one named.
func (t tiffReader) angleIn(ifd uint32, tag, refTag uint16, negativeRef string) (float64, bool) {
	field, ok := t.find(ifd, tag)
	if !ok || field.kind != 5 || field.count < 3 || len(field.value) < 24 {
		return 0, false
	}
	var parts [3]float64
	for i := range parts {
		numerator := t.order.Uint32(field.value[i*8:])
		denominator := t.order.Uint32(field.value[i*8+4:])
		if denominator == 0 {
			if numerator == 0 {
				continue
			}
			return 0, false
		}
		parts[i] = float64(numerator) / float64(denominator)
	}
	angle := parts[0] + parts[1]/60 + parts[2]/3600

	if ref, ok := t.find(ifd, refTag); ok && len(ref.value) > 0 && string(ref.value[:1]) == negativeRef {
		angle = -angle
	}
	return angle, true
}

// plausible refuses a location that cannot be one: a coordinate off the globe, or the
// 0°,0° in the Gulf of Guinea that a phone with its GPS off writes instead of nothing.
func plausible(c Coordinates) (Coordinates, bool, error) {
	if c.Latitude == 0 && c.Longitude == 0 {
		return Coordinates{}, false, nil
	}
	if c.Latitude < -90 || c.Latitude > 90 || c.Longitude < -180 || c.Longitude > 180 {
		return Coordinates{}, false, nil
	}
	return c, true, nil
}
