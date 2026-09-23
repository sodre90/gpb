package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// motionXMP is the shape a 2026 Pixel writes: the video's length on a container item whose
// attributes come in the phone's own order, beside a gain map item.
const motionXMP = `<x:xmpmeta><rdf:Description GCamera:MotionPhoto="1" GCamera:MotionPhotoVersion="1">
<Container:Directory><rdf:Seq>
<rdf:li><Container:Item Item:Mime="image/jpeg" Item:Semantic="Primary"/></rdf:li>
<rdf:li><Container:Item Item:Semantic="GainMap" Item:Mime="image/jpeg" Item:Length="46885"/></rdf:li>
<rdf:li><Container:Item Item:Mime="video/mp4" Item:Semantic="MotionPhoto" Item:Length="6287310"/></rdf:li>
</rdf:Seq></Container:Directory></rdf:Description></x:xmpmeta>`

func writeFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func jpegWith(parts ...string) []byte {
	content := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	for _, part := range parts {
		content = append(content, part...)
	}
	return content
}

func TestAMotionPhotoCarryingItsVideoIsRecognised(t *testing.T) {
	path := writeFile(t, "PXL.MP.jpg", jpegWith(motionXMP, "\xFF\xD9", "\x00\x00\x00\x18ftypmp42rest-of-clip"))

	body, err := inspectFile(path)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if body.Kind != "JPEG" || !body.DeclaresMotion || !body.CarriesVideo {
		t.Fatalf("read as %+v, want a JPEG declaring and carrying its video", body)
	}
	if body.DeclaredVideoLength != 6287310 {
		t.Errorf("declared video length %d, want 6287310", body.DeclaredVideoLength)
	}
}

// The case found on the backup disk: the metadata still promises a video and the bytes stop
// at the end of the still.
func TestAMotionPhotoThatLostItsVideoSaysSo(t *testing.T) {
	path := writeFile(t, "PXL.MP.jpg", jpegWith(motionXMP, "\xFF\xD9"))

	body, err := inspectFile(path)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if !body.DeclaresMotion || body.CarriesVideo {
		t.Fatalf("read as %+v, want a declared motion photo with no video", body)
	}
	if got := describeBody(body); got != "JPEG, says it is a motion photo with a 6287310-byte video, and carries none" {
		t.Errorf("described as %q", got)
	}
}

func TestAHEICIsNotMistakenForItsOwnVideo(t *testing.T) {
	path := writeFile(t, "IMG.HEIC", []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic-body"))

	body, err := inspectFile(path)
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if body.Kind != "HEIC" || body.CarriesVideo {
		t.Errorf("read as %+v, want a HEIC with no embedded video", body)
	}
}

// A Live Photo reached the pool once as a zip of its two halves; the probe has to say what is
// inside one rather than stop at the word.
func TestAZipIsListed(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"IMG_0138.jpg", "IMG_0138.mov"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("adding %s: %v", name, err)
		}
		entry.Write([]byte("half of a live photo"))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the zip: %v", err)
	}

	body, err := inspectFile(writeFile(t, "live.zip", archive.Bytes()))
	if err != nil {
		t.Fatalf("inspecting: %v", err)
	}
	if body.Kind != "ZIP" || len(body.ZipEntries) != 2 || body.ZipEntries[1].Name != "IMG_0138.mov" {
		t.Errorf("read as %+v, want a zip listing both halves", body)
	}
}
