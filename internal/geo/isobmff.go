package geo

import (
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
)

// The ISO base media file format is a tree of boxes, and it is what HEIC, MP4 and QuickTime
// files all are. A HEIC keeps its Exif as an item named in the meta box's item table; a
// movie keeps its location as an ISO 6709 string, in a ©xyz box under udta or under the
// QuickTime metadata keys. The walk reads box headers and seeks past bodies: the frames of
// a movie come first and are most of the file.

// boxReadLimit is the largest box body that is read whole. The item table of a HEIC and the
// index of a movie are kilobytes; anything larger than this is not one of those.
const boxReadLimit = 16 << 20

type box struct {
	kind   string
	start  int64 // where the header begins
	body   int64 // where the payload begins
	end    int64 // one past the last byte
	header int64
}

func readISOBMFF(r io.ReadSeeker) (Coordinates, bool, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Coordinates{}, false, err
	}
	var at int64
	for at < end {
		found, err := readBoxHeader(r, at, end)
		if err != nil {
			return Coordinates{}, false, err
		}
		if found.kind == "" {
			break
		}
		switch found.kind {
		case "meta":
			coordinates, ok, err := exifItemOfHEIC(r, found)
			if err != nil || ok {
				return coordinates, ok, err
			}
		case "moov":
			return locationOfMovie(r, found)
		}
		at = found.end
	}
	return Coordinates{}, false, nil
}

// readBoxHeader reads the box that starts at the offset. A box of size 1 carries a 64-bit
// size after its type; a box of size 0 runs to the end of the file. A kind of "" is the end.
func readBoxHeader(r io.ReadSeeker, at, end int64) (box, error) {
	if at+8 > end {
		return box{}, nil
	}
	if _, err := r.Seek(at, io.SeekStart); err != nil {
		return box{}, err
	}
	var header [16]byte
	if _, err := io.ReadFull(r, header[:8]); err != nil {
		return box{}, err
	}
	found := box{kind: string(header[4:8]), start: at, header: 8}
	size := int64(binary.BigEndian.Uint32(header[:4]))
	switch size {
	case 0:
		size = end - at
	case 1:
		if _, err := io.ReadFull(r, header[8:16]); err != nil {
			return box{}, err
		}
		size = int64(binary.BigEndian.Uint64(header[8:16]))
		found.header = 16
	}
	if size < found.header || at+size > end {
		return box{}, nil
	}
	found.body = at + found.header
	found.end = at + size
	return found, nil
}

// childrenOf lists the boxes directly inside one. skip is what precedes the first child: the
// four bytes of version and flags a full box carries, or nothing.
func childrenOf(r io.ReadSeeker, parent box, skip int64) ([]box, error) {
	var children []box
	at := parent.body + skip
	for at < parent.end {
		child, err := readBoxHeader(r, at, parent.end)
		if err != nil {
			return nil, err
		}
		if child.kind == "" {
			break
		}
		children = append(children, child)
		at = child.end
	}
	return children, nil
}

func childOf(r io.ReadSeeker, parent box, skip int64, kind string) (box, bool, error) {
	children, err := childrenOf(r, parent, skip)
	if err != nil {
		return box{}, false, err
	}
	for _, child := range children {
		if child.kind == kind {
			return child, true, nil
		}
	}
	return box{}, false, nil
}

func bodyOf(r io.ReadSeeker, found box) ([]byte, error) {
	if found.end-found.body > boxReadLimit {
		return nil, nil
	}
	if _, err := r.Seek(found.body, io.SeekStart); err != nil {
		return nil, err
	}
	return readUpTo(r, found.end-found.body)
}

// metaVersionSkip is the four bytes of version and flags a HEIC's meta box begins with. A
// QuickTime movie's meta box has no such bytes and begins straight with a hdlr box, which is
// how the two are told apart.
func metaVersionSkip(r io.ReadSeeker, meta box) (int64, error) {
	if _, err := r.Seek(meta.body, io.SeekStart); err != nil {
		return 0, err
	}
	var peek [8]byte
	if _, err := io.ReadFull(r, peek[:]); err != nil {
		return 0, nil
	}
	if string(peek[4:8]) == "hdlr" {
		return 0, nil
	}
	return 4, nil
}

// ---- HEIC: the Exif is an item, found by id in iinf and located by iloc ----

func exifItemOfHEIC(r io.ReadSeeker, meta box) (Coordinates, bool, error) {
	skip, err := metaVersionSkip(r, meta)
	if err != nil {
		return Coordinates{}, false, err
	}
	iinf, ok, err := childOf(r, meta, skip, "iinf")
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	exifItem, ok, err := exifItemID(r, iinf)
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	iloc, ok, err := childOf(r, meta, skip, "iloc")
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	offset, length, ok, err := extentOf(r, iloc, exifItem)
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	if length > boxReadLimit || length < 4 {
		return Coordinates{}, false, nil
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return Coordinates{}, false, err
	}
	block, err := readUpTo(r, length)
	if err != nil {
		return Coordinates{}, false, err
	}
	// The item is an ExifDataBlock: four bytes saying how far past them the TIFF header sits,
	// then the payload — in practice "Exif\0\0" and the TIFF at offset six.
	tiffAt := 4 + int64(binary.BigEndian.Uint32(block[:4]))
	if tiffAt < 4 || tiffAt > int64(len(block)) {
		return Coordinates{}, false, nil
	}
	return fromTIFF(block[tiffAt:])
}

// exifItemID finds the item whose type is Exif in the item information box.
func exifItemID(r io.ReadSeeker, iinf box) (uint32, bool, error) {
	body, err := bodyOf(r, iinf)
	if err != nil || len(body) < 6 {
		return 0, false, err
	}
	version := body[0]
	at := 4
	var count int
	if version == 0 {
		count = int(binary.BigEndian.Uint16(body[at:]))
		at += 2
	} else {
		count = int(binary.BigEndian.Uint32(body[at:]))
		at += 4
	}
	for range count {
		if at+8 > len(body) {
			return 0, false, nil
		}
		size := int(binary.BigEndian.Uint32(body[at:]))
		if string(body[at+4:at+8]) != "infe" || size < 12 || at+size > len(body) {
			return 0, false, nil
		}
		info := body[at+8 : at+size]
		if id, ok := exifInfoEntry(info); ok {
			return id, true, nil
		}
		at += size
	}
	return 0, false, nil
}

// exifInfoEntry reads one infe box: version 2 has a 16-bit item id, version 3 a 32-bit one,
// each followed by a protection index and the item type.
func exifInfoEntry(info []byte) (uint32, bool) {
	if len(info) < 4 {
		return 0, false
	}
	var id uint32
	var at int
	switch info[0] {
	case 2:
		if len(info) < 4+2+2+4 {
			return 0, false
		}
		id = uint32(binary.BigEndian.Uint16(info[4:]))
		at = 6
	case 3:
		if len(info) < 4+4+2+4 {
			return 0, false
		}
		id = binary.BigEndian.Uint32(info[4:])
		at = 8
	default:
		return 0, false
	}
	at += 2 // protection index
	return id, string(info[at:at+4]) == "Exif"
}

// extentOf reads the item location box for one item and returns its first extent as a file
// offset and a length. Only construction method 0 — bytes in this file — is understood,
// which is the one every camera and phone uses for Exif.
func extentOf(r io.ReadSeeker, iloc box, item uint32) (offset, length int64, ok bool, err error) {
	body, err := bodyOf(r, iloc)
	if err != nil || len(body) < 8 {
		return 0, 0, false, err
	}
	version := body[0]
	offsetSize := int(body[4] >> 4)
	lengthSize := int(body[4] & 0xF)
	baseOffsetSize := int(body[5] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(body[5] & 0xF)
	}
	cursor := &byteCursor{data: body, at: 6}
	var count uint64
	if version < 2 {
		count = cursor.uint(2)
	} else {
		count = cursor.uint(4)
	}
	for range count {
		var id uint64
		if version < 2 {
			id = cursor.uint(2)
		} else {
			id = cursor.uint(4)
		}
		method := uint64(0)
		if version == 1 || version == 2 {
			method = cursor.uint(2) & 0xF
		}
		cursor.uint(2) // data reference index
		base := cursor.uint(baseOffsetSize)
		extents := cursor.uint(2)
		for range extents {
			if indexSize > 0 {
				cursor.uint(indexSize)
			}
			extentOffset := cursor.uint(offsetSize)
			extentLength := cursor.uint(lengthSize)
			if cursor.failed {
				return 0, 0, false, nil
			}
			if uint32(id) == item && method == 0 {
				return int64(base + extentOffset), int64(extentLength), true, nil
			}
		}
		if cursor.failed {
			return 0, 0, false, nil
		}
	}
	return 0, 0, false, nil
}

// byteCursor reads big-endian unsigned integers of the widths iloc declares, and remembers
// having run off the end rather than panicking there.
type byteCursor struct {
	data   []byte
	at     int
	failed bool
}

func (c *byteCursor) uint(width int) uint64 {
	if width == 0 {
		return 0
	}
	if c.at+width > len(c.data) || width > 8 {
		c.failed = true
		return 0
	}
	var value uint64
	for _, b := range c.data[c.at : c.at+width] {
		value = value<<8 | uint64(b)
	}
	c.at += width
	return value
}

// ---- Movies: an ISO 6709 string, under udta as ©xyz or under the QuickTime keys ----

func locationOfMovie(r io.ReadSeeker, moov box) (Coordinates, bool, error) {
	if udta, ok, err := childOf(r, moov, 0, "udta"); err != nil {
		return Coordinates{}, false, err
	} else if ok {
		if xyz, ok, err := childOf(r, udta, 0, "\xa9xyz"); err != nil {
			return Coordinates{}, false, err
		} else if ok {
			body, err := bodyOf(r, xyz)
			if err != nil {
				return Coordinates{}, false, err
			}
			// Two bytes of length and two of language precede the text.
			if len(body) > 4 {
				if coordinates, ok := parseISO6709(string(body[4:])); ok {
					return plausible(coordinates)
				}
			}
		}
	}

	meta, ok, err := childOf(r, moov, 0, "meta")
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	skip, err := metaVersionSkip(r, meta)
	if err != nil {
		return Coordinates{}, false, err
	}
	text, ok, err := quickTimeKey(r, meta, skip, "com.apple.quicktime.location.ISO6709")
	if err != nil || !ok {
		return Coordinates{}, false, err
	}
	if coordinates, ok := parseISO6709(text); ok {
		return plausible(coordinates)
	}
	return Coordinates{}, false, nil
}

// quickTimeKey reads one of the movie's metadata values: the keys box names them in order,
// and the ilst box holds a value for each under that one-based position.
func quickTimeKey(r io.ReadSeeker, meta box, skip int64, name string) (string, bool, error) {
	keys, ok, err := childOf(r, meta, skip, "keys")
	if err != nil || !ok {
		return "", false, err
	}
	body, err := bodyOf(r, keys)
	if err != nil || len(body) < 8 {
		return "", false, err
	}
	count := int(binary.BigEndian.Uint32(body[4:]))
	position := 0
	at := 8
	for i := 1; i <= count; i++ {
		if at+8 > len(body) {
			return "", false, nil
		}
		size := int(binary.BigEndian.Uint32(body[at:]))
		if size < 8 || at+size > len(body) {
			return "", false, nil
		}
		if string(body[at+8:at+size]) == name {
			position = i
			break
		}
		at += size
	}
	if position == 0 {
		return "", false, nil
	}

	ilst, ok, err := childOf(r, meta, skip, "ilst")
	if err != nil || !ok {
		return "", false, err
	}
	entries, err := childrenOf(r, ilst, 0)
	if err != nil {
		return "", false, err
	}
	for _, item := range entries {
		if binary.BigEndian.Uint32([]byte(item.kind)) != uint32(position) {
			continue
		}
		data, ok, err := childOf(r, item, 0, "data")
		if err != nil || !ok {
			return "", false, err
		}
		value, err := bodyOf(r, data)
		if err != nil || len(value) < 8 {
			return "", false, err
		}
		return string(value[8:]), true, nil // past the type and locale words
	}
	return "", false, nil
}

// parseISO6709 reads the "+DD.DDDD+DDD.DDDD/" form a phone writes, in any of its three
// precisions: degrees, degrees and minutes, or degrees, minutes and seconds — told apart by
// how many digits come before the decimal point.
func parseISO6709(text string) (Coordinates, bool) {
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "/"))
	parts := signedParts(text)
	if len(parts) < 2 {
		return Coordinates{}, false
	}
	latitude, err := angleOfISO6709(parts[0], 2)
	if err != nil {
		return Coordinates{}, false
	}
	longitude, err := angleOfISO6709(parts[1], 3)
	if err != nil {
		return Coordinates{}, false
	}
	return Coordinates{Latitude: latitude, Longitude: longitude}, true
}

func signedParts(text string) []string {
	var parts []string
	start := -1
	for i, c := range text {
		if c == '+' || c == '-' {
			if start >= 0 {
				parts = append(parts, text[start:i])
			}
			start = i
		}
	}
	if start >= 0 {
		parts = append(parts, text[start:])
	}
	return parts
}

// angleOfISO6709 reads one signed number whose integer digits are degrees alone, degrees
// then two of minutes, or degrees then minutes then two of seconds.
func angleOfISO6709(part string, degreeDigits int) (float64, error) {
	if len(part) < 2 {
		return 0, errors.New("too short")
	}
	sign := 1.0
	if part[0] == '-' {
		sign = -1
	}
	number := part[1:]
	integer := number
	if dot := strings.IndexByte(number, '.'); dot >= 0 {
		integer = number[:dot]
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, err
	}
	switch len(integer) - degreeDigits {
	case 0:
		return sign * value, nil
	case 2:
		degrees := float64(int(value) / 100)
		return sign * (degrees + (value-degrees*100)/60), nil
	case 4:
		degrees := float64(int(value) / 10000)
		minutes := float64(int(value) / 100 % 100)
		seconds := value - degrees*10000 - minutes*100
		return sign * (degrees + minutes/60 + seconds/3600), nil
	}
	return 0, errors.New("not an ISO 6709 angle")
}
