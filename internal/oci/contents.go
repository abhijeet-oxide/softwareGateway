package oci

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Looking inside a layer.
//
// # This is OCI, not one vendor's convention
//
// The image specification defines a layer as a TAR ARCHIVE - the media types
// say so out loud (`application/vnd.oci.image.layer.v1.tar`, and the same with
// `+gzip`), and the "Layer Changeset" section defines the whiteout entries that
// mark deletions. Enumerating the files inside one is reading the format the
// specification names, not decoding something a vendor invented.
//
// The other shape here is equally standard: an OCI 1.1 artifact frequently
// carries ONE FILE PER LAYER with `org.opencontainers.image.title` naming it.
// That is the ORAS convention, used by Helm, by cosign and by everything else
// that publishes non-image artifacts.
//
// So a bundle whose configuration arrives as `custo.tar.gz` and a bundle whose
// configuration arrives as one titled layer per file are two standard layouts,
// and both are read here without anybody knowing whose bundle it is. What would
// have been vendor-specific - a list of "artifact types worth opening" - is
// answered instead by a byte BUDGET, which degrades correctly for a vendor
// nobody has written a plugin for.
//
// # Why the format is sniffed rather than taken from the media type
//
// Because it is routinely wrong. A vendor shipping a gzipped tar under
// `application/octet-stream`, or under a media type of their own invention, is
// ordinary - and refusing to look would disable this exactly where it is most
// wanted. Both formats read here are self-identifying, so sniffing is sound:
// gzip has a two-byte magic, tar has a checksummed header, zip has a signature.
//
// It fails CLOSED. Anything that does not parse is one opaque blob, which is
// what the caller assumed before asking.

const (
	// maxArchiveEntries bounds how many files one layer may contribute.
	//
	// A layer with a million entries is not a configuration bundle, and
	// enumerating it would turn a comparison into an out-of-memory report.
	maxArchiveEntries = 20000
	// maxEntryBytes bounds one entry, and maxExpandedBytes the whole archive.
	//
	// These are the decompression-bomb guards, and they are not optional: a
	// four-kilobyte gzip stream can expand to gigabytes, and this reads
	// archives fetched from a registry we are in the middle of DOUBTING. The
	// budget the caller sets bounds what is DOWNLOADED; these bound what that
	// download can turn into.
	maxEntryBytes     = 64 << 20
	tarHeaderSize     = 512
	sniffBytesNeeded  = 512
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = ".wh..wh..opq"
)

// maxExpandedBytes is a var rather than a const so a test can lower it.
//
// Proving that the guard fires otherwise means actually expanding half a
// gigabyte, which is a unit test nobody will keep. Lowering the bound tests the
// same branch in milliseconds.
var maxExpandedBytes int64 = 512 << 20

// File is one entry inside a layer.
type File struct {
	// Path is the entry's name, cleaned. Leading "./" is removed so the same
	// file packed by two different tools compares equal.
	Path string
	Size int64
	// Digest is the sha256 of the entry's CONTENT.
	//
	// Content, not metadata: two archives of the same files differ in
	// modification times and packing order every time they are built, and a
	// comparison keyed on anything but the bytes would report a rebuild as a
	// change to every file in it.
	Digest string
	// Whiteout marks an OCI deletion entry - the layer removing a path that a
	// lower layer provided.
	Whiteout bool
}

// ReadFiles enumerates the files inside a blob.
//
// Reports whether the blob WAS an archive. False is an ordinary answer, not a
// failure: an image layer that is a squashed filesystem, an encrypted blob, or
// a single JSON document are all things a caller should treat as one opaque
// unit, and saying so is more useful than an error.
//
// limit bounds how many bytes are read from r. A blob larger than the caller's
// budget is not an archive as far as this is concerned, because opening it
// would cost more than the answer is worth.
func ReadFiles(r io.Reader, limit int64) ([]File, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}

	// Buffered whole, because both readers need to seek or re-read the header:
	// zip needs random access by specification, and sniffing gzip means looking
	// at two bytes and then handing the same two bytes on. The limit is what
	// makes that safe.
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("read blob: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, false, nil
	}

	if files, ok := readZip(body); ok {
		return files, true, nil
	}
	return readTar(body)
}

// readTar enumerates a tar archive, transparently un-gzipping one.
func readTar(body []byte) ([]File, bool, error) {
	stream, err := decompress(body)
	if err != nil {
		// A gzip header that does not decompress is corrupt rather than
		// opaque, and saying so beats reporting the layer as one unchanged
		// blob when its contents may be anything.
		return nil, false, err
	}
	if !looksLikeTar(stream) {
		return nil, false, nil
	}

	var (
		files    []File
		expanded int64
		reader   = tar.NewReader(bytes.NewReader(stream))
	)
	for len(files) < maxArchiveEntries {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A truncated archive still tells us about the entries it did
			// yield, and those are usually the whole answer. Reporting nothing
			// because the last header was short would throw them away.
			break
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}

		expanded += header.Size
		if header.Size > maxEntryBytes || expanded > maxExpandedBytes {
			return nil, false, fmt.Errorf(
				"archive expands past %d bytes; refusing to read it", maxExpandedBytes)
		}

		file, err := entryFrom(header.Name, header.Size, reader)
		if err != nil {
			break
		}
		files = append(files, file)
	}
	return files, len(files) > 0, nil
}

// readZip enumerates a zip archive.
//
// Not an OCI media type, and supported anyway: a vendor shipping configuration
// as a zip inside an OCI artifact is common, and the format is self-identifying
// so recognising it costs nothing and mistakes nothing.
func readZip(body []byte) ([]File, bool) {
	if len(body) < 4 || string(body[:4]) != "PK\x03\x04" {
		return nil, false
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, false
	}

	var (
		files    []File
		expanded int64
	)
	for _, entry := range archive.File {
		if len(files) >= maxArchiveEntries {
			break
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		// Compared as UNSIGNED before converting. A zip declaring a size past
		// MaxInt64 would wrap to a negative int64 and sail through the bound
		// below - which is precisely the input the bound exists to stop.
		if entry.UncompressedSize64 > uint64(maxEntryBytes) {
			return nil, false
		}
		size := int64(entry.UncompressedSize64) //nolint:gosec // bounded above
		expanded += size
		if expanded > maxExpandedBytes {
			return nil, false
		}

		rc, err := entry.Open()
		if err != nil {
			break
		}
		file, err := entryFrom(entry.Name, size, rc)
		_ = rc.Close()
		if err != nil {
			break
		}
		files = append(files, file)
	}
	return files, len(files) > 0
}

// entryFrom hashes one entry's content and names it.
func entryFrom(name string, size int64, r io.Reader) (File, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(r, maxEntryBytes)); err != nil {
		return File{}, err
	}

	clean, whiteout := normalisePath(name)
	return File{
		Path:     clean,
		Size:     size,
		Digest:   "sha256:" + hex.EncodeToString(sum.Sum(nil)),
		Whiteout: whiteout,
	}, nil
}

// normalisePath cleans an archive entry name and recognises an OCI whiteout.
//
// The whiteout convention is the image specification's: a layer deletes a path
// by adding a sibling entry named `.wh.<basename>`. Left alone, it would appear
// in a comparison as a file called `.wh.thing` being added, which is the
// opposite of what happened.
func normalisePath(name string) (clean string, whiteout bool) {
	clean = strings.TrimPrefix(path.Clean("/"+name), "/")

	dir, base := path.Split(clean)
	switch {
	case base == whiteoutOpaqueDir:
		// "everything below this directory is removed". Reported as the
		// directory itself, which is the only honest name for it.
		return strings.TrimSuffix(dir, "/"), true
	case strings.HasPrefix(base, whiteoutPrefix):
		return dir + strings.TrimPrefix(base, whiteoutPrefix), true
	default:
		return clean, false
	}
}

// decompress un-gzips a body when it is gzipped, and passes it through when it
// is not.
func decompress(body []byte) ([]byte, error) {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gzip header: %w", err)
	}
	defer func() { _ = zr.Close() }()

	out, err := io.ReadAll(io.LimitReader(bufio.NewReader(zr), maxExpandedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if int64(len(out)) > maxExpandedBytes {
		return nil, fmt.Errorf(
			"archive expands past %d bytes; refusing to read it", maxExpandedBytes)
	}
	return out, nil
}

// looksLikeTar checks the header checksum, which is what makes a tar
// self-identifying.
//
// The `ustar` magic alone is not enough: the original v7 format has none, and
// plenty of real archives are written in it. The checksum is present in every
// variant and is what a tar reader itself validates.
func looksLikeTar(body []byte) bool {
	if len(body) < sniffBytesNeeded {
		return false
	}
	if bytes.HasPrefix(body[257:], []byte("ustar")) {
		return true
	}

	// The header checksum: bytes 148..155, an octal string, computed over the
	// header with those eight bytes treated as spaces.
	// The field is written as octal digits followed by a NUL and a space, in
	// either order depending on the writer, so BOTH are trimmed from both ends
	// - trimming one and then the other leaves whichever came last in place,
	// and one stray byte makes the parse below reject a perfectly good header.
	var want int64
	field := strings.Trim(string(body[148:156]), " \x00")
	if field == "" {
		return false
	}
	for _, c := range field {
		if c < '0' || c > '7' {
			return false
		}
		want = want*8 + int64(c-'0')
	}

	var unsigned int64
	for i := range tarHeaderSize {
		b := body[i]
		if i >= 148 && i < 156 {
			b = ' '
		}
		unsigned += int64(b)
	}
	return unsigned == want
}
