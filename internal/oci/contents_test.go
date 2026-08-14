package oci

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Reading the files inside a layer.
//
// This is OCI, not a vendor's convention: the image specification defines a
// layer as a tar archive — the media types say `tar` and `tar+gzip` out loud —
// and defines the whiteout entries that mark deletions. The tests are about the
// format, not about anybody's bundle.

func TestATarLayerYieldsItsFiles(t *testing.T) {
	body := tarOf(map[string]string{
		"CONFIGURATION/example_parameters.json": `{"replicas":3}`,
		"DOCUMENTATION/readme":                  "how to deploy",
	})

	files, isArchive, err := ReadFiles(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !isArchive {
		t.Fatal("a tar was not recognised as an archive")
	}
	if len(files) != 2 {
		t.Fatalf("read %d files, want 2: %+v", len(files), files)
	}
	byPath := index(files)
	if _, ok := byPath["CONFIGURATION/example_parameters.json"]; !ok {
		t.Errorf("the configuration file is missing: %+v", files)
	}
}

// Gzipped is the common case — `+gzip` is in the media type — and it must be
// transparent.
func TestAGzippedTarIsRead(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(tarOf(map[string]string{"a/b.json": "{}"})); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	files, isArchive, err := ReadFiles(&buf, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !isArchive || len(files) != 1 {
		t.Fatalf("read %d files from a gzipped tar (archive=%v)", len(files), isArchive)
	}
}

// A vendor shipping configuration as a zip inside an OCI artifact is ordinary.
// The format is self-identifying, so recognising it costs nothing.
func TestAZipLayerIsRead(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("CONFIGURATION/site.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"site":"example"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	files, isArchive, err := ReadFiles(&buf, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !isArchive || len(files) != 1 {
		t.Fatalf("read %d files from a zip (archive=%v)", len(files), isArchive)
	}
	if files[0].Path != "CONFIGURATION/site.json" {
		t.Errorf("path = %q", files[0].Path)
	}
}

// THE FORMAT IS SNIFFED, NOT TAKEN FROM THE MEDIA TYPE — because the media type
// is routinely wrong, and refusing to look would disable this exactly where it
// is most wanted. Nothing here is told what the bytes are supposed to be.
func TestTheFormatIsRecognisedWithoutBeingDeclared(t *testing.T) {
	// A tar in the v7 format, with no `ustar` magic — which plenty of real
	// archives use, and which a magic-string check would miss.
	body := asV7(tarOf(map[string]string{"plain.txt": "content"}))

	files, isArchive, err := ReadFiles(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !isArchive || len(files) != 1 {
		t.Errorf("a tar without the ustar magic was not recognised (archive=%v, %d files)",
			isArchive, len(files))
	}
}

// It FAILS CLOSED. Anything unrecognised is one opaque blob, which is what the
// caller assumed before asking — not an error, and not a guess.
func TestAnythingElseIsOpaque(t *testing.T) {
	for _, body := range []string{
		`{"just":"json"}`,
		"\x89PNG\r\n\x1a\n and some binary",
		"",
	} {
		files, isArchive, err := ReadFiles(strings.NewReader(body), 1<<20)
		if err != nil {
			t.Errorf("reading %q errored rather than reporting it opaque: %v", body, err)
		}
		if isArchive || len(files) != 0 {
			t.Errorf("%q was read as an archive of %d files", body, len(files))
		}
	}
}

// A blob past the caller's budget is not opened. That budget is what stops a
// comparison downloading a two-gigabyte image layer to answer a question about
// a four-kilobyte configuration bundle.
func TestABlobPastTheBudgetIsNotOpened(t *testing.T) {
	body := tarOf(map[string]string{"a": strings.Repeat("x", 4096)})

	files, isArchive, err := ReadFiles(bytes.NewReader(body), 64)
	if err != nil {
		t.Fatal(err)
	}
	if isArchive || len(files) != 0 {
		t.Errorf("a blob past the budget was read anyway: %d files", len(files))
	}
}

// A small gzip stream can expand to gigabytes, and this reads archives fetched
// from a registry we are in the middle of doubting. The bound is lowered here
// rather than the fixture being made half a gigabyte: it is the same branch.
func TestADecompressionBombIsRefused(t *testing.T) {
	restore := maxExpandedBytes
	maxExpandedBytes = 4096
	t.Cleanup(func() { maxExpandedBytes = restore })

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 64<<10 {
		t.Fatalf("the fixture is %d bytes; it is not testing what it claims", buf.Len())
	}

	_, _, err := ReadFiles(&buf, 8<<20)
	if err == nil {
		t.Fatal("a decompression bomb was expanded without complaint")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// An entry that DECLARES an absurd size is refused before its bytes are read,
// which is what makes the guard cheap as well as correct.
func TestAnAbsurdlyLargeEntryIsRefusedBeforeItIsRead(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "huge.bin", Size: 600 << 20, Mode: 0o644, Format: tar.FormatGNU,
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT written: the header alone is what the guard reads.
	body := buf.Bytes()

	_, _, err := ReadFiles(bytes.NewReader(body), 1<<20)
	if err == nil {
		t.Fatal("an entry claiming 600 MiB was accepted")
	}
}

// Content, not metadata. Two archives of the same files differ in modification
// times and packing order every time they are built, and a comparison keyed on
// anything but the bytes would report a rebuild as a change to every file.
func TestTheDigestIsOfTheContentAlone(t *testing.T) {
	first := tarWithTimes(map[string]string{"a.json": "{}"}, 1000)
	second := tarWithTimes(map[string]string{"a.json": "{}"}, 999999)

	a, _, err := ReadFiles(bytes.NewReader(first), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := ReadFiles(bytes.NewReader(second), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Digest != b[0].Digest {
		t.Errorf("the same file packed at two times hashes differently: %s vs %s",
			a[0].Digest, b[0].Digest)
	}
}

// The OCI whiteout convention. Left alone it would appear as a file called
// `.wh.thing` being ADDED, which is the opposite of what happened.
func TestAWhiteoutIsADeletionRatherThanAFile(t *testing.T) {
	body := tarOf(map[string]string{"etc/.wh.removed.conf": ""})

	files, _, err := ReadFiles(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("read %d entries, want 1", len(files))
	}
	if !files[0].Whiteout {
		t.Error("a whiteout entry was not recognised as a deletion")
	}
	if files[0].Path != "etc/removed.conf" {
		t.Errorf("path = %q, want the path being deleted", files[0].Path)
	}
}

// Paths are cleaned, so the same file packed by two different tools compares
// equal. `./a/b` and `a/b` are the same file.
func TestPathsAreNormalised(t *testing.T) {
	body := tarOf(map[string]string{"./CONFIGURATION/one.json": "{}"})

	files, _, err := ReadFiles(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Path != "CONFIGURATION/one.json" {
		t.Errorf("path = %q, want it cleaned", files[0].Path)
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func tarOf(files map[string]string) []byte { return tarWithTimes(files, 0) }

func tarWithTimes(files map[string]string, unix int64) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		header := &tar.Header{
			Name: name, Size: int64(len(content)), Mode: 0o644, Format: tar.FormatGNU,
		}
		if unix > 0 {
			header.ModTime = time.Unix(unix, 0).UTC()
		}
		if err := tw.WriteHeader(header); err != nil {
			panic(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// asV7 blanks the `ustar` magic and repairs the header checksum, producing the
// original tar format — which has no magic at all, and which a magic-string
// check would miss.
func asV7(body []byte) []byte {
	out := append([]byte(nil), body...)
	for i := 257; i < 265; i++ {
		out[i] = 0
	}

	var sum int64
	for i := range 512 {
		b := out[i]
		if i >= 148 && i < 156 {
			b = ' '
		}
		sum += int64(b)
	}
	copy(out[148:156], []byte(fmt.Sprintf("%06o\x00 ", sum)))
	return out
}

func index(files []File) map[string]File {
	out := make(map[string]File, len(files))
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}
