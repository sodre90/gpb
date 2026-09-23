package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// maxScannedBody bounds how much of a served file is read into memory to look inside it. Stills
// and their embedded clips are tens of megabytes; a half-gigabyte video has nothing inside it
// this looks for, so it is hashed and named but not scanned.
const maxScannedBody = 256 << 20

// Body is what a served file turned out to be, judged from its own bytes rather than from what
// the headers called it.
type Body struct {
	Kind   string
	SHA256 string
	Size   int64

	DeclaresMotion      bool
	DeclaredVideoLength int64
	CarriesVideo        bool

	ZipEntries []ZipEntry
}

type ZipEntry struct {
	Name string
	Size uint64
}

func inspectFile(path string) (Body, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Body{}, err
	}
	digest, err := sha256OfFile(path)
	if err != nil {
		return Body{}, err
	}
	body := Body{Size: info.Size(), SHA256: digest}
	if info.Size() > maxScannedBody {
		body.Kind = "too large to look inside"
		return body, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Body{}, err
	}
	body.Kind = kindOf(content)

	switch body.Kind {
	case "ZIP":
		body.ZipEntries, err = zipEntries(path)
	case "JPEG", "HEIC":
		body.DeclaresMotion, body.DeclaredVideoLength = declaredMotionVideo(content)
		body.CarriesVideo = carriesEmbeddedVideo(content)
	}
	return body, err
}

func sha256OfFile(path string) (string, error) {
	content, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer content.Close()
	digest := sha256.New()
	if _, err := content.WriteTo(digest); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func kindOf(content []byte) string {
	switch {
	case bytes.HasPrefix(content, []byte{0xFF, 0xD8, 0xFF}):
		return "JPEG"
	case bytes.HasPrefix(content, []byte("PK\x03\x04")):
		return "ZIP"
	case bytes.HasPrefix(content, []byte("\x89PNG")):
		return "PNG"
	case bytes.HasPrefix(content, []byte("GIF8")):
		return "GIF"
	case len(content) >= 12 && string(content[4:8]) == "ftyp":
		return isoBrandKind(string(content[8:12]))
	default:
		return "unrecognised"
	}
}

func isoBrandKind(brand string) string {
	switch brand {
	case "heic", "heix", "mif1", "msf1", "heim", "heis":
		return "HEIC"
	case "qt  ":
		return "QuickTime"
	default:
		return "MP4 (" + strings.TrimSpace(brand) + ")"
	}
}

func zipEntries(path string) ([]ZipEntry, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("reading the zip: %w", err)
	}
	defer archive.Close()

	entries := make([]ZipEntry, 0, len(archive.File))
	for _, file := range archive.File {
		entries = append(entries, ZipEntry{Name: file.Name, Size: file.UncompressedSize64})
	}
	return entries, nil
}

var motionPhotoFlag = regexp.MustCompile(`GCamera:(MotionPhoto|MicroVideo)="1"`)

var itemLength = regexp.MustCompile(`Item:Length="(\d+)"`)

var microVideoOffset = regexp.MustCompile(`GCamera:MicroVideoOffset="(\d+)"`)

// declaredMotionVideo reads how long the file says its appended video is: the Container:Item
// marked MotionPhoto in the v1 format, or the older MicroVideoOffset, which counts the same
// bytes from the end of the file.
func declaredMotionVideo(content []byte) (bool, int64) {
	if !motionPhotoFlag.Match(content) {
		return false, 0
	}
	if element := elementAround(content, []byte(`Item:Semantic="MotionPhoto"`)); element != nil {
		if length := itemLength.FindSubmatch(element); length != nil {
			return true, parseLength(length[1])
		}
	}
	if offset := microVideoOffset.FindSubmatch(content); offset != nil {
		return true, parseLength(offset[1])
	}
	return true, 0
}

// elementAround returns the XML start tag holding an attribute. Phones write the attributes of
// a container item in different orders, so the length has to be looked for in the whole tag.
func elementAround(content, attribute []byte) []byte {
	at := bytes.Index(content, attribute)
	if at < 0 {
		return nil
	}
	start := bytes.LastIndexByte(content[:at], '<')
	end := bytes.IndexByte(content[at:], '>')
	if start < 0 || end < 0 {
		return nil
	}
	return content[start : at+end+1]
}

func parseLength(digits []byte) int64 {
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// carriesEmbeddedVideo looks for an MP4 file-type box past the file's own header. A HEIC opens
// with one of its own, so only a second one counts.
func carriesEmbeddedVideo(content []byte) bool {
	const pastOwnHeader = 16
	if len(content) <= pastOwnHeader {
		return false
	}
	for _, brand := range []string{"ftypmp4", "ftypisom", "ftypmp42", "ftypqt"} {
		if bytes.Contains(content[pastOwnHeader:], []byte(brand)) {
			return true
		}
	}
	return false
}
