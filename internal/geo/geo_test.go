package geo

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"testing"
)

// Every fixture is built here rather than checked in: a real photograph's coordinates are a
// real place someone stood, and the readers work from the file's structure, which a builder
// reproduces exactly.

const (
	fuerteventuraLat = 28.4567
	fuerteventuraLon = -14.0123
)

func TestAJPEGCarriesItsPlaceInAnExifSegment(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		photo := jpegWithExif(t, tiffWithGPS(order, fuerteventuraLat, fuerteventuraLon))
		expectPlace(t, photo, fuerteventuraLat, fuerteventuraLon)
	}
}

func TestAJPEGWithoutExifHasNoPlace(t *testing.T) {
	expectNoPlace(t, plainJPEG(t))
}

func TestAJPEGWhoseExifComesAfterAnotherSegmentIsStillRead(t *testing.T) {
	comment := append([]byte{0xFF, 0xFE, 0x00, 0x08}, []byte("hello!")...)
	photo := plainJPEG(t)
	photo = append(photo[:2], append(comment, jpegWithExif(t, tiffWithGPS(binary.LittleEndian, 1.5, 2.5))[2:]...)...)
	expectPlace(t, photo, 1.5, 2.5)
}

func TestADNGIsATIFFReadDirectly(t *testing.T) {
	expectPlace(t, tiffWithGPS(binary.LittleEndian, -33.8688, 151.2093), -33.8688, 151.2093)
}

func TestAWebPCarriesItsPlaceInAnEXIFChunk(t *testing.T) {
	tiff := tiffWithGPS(binary.LittleEndian, 47.4979, 19.0402)
	expectPlace(t, webpWithExif(tiff), 47.4979, 19.0402)
	expectPlace(t, webpWithExif(append([]byte("Exif\x00\x00"), tiff...)), 47.4979, 19.0402)
}

func TestAPNGCarriesItsPlaceInAnEXIfChunk(t *testing.T) {
	expectPlace(t, pngWithExif(tiffWithGPS(binary.BigEndian, 51.5, -0.12)), 51.5, -0.12)
	expectNoPlace(t, pngWithExif(nil))
}

func TestAHEICCarriesItsPlaceAsAnExifItem(t *testing.T) {
	expectPlace(t, heicWithExif(tiffWithGPS(binary.BigEndian, 28.1, -15.4)), 28.1, -15.4)
}

func TestAnMP4CarriesItsPlaceUnderUserData(t *testing.T) {
	movie := mp4WithLocation("+28.4567-014.0123+050.000/")
	expectPlace(t, movie, fuerteventuraLat, fuerteventuraLon)
}

func TestAQuickTimeMovieCarriesItsPlaceUnderItsKeys(t *testing.T) {
	movie := quickTimeWithLocation("+28.4567-014.0123+050.000/")
	expectPlace(t, movie, fuerteventuraLat, fuerteventuraLon)
}

func TestISO6709ComesInThreePrecisions(t *testing.T) {
	for text, want := range map[string]Coordinates{
		"+28.4567-014.0123/":         {28.4567, -14.0123},
		"+2827.402-01400.738/":       {28 + 27.402/60, -(14 + 0.738/60)},
		"+282724.12-0140044.28/":     {28 + 27.0/60 + 24.12/3600, -(14 + 0.0/60 + 44.28/3600)},
		"+28.4567-014.0123+050.000/": {28.4567, -14.0123},
		"-33.8688+151.2093/":         {-33.8688, 151.2093},
	} {
		got, ok := parseISO6709(text)
		if !ok || !close(got.Latitude, want.Latitude) || !close(got.Longitude, want.Longitude) {
			t.Errorf("%q read as %+v (ok=%v), want %+v", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "/", "28.4567", "+abc-def/", "+2.5/"} {
		if _, ok := parseISO6709(text); ok {
			t.Errorf("%q read as a place", text)
		}
	}
}

// A phone with its GPS off writes 0°,0° rather than nothing. That is a point in the Gulf of
// Guinea, and no photograph in a library is from there.
func TestZeroZeroIsNotAPlace(t *testing.T) {
	expectNoPlace(t, jpegWithExif(t, tiffWithGPS(binary.LittleEndian, 0, 0)))
	expectNoPlace(t, mp4WithLocation("+00.0000+000.0000/"))
}

func TestAFileOfNoKnownKindHasNoPlace(t *testing.T) {
	expectNoPlace(t, []byte("GIF89a...."))
	expectNoPlace(t, nil)
}

// A reader that panics on a malformed file would take the sweep down with it, so every walk
// has to survive being handed rubbish under a familiar signature.
func TestRubbishUnderEverySignatureIsSurvived(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	signatures := [][]byte{
		{0xFF, 0xD8, 0xFF, 0xE1},
		[]byte("II*\x00"), []byte("MM\x00*"),
		[]byte("RIFF\x10\x00\x00\x00WEBPEXIF"),
		[]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x10eXIf"),
		[]byte("\x00\x00\x00\x18ftypheic"),
		[]byte("\x00\x00\x00\x18ftypmp42"),
	}
	for _, signature := range signatures {
		for range 200 {
			body := make([]byte, random.IntN(300))
			for i := range body {
				body[i] = byte(random.IntN(256))
			}
			Read(bytes.NewReader(append(signature, body...)))
		}
	}

	// And the fixtures themselves, truncated at every length.
	for _, whole := range [][]byte{
		jpegWithExif(t, tiffWithGPS(binary.LittleEndian, 1, 2)),
		heicWithExif(tiffWithGPS(binary.BigEndian, 1, 2)),
		mp4WithLocation("+01.0+002.0/"),
		quickTimeWithLocation("+01.0+002.0/"),
	} {
		for cut := range len(whole) {
			Read(bytes.NewReader(whole[:cut]))
		}
	}
}

func expectPlace(t *testing.T, file []byte, latitude, longitude float64) {
	t.Helper()
	got, ok, err := Read(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if !ok || !close(got.Latitude, latitude) || !close(got.Longitude, longitude) {
		t.Errorf("the file reads as %+v (found=%v), want %v, %v", got, ok, latitude, longitude)
	}
}

func expectNoPlace(t *testing.T, file []byte) {
	t.Helper()
	got, ok, err := Read(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if ok {
		t.Errorf("the file reads as %+v, want no place", got)
	}
}

func close(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

// ---- builders ----

// tiffWithGPS is a TIFF whose IFD0 holds only a pointer to a GPS IFD, and whose GPS IFD holds
// the two angles and their hemispheres.
func tiffWithGPS(order binary.ByteOrder, latitude, longitude float64) []byte {
	var out bytes.Buffer
	put := func(v any) { binary.Write(&out, order, v) }
	if order == binary.LittleEndian {
		out.WriteString("II")
	} else {
		out.WriteString("MM")
	}
	put(uint16(42))
	put(uint32(8)) // IFD0 follows the header

	// IFD0: one entry, then no next IFD.
	gpsIFD := uint32(8 + 2 + 12 + 4)
	put(uint16(1))
	put(uint16(tagGPSInfo))
	put(uint16(4))
	put(uint32(1))
	put(gpsIFD)
	put(uint32(0))

	// GPS IFD: four entries; the rationals live after the entry table.
	rationals := gpsIFD + 2 + 4*12 + 4
	put(uint16(4))
	writeASCII := func(tag uint16, ref string) {
		put(tag)
		put(uint16(2))
		put(uint32(2))
		out.WriteString(ref)
		out.Write([]byte{0, 0, 0})
	}
	writeAngle := func(tag uint16, at uint32) {
		put(tag)
		put(uint16(5))
		put(uint32(3))
		put(at)
	}
	writeASCII(tagLatitudeRef, hemisphere(latitude, "N", "S"))
	writeAngle(tagLatitude, rationals)
	writeASCII(tagLongitudeRef, hemisphere(longitude, "E", "W"))
	writeAngle(tagLongitude, rationals+24)
	put(uint32(0))

	for _, angle := range []float64{math.Abs(latitude), math.Abs(longitude)} {
		degrees := math.Floor(angle)
		minutes := math.Floor((angle - degrees) * 60)
		seconds := (angle - degrees - minutes/60) * 3600
		put(uint32(degrees))
		put(uint32(1))
		put(uint32(minutes))
		put(uint32(1))
		put(uint32(math.Round(seconds * 10000)))
		put(uint32(10000))
	}
	return out.Bytes()
}

func hemisphere(angle float64, positive, negative string) string {
	if angle < 0 {
		return negative
	}
	return positive
}

func plainJPEG(t *testing.T) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, picture, nil); err != nil {
		t.Fatalf("encoding a JPEG: %v", err)
	}
	return out.Bytes()
}

// jpegWithExif splices an APP1 segment in after the start-of-image marker, where cameras put it.
func jpegWithExif(t *testing.T, tiff []byte) []byte {
	t.Helper()
	photo := plainJPEG(t)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	segment = append(segment, payload...)
	return append(append(append([]byte{}, photo[:2]...), segment...), photo[2:]...)
}

func webpWithExif(exif []byte) []byte {
	var out bytes.Buffer
	chunk := func(kind string, body []byte) {
		out.WriteString(kind)
		binary.Write(&out, binary.LittleEndian, uint32(len(body)))
		out.Write(body)
		if len(body)%2 == 1 {
			out.WriteByte(0)
		}
	}
	out.WriteString("RIFF")
	binary.Write(&out, binary.LittleEndian, uint32(0)) // the size is not read
	out.WriteString("WEBP")
	chunk("VP8X", make([]byte, 10))
	chunk("EXIF", exif)
	return out.Bytes()
}

func pngWithExif(tiff []byte) []byte {
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, body []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(body)))
		out.WriteString(kind)
		out.Write(body)
		out.Write([]byte{0, 0, 0, 0}) // a CRC nobody checks here
	}
	chunk("IHDR", make([]byte, 13))
	if tiff != nil {
		chunk("eXIf", tiff)
	}
	chunk("IDAT", []byte{0})
	chunk("IEND", nil)
	return out.Bytes()
}

func isoBox(kind string, body ...[]byte) []byte {
	var out bytes.Buffer
	size := 8
	for _, part := range body {
		size += len(part)
	}
	binary.Write(&out, binary.BigEndian, uint32(size))
	out.WriteString(kind)
	for _, part := range body {
		out.Write(part)
	}
	return out.Bytes()
}

func fullBox(kind string, body ...[]byte) []byte {
	return isoBox(kind, append([][]byte{{0, 0, 0, 0}}, body...)...)
}

func be16(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
func be32(v int) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

// heicWithExif is the smallest HEIC shape that carries an Exif item: a meta box naming item 1
// as Exif and locating it in the mdat that follows.
func heicWithExif(tiff []byte) []byte {
	ftyp := isoBox("ftyp", []byte("heic"), be32(0), []byte("mif1heic"))
	block := append(be32(6), append([]byte("Exif\x00\x00"), tiff...)...)

	infe := isoBox("infe", []byte{2, 0, 0, 0}, be16(1), be16(0), []byte("Exif"), []byte{0})
	iinf := fullBox("iinf", be16(1), infe)
	hdlr := fullBox("hdlr", be32(0), []byte("pict"), make([]byte, 13))

	build := func(offset int) []byte {
		iloc := fullBox("iloc", []byte{0x44, 0x00}, be16(1),
			be16(1), be16(0), be16(1), be32(offset), be32(len(block)))
		meta := fullBox("meta", hdlr, iinf, iloc)
		mdat := isoBox("mdat", block)
		return append(append(ftyp, meta...), mdat...)
	}
	// The extent offset is where the block lands, which depends on the size of what precedes
	// it — the same either way, so a first build measures it.
	first := build(0)
	offset := len(first) - len(block)
	return build(offset)
}

// mp4WithLocation puts the movie's index behind a large frame box, as phones do, with the
// frame box using the 64-bit size form.
func mp4WithLocation(iso6709 string) []byte {
	ftyp := isoBox("ftyp", []byte("mp42"), be32(0), []byte("mp42isom"))
	frames := make([]byte, 5000)
	var mdat bytes.Buffer
	binary.Write(&mdat, binary.BigEndian, uint32(1))
	mdat.WriteString("mdat")
	binary.Write(&mdat, binary.BigEndian, uint64(16+len(frames)))
	mdat.Write(frames)

	xyz := isoBox("\xa9xyz", be16(len(iso6709)), be16(0x15c7), []byte(iso6709))
	moov := isoBox("moov", isoBox("mvhd", make([]byte, 100)), isoBox("udta", xyz))
	return append(append(ftyp, mdat.Bytes()...), moov...)
}

func quickTimeWithLocation(iso6709 string) []byte {
	ftyp := isoBox("ftyp", []byte("qt  "), be32(0), []byte("qt  "))
	key := isoBox("mdta", []byte("com.apple.quicktime.location.ISO6709"))
	other := isoBox("mdta", []byte("com.apple.quicktime.make"))
	keys := fullBox("keys", be32(2), other, key)
	value := isoBox("data", be32(1), be32(0), []byte(iso6709))
	ilst := isoBox("ilst", isoBox("\x00\x00\x00\x02", value))
	hdlr := fullBox("hdlr", be32(0), []byte("mdta"), make([]byte, 13))
	meta := isoBox("meta", hdlr, keys, ilst)
	moov := isoBox("moov", isoBox("mvhd", make([]byte, 100)), meta)
	return append(append(ftyp, isoBox("mdat", make([]byte, 64))...), moov...)
}
