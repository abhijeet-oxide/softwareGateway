package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// The raw scanner bodies: vulnerability responses, SBOMs, policy violations and
// malware verdicts, kept as the scanner sent them.
//
// See internal/security/document.go for why they are one concept, and
// db/migrations/*/00033_security_cache.sql for why they are kept rather than
// expired. This file is the storage half: compress, account, serve, and let the
// sweeper decide when the disk matters more than the answer.

// codecGzip and codecRaw name how a payload is stored.
//
// A column rather than a convention, because the day something arrives already
// compressed - and an SBOM from a large image will - a store that assumes gzip
// double-compresses it, and a store that sniffs is a store that is one day
// wrong about somebody's findings.
const (
	codecGzip = "gzip"
	codecRaw  = "raw"
	// codecJSON is what the detail tier's pre-existing rows were written as:
	// uncompressed JSON, before there was a column to say so. Rows written
	// before the migration carry it and must keep decoding.
	codecJSON = "json"
)

// compressAbove is the size past which compressing is worth the CPU.
//
// A few hundred bytes of JSON gzips to slightly more than a few hundred bytes,
// because the deflate header and dictionary cost more than the redundancy in a
// short body saves. The threshold is low because the bodies this stores are
// almost never small - it exists so the rare one-line error payload is not
// stored larger than it arrived.
const compressAbove = 512

// encodePayload compresses a body when that is worth doing, and says which.
func encodePayload(raw []byte) ([]byte, string) {
	if len(raw) < compressAbove {
		return raw, codecRaw
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return raw, codecRaw
	}
	if _, err := zw.Write(raw); err != nil {
		return raw, codecRaw
	}
	if err := zw.Close(); err != nil {
		return raw, codecRaw
	}
	// BestSpeed rather than BestCompression: this runs on a path that has just
	// waited seconds for a scanner, JSON at level 1 is within a few percent of
	// level 9, and the difference in CPU is an order of magnitude.
	//
	// And if the result is not smaller, store the original. A payload that grew
	// is a payload that would be decompressed on every read to hand back what
	// was already there.
	if buf.Len() >= len(raw) {
		return raw, codecRaw
	}
	return buf.Bytes(), codecGzip
}

// decodePayload reverses encodePayload.
func decodePayload(stored []byte, codec string) ([]byte, error) {
	switch codec {
	case codecGzip:
		zr, err := gzip.NewReader(bytes.NewReader(stored))
		if err != nil {
			return nil, fmt.Errorf("decompress stored payload: %w", err)
		}
		defer func() { _ = zr.Close() }()
		// Bounded. A stored body is bounded by what the scanner sent and by the
		// bound on the fetch, but a corrupted gzip header can describe an
		// arbitrarily large stream, and a decompression bomb reached through
		// our own table is still a decompression bomb.
		out, err := io.ReadAll(io.LimitReader(zr, maxStoredPayload))
		if err != nil {
			return nil, fmt.Errorf("read stored payload: %w", err)
		}
		return out, nil
	case codecRaw, codecJSON, "":
		return stored, nil
	default:
		return nil, fmt.Errorf("unknown payload codec %q", codec)
	}
}

// maxStoredPayload bounds one decoded body. Generous: an SBOM for a large
// container image is tens of megabytes and a policy response can be similar.
const maxStoredPayload = 256 << 20

// fingerprintPayload is a stable hash of a body, so an unchanged re-fetch is
// recognisable without comparing megabytes.
func fingerprintPayload(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// SaveDocuments implements security.DocumentStore.
//
// # Why a document with no payload is still written
//
// Because "we asked Xray for this image's SBOM and it does not have one" is an
// answer, and an answer worth not re-asking for on every export. A row with no
// payload and a message costs nothing and stops a hundred and fifty-seven
// pointless requests the next time somebody presses download.
func (s *Security) SaveDocuments(
	ctx context.Context, scope security.Scope, docs []security.Document, ttl time.Duration,
) error {
	if len(docs) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	evictable := time.Now().UTC().Add(ttl)

	for _, doc := range docs {
		provider := doc.Provider
		if provider == "" {
			provider = scope.Provider
		}
		payload, codec := encodePayload(doc.Payload)
		fetched := doc.FetchedAt
		if fetched.IsZero() {
			fetched = time.Now().UTC()
		}
		fingerprint := doc.Fingerprint
		if fingerprint == "" && len(doc.Payload) > 0 {
			fingerprint = fingerprintPayload(doc.Payload)
		}
		contentType := doc.ContentType
		if contentType == "" {
			contentType = "application/json"
		}

		_, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO security_documents (
				product, repository, provider, artifact_ref, kind,
				payload, codec, content_type, bytes, source_bytes, fingerprint,
				fetched_at, last_used_at, evictable_at)
			VALUES (?,?,?,?,?, ?,?,?,?,?,?, ?,?,?)
			ON CONFLICT (product, repository, provider, artifact_ref, kind) DO UPDATE SET
				payload = excluded.payload,
				codec = excluded.codec,
				content_type = excluded.content_type,
				bytes = excluded.bytes,
				source_bytes = excluded.source_bytes,
				fingerprint = excluded.fingerprint,
				fetched_at = excluded.fetched_at,
				last_used_at = excluded.last_used_at,
				evictable_at = excluded.evictable_at`),
			scope.Product, scope.Repository, provider, doc.Artifact.Ref(), string(doc.Kind),
			payload, codec, contentType, len(payload), len(doc.Payload), fingerprint,
			securityTime(fetched), securityTime(fetched), securityTime(evictable))
		if err != nil {
			return fmt.Errorf("save security document %s/%s: %w", doc.Artifact.Ref(), doc.Kind, err)
		}
	}
	return nil
}

// LoadDocuments implements security.DocumentStore.
//
// Kinds are a filter rather than a fetch-everything, because the SBOM is the
// largest thing this table holds and a page that only wants to know whether one
// exists must not pull forty megabytes to find out. Callers who want the
// existence answer alone use DocumentSummaries.
func (s *Security) LoadDocuments(
	ctx context.Context, scope security.Scope,
	refs []security.ArtifactRef, kinds []security.DocumentKind,
) (map[string]map[security.DocumentKind]security.Document, error) {
	out := map[string]map[security.DocumentKind]security.Document{}
	if len(refs) == 0 {
		return out, nil
	}

	byRef := make(map[string]security.ArtifactRef, len(refs))
	for _, ref := range refs {
		byRef[ref.Ref()] = ref
	}

	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}
		where := ""
		if len(kinds) > 0 {
			kindHolders := make([]string, 0, len(kinds))
			for _, k := range kinds {
				kindHolders = append(kindHolders, "?")
				args = append(args, string(k))
			}
			where = " AND kind IN (" + strings.Join(kindHolders, ",") + ")"
		}

		rows, err := s.db.QueryContext(ctx, s.q(`
			SELECT artifact_ref, kind, payload, codec, content_type, source_bytes,
			       fingerprint, `+s.dialect.TimestampText("fetched_at")+`, provider
			  FROM security_documents
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (`+strings.Join(placeholders, ",")+`)`+where), args...)
		if err != nil {
			return nil, fmt.Errorf("load security documents: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var (
					ref, kind, codec, contentType, fingerprint, fetchedAt, provider string
					payload                                                         []byte
					sourceBytes                                                     int
				)
				if err := rows.Scan(&ref, &kind, &payload, &codec, &contentType,
					&sourceBytes, &fingerprint, &fetchedAt, &provider); err != nil {
					return fmt.Errorf("scan security document: %w", err)
				}
				decoded, err := decodePayload(payload, codec)
				if err != nil {
					// A body we cannot decode is a miss, not a failure: the
					// scanner is right there and the answer is recoverable.
					continue
				}
				doc := security.Document{
					Artifact:    byRef[ref],
					Kind:        security.DocumentKind(kind),
					Provider:    provider,
					ContentType: contentType,
					Payload:     decoded,
					Available:   len(decoded) > 0,
					SourceBytes: sourceBytes,
					Fingerprint: fingerprint,
				}
				if t, err := parseSecurityTime(fetchedAt); err == nil {
					doc.FetchedAt = t
				}
				if out[ref] == nil {
					out[ref] = map[security.DocumentKind]security.Document{}
				}
				out[ref][doc.Kind] = doc
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		s.touchDocuments(ctx, scope, chunk)
	}
	return out, nil
}

// DocumentSummaries answers "what is held for these artifacts" WITHOUT reading
// the payloads.
//
// The distinction is the whole reason this method exists beside LoadDocuments.
// A release page draws a download button per image per kind: 157 images times
// four kinds, and reading the bodies to decide whether to draw a button would
// be hundreds of megabytes to render a row of icons.
func (s *Security) DocumentSummaries(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
) (map[string][]security.DocumentSummary, error) {
	out := map[string][]security.DocumentSummary{}
	if len(refs) == 0 {
		return out, nil
	}

	for _, chunk := range chunkRefs(refs, sqlChunk) {
		args := []any{scope.Product, scope.Repository, scope.Provider}
		placeholders := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			placeholders = append(placeholders, "?")
			args = append(args, ref.Ref())
		}

		rows, err := s.db.QueryContext(ctx, s.q(`
			SELECT artifact_ref, kind, content_type, source_bytes,
			       `+s.dialect.TimestampText("fetched_at")+`
			  FROM security_documents
			 WHERE product = ? AND repository = ? AND provider = ?
			   AND artifact_ref IN (`+strings.Join(placeholders, ",")+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("list security documents: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var ref, kind, contentType, fetchedAt string
				var sourceBytes int
				if err := rows.Scan(&ref, &kind, &contentType, &sourceBytes, &fetchedAt); err != nil {
					return fmt.Errorf("scan security document summary: %w", err)
				}
				out[ref] = append(out[ref], security.DocumentSummary{
					Kind: security.DocumentKind(kind),
					// The scope's provider, because that is what the row was
					// filtered by. Carried so a caller reading several scopes
					// can tell whose body is whose.
					Provider:    scope.Provider,
					Available:   sourceBytes > 0,
					ContentType: contentType,
					SourceBytes: sourceBytes,
					FetchedAt:   fetchedAt,
				})
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// touchDocuments records that these rows were read, for the eviction order.
func (s *Security) touchDocuments(
	ctx context.Context, scope security.Scope, refs []security.ArtifactRef,
) {
	if len(refs) == 0 {
		return
	}
	args := []any{securityTime(time.Now().UTC()), scope.Product, scope.Repository, scope.Provider}
	placeholders := make([]string, 0, len(refs))
	for _, ref := range refs {
		placeholders = append(placeholders, "?")
		args = append(args, ref.Ref())
	}
	_, _ = s.db.ExecContext(ctx, s.q(`
		UPDATE security_documents SET last_used_at = ?
		 WHERE product = ? AND repository = ? AND provider = ?
		   AND artifact_ref IN (`+strings.Join(placeholders, ",")+`)`), args...)
}

var _ security.DocumentStore = (*Security)(nil)
