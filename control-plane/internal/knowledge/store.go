package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/internal/models"
	"github.com/systera/control-plane/internal/platform/ids"
	"github.com/systera/control-plane/internal/platform/outbox"
	"github.com/systera/control-plane/internal/platform/postgres"
)

// Store manages versioned knowledge documents, pgvector chunks, provenance, and hybrid retrieval.
type Store struct {
	pool           *pgxpool.Pool
	embedder       models.EmbeddingProvider
	embeddingModel string
	now            func() time.Time
}

func NewStore(pool *pgxpool.Pool, embedder models.EmbeddingProvider, embeddingModel string) *Store {
	if embedder == nil {
		embedder = models.NewDeterministicHashEmbedder(models.DefaultEmbeddingDimensions)
	}
	if embeddingModel == "" {
		embeddingModel = "text-embedding-3-small"
	}
	return &Store{
		pool:           pool,
		embedder:       embedder,
		embeddingModel: embeddingModel,
		now:            time.Now,
	}
}

// Prepared is a normalized, chunked and embedded document ready to persist. Embedding runs
// outside any transaction (it may call a remote provider).
type Prepared struct {
	In       IngestInput
	DocHash  string
	Snippets []CodeSnippet
	Chunks   []string
	Vectors  [][]float32
}

// Prepare validates, defaults, normalizes, chunks and embeds an ingest request (§6.2).
func (s *Store) Prepare(ctx context.Context, in IngestInput) (Prepared, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.RawContent) == "" {
		return Prepared{}, fmt.Errorf("%w: title and raw_content are required", ErrInvalidInput)
	}
	if in.CanonicalKey == "" {
		in.CanonicalKey = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(in.Title), " ", "-"))
	}
	if in.SourceType == "" {
		in.SourceType = "PRIMARY_DOC"
	}
	switch in.SourceType {
	case "PRIMARY_DOC", "RFC", "RELEASE_NOTES", "ENGINEERING_BLOG", "PAPER", "COMMUNITY", "INTERNAL":
	default:
		return Prepared{}, fmt.Errorf("%w: invalid source_type %q", ErrInvalidInput, in.SourceType)
	}
	if in.AccessPolicy == "" {
		in.AccessPolicy = "TENANT"
	}
	switch in.AccessPolicy {
	case "PUBLIC", "TENANT", "RESTRICTED":
	default:
		return Prepared{}, fmt.Errorf("%w: invalid access_policy %q", ErrInvalidInput, in.AccessPolicy)
	}
	if in.Maturity == "" {
		in.Maturity = "MAINSTREAM"
	}
	switch in.Maturity {
	case "EXPERIMENTAL", "EMERGING", "MAINSTREAM", "LEGACY", "DEPRECATED":
	default:
		return Prepared{}, fmt.Errorf("%w: invalid maturity %q", ErrInvalidInput, in.Maturity)
	}
	if in.ConfidenceBand == "" {
		in.ConfidenceBand = "MEDIUM"
	}
	switch in.ConfidenceBand {
	case "HIGH", "MEDIUM", "LOW":
	default:
		return Prepared{}, fmt.Errorf("%w: invalid confidence_band %q", ErrInvalidInput, in.ConfidenceBand)
	}
	if in.Licence == "" {
		in.Licence = "UNKNOWN"
	}
	if in.Collector == "" {
		in.Collector = "api"
	}
	if in.TrustScore <= 0 {
		if in.IsPrimarySource {
			in.TrustScore = 0.85
		} else {
			in.TrustScore = 0.60
		}
	}

	normalized, snippets, docHash := ExtractAndNormalize(in.RawContent)
	chunkTexts := ChunkText(normalized, 1200, 200)
	if len(chunkTexts) == 0 {
		return Prepared{}, fmt.Errorf("%w: normalized content is empty", ErrInvalidInput)
	}
	vecs, err := s.embedder.Embed(ctx, s.embeddingModel, chunkTexts)
	if err != nil {
		return Prepared{}, fmt.Errorf("embed chunks: %w", err)
	}
	return Prepared{In: in, DocHash: docHash, Snippets: snippets, Chunks: chunkTexts, Vectors: vecs}, nil
}

// Ingest extracts, normalizes, deduplicates, chunks, embeds, and persists a knowledge document (§6.2).
// If an identical content_hash already exists for the tenant, it returns the existing document with created=false.
func (s *Store) Ingest(ctx context.Context, tenantID string, in IngestInput) (Document, bool, error) {
	p, err := s.Prepare(ctx, in)
	if err != nil {
		return Document{}, false, err
	}
	var doc Document
	var created bool
	err = postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		doc, created, err = IngestPreparedTx(ctx, tx, tenantID, p)
		return err
	})
	return doc, created, err
}

// IngestPreparedTx persists a prepared document inside the caller's transaction. tenantID ""
// stores platform-wide knowledge (tenant_id NULL); that requires the systera_system role and
// access_policy PUBLIC. A new version of an existing canonical_key supersedes the previous
// live versions in the same (tenant, key) chain (§6.5 "mark stale or superseded knowledge").
func IngestPreparedTx(ctx context.Context, tx pgx.Tx, tenantID string, p Prepared) (Document, bool, error) {
	in := p.In
	if tenantID == "" && in.AccessPolicy != "PUBLIC" {
		return Document{}, false, fmt.Errorf("%w: platform-wide knowledge must be PUBLIC", ErrInvalidInput)
	}
	var doc Document
	// Deduplication check by content_hash within the tenant (and against PUBLIC knowledge).
	row := tx.QueryRow(ctx, `
		SELECT id::text, COALESCE(tenant_id::text, ''), canonical_key, version_number, title,
		       source_url, source_type, is_primary_source, licence, access_policy, published_at,
		       maturity, confidence_band, status, COALESCE(superseded_by::text, ''),
		       content_hash, metadata_json, created_at, updated_at
		FROM knowledge_documents
		WHERE content_hash = $1 AND (tenant_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid OR access_policy = 'PUBLIC')
		ORDER BY created_at
		LIMIT 1`, p.DocHash, tenantID)
	existing, scanErr := scanDoc(row)
	if scanErr == nil {
		return existing, false, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return Document{}, false, scanErr
	}

	var nextVer int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_number), 0) + 1 FROM knowledge_documents
		WHERE canonical_key = $1 AND tenant_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid`,
		in.CanonicalKey, tenantID).Scan(&nextVer); err != nil {
		return Document{}, false, err
	}

	metaJSON, _ := json.Marshal(in.Metadata)
	if len(metaJSON) == 0 || string(metaJSON) == "null" {
		metaJSON = []byte("{}")
	}
	docID := ids.New()
	err := tx.QueryRow(ctx, `
		INSERT INTO knowledge_documents (
			id, tenant_id, canonical_key, version_number, title, source_url, source_type,
			is_primary_source, licence, access_policy, published_at, maturity, confidence_band,
			status, content_hash, metadata_json
		) VALUES (
			$1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13,
			'ACTIVE', $14, $15
		)
		RETURNING created_at, updated_at`,
		docID, tenantID, in.CanonicalKey, nextVer, in.Title, in.SourceURL, in.SourceType,
		in.IsPrimarySource, in.Licence, in.AccessPolicy, in.PublishedAt, in.Maturity, in.ConfidenceBand,
		p.DocHash, metaJSON,
	).Scan(&doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		return Document{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge_documents SET status = 'SUPERSEDED', superseded_by = $1, updated_at = NOW()
		WHERE canonical_key = $2 AND tenant_id IS NOT DISTINCT FROM NULLIF($3, '')::uuid
		  AND id <> $1 AND status <> 'SUPERSEDED'`, docID, in.CanonicalKey, tenantID); err != nil {
		return Document{}, false, err
	}

	snipJSON, _ := json.Marshal(p.Snippets)
	if len(snipJSON) == 0 || string(snipJSON) == "null" {
		snipJSON = []byte("[]")
	}
	for i, txt := range p.Chunks {
		cSum := sha256.Sum256([]byte(txt))
		cHash := hex.EncodeToString(cSum[:])
		vecLit := models.FormatPGVector(p.Vectors[i])
		cSnip := []byte("[]")
		if i == 0 {
			cSnip = snipJSON
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_chunks (
				tenant_id, document_id, chunk_index, content, code_snippets,
				token_count, content_hash, embedding
			) VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8::vector)`,
			tenantID, docID, i, txt, cSnip, estimateTokens(txt), cHash, vecLit,
		); err != nil {
			return Document{}, false, err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_provenance (tenant_id, document_id, source_uri, collector, trust_score)
		VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5)`,
		tenantID, docID, in.SourceURL, in.Collector, in.TrustScore,
	); err != nil {
		return Document{}, false, err
	}

	doc.ID = docID
	doc.TenantID = tenantID
	doc.CanonicalKey = in.CanonicalKey
	doc.VersionNumber = nextVer
	doc.Title = in.Title
	doc.SourceURL = in.SourceURL
	doc.SourceType = in.SourceType
	doc.IsPrimarySource = in.IsPrimarySource
	doc.Licence = in.Licence
	doc.AccessPolicy = in.AccessPolicy
	doc.PublishedAt = in.PublishedAt
	doc.Maturity = in.Maturity
	doc.ConfidenceBand = in.ConfidenceBand
	doc.Status = StatusActive
	doc.ContentHash = p.DocHash
	doc.Metadata = in.Metadata

	if tenantID == "" {
		// Platform-wide knowledge is announced through Engineering Pulse's agent inbox
		// (agent_knowledge_notifications); the outbox is tenant-scoped.
		return doc, true, nil
	}
	_, err = outbox.InsertTx(ctx, tx, outbox.EventEnvelope{
		EventType:     "knowledge.updated",
		TenantID:      tenantID,
		CorrelationID: ids.New(),
		Payload: map[string]any{
			"document_id":       docID,
			"canonical_key":     in.CanonicalKey,
			"version_number":    nextVer,
			"is_primary_source": in.IsPrimarySource,
			"chunks":            len(p.Chunks),
		},
	})
	return doc, true, err
}

// GetDocument returns one visible knowledge document.
func (s *Store) GetDocument(ctx context.Context, tenantID, id string) (Document, error) {
	if !ids.Valid(id) {
		return Document{}, ErrNotFound
	}
	var d Document
	err := postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		var err error
		d, err = scanDoc(tx.QueryRow(ctx, `
			SELECT id::text, COALESCE(tenant_id::text, ''), canonical_key, version_number, title,
			       source_url, source_type, is_primary_source, licence, access_policy, published_at,
			       maturity, confidence_band, status, COALESCE(superseded_by::text, ''),
			       content_hash, metadata_json, created_at, updated_at
			FROM knowledge_documents WHERE id = $1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// Search executes hybrid retrieval (metadata filters + keyword ts_rank_cd + pgvector cosine
// + recency + primary-source preference + graph/memory links) and frames content as untrusted data (§6.3, §6.5).
func (s *Store) Search(ctx context.Context, tenantID string, q SearchQuery) ([]SearchResult, error) {
	if strings.TrimSpace(q.Query) == "" {
		return nil, fmt.Errorf("%w: query is required", ErrInvalidInput)
	}
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 10
	}

	vecs, err := s.embedder.Embed(ctx, s.embeddingModel, []string{q.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	vecLit := models.FormatPGVector(vecs[0])

	var results []SearchResult
	err = postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		// Fetch top candidate chunks combining vector + keyword signals, then apply full hybrid scoring.
		rows, err := tx.Query(ctx, `
			SELECT d.id::text, c.id::text, d.title, d.source_url, d.source_type, d.is_primary_source,
			       d.licence, d.published_at, d.maturity, d.confidence_band, d.status,
			       COALESCE(p.conflicts_with_doc_ids::text[], '{}'), COALESCE(p.conflict_note, ''),
			       c.content,
			       COALESCE(1.0 - (c.embedding <=> $1::vector), 0.0) AS vec_sim,
			       COALESCE(ts_rank_cd(c.search_tsv, plainto_tsquery('english', $2)), 0.0) AS kw_rank
			FROM knowledge_chunks c
			JOIN knowledge_documents d ON d.id = c.document_id
			LEFT JOIN knowledge_provenance p ON p.document_id = d.id
			WHERE d.status <> 'SUPERSEDED'
			  AND ($3 OR d.status <> 'STALE')
			  AND (NOT $4 OR d.is_primary_source = true)
			  AND (COALESCE(cardinality($5::text[]), 0) = 0 OR d.source_type = ANY($5::text[]))
			  AND (COALESCE(cardinality($6::text[]), 0) = 0 OR d.maturity = ANY($6::text[]))
			  AND ($8 = '' OR d.canonical_key LIKE $8 || '%')
			ORDER BY (0.6 * COALESCE(1.0 - (c.embedding <=> $1::vector), 0.0) +
			          0.4 * COALESCE(ts_rank_cd(c.search_tsv, plainto_tsquery('english', $2)), 0.0)) DESC
			LIMIT $7`,
			vecLit, q.Query, q.IncludeStale, q.PrimaryOnly, q.SourceTypes, q.Maturities, q.Limit*3, likeEscape(q.KeyPrefix),
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		now := s.now().UTC()
		for rows.Next() {
			var r SearchResult
			var rawContent string
			var vecSim, kwRank float64
			if err := rows.Scan(
				&r.DocumentID, &r.ChunkID, &r.Title, &r.SourceURL, &r.SourceType, &r.IsPrimarySource,
				&r.Licence, &r.PublishedAt, &r.Maturity, &r.ConfidenceBand, &r.Status,
				&r.ConflictsWith, &r.ConflictNote, &rawContent, &vecSim, &kwRank,
			); err != nil {
				return err
			}
			r.ProvenanceRef = "knowledge:" + r.DocumentID
			r.KeywordScore = kwRank
			r.VectorScore = vecSim
			r.InjectionSuspected = DetectPromptInjection(rawContent)
			r.Untrusted = true
			r.Content = FrameUntrustedContent(r.ProvenanceRef, rawContent, r.InjectionSuspected)
			r.Score = ComputeHybridScore(ScoreFactors{
				VectorSim:       vecSim,
				KeywordRank:     kwRank,
				PublishedAt:     r.PublishedAt,
				Now:             now,
				IsPrimarySource: r.IsPrimarySource,
				ConfidenceBand:  r.ConfidenceBand,
				Status:          r.Status,
			})
			results = append(results, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > q.Limit {
		results = results[:q.Limit]
	}
	if results == nil {
		results = []SearchResult{}
	}
	return results, nil
}

// RecordConflict explicitly records a conflict between two knowledge documents instead of
// silently overwriting or resolving it (§6.5).
func (s *Store) RecordConflict(ctx context.Context, tenantID, docID, conflictingDocID, note string) error {
	if !ids.Valid(docID) || !ids.Valid(conflictingDocID) {
		return ErrNotFound
	}
	return postgres.WithTenantRoleTx(ctx, s.pool, tenantID, postgres.AppRole, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE knowledge_documents
			SET status = 'CONFLICTED', updated_at = NOW()
			WHERE id = $1 AND tenant_id = $2`, docID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `
			UPDATE knowledge_provenance
			SET conflicts_with_doc_ids = array_append(conflicts_with_doc_ids, $1::uuid),
			    conflict_note = CASE WHEN conflict_note = '' THEN $2 ELSE conflict_note || '; ' || $2 END
			WHERE document_id = $3 AND tenant_id = $4`, conflictingDocID, note, docID, tenantID)
		return err
	})
}

type rowScanner interface{ Scan(dest ...any) error }

func scanDoc(row rowScanner) (Document, error) {
	var d Document
	var metaRaw []byte
	err := row.Scan(
		&d.ID, &d.TenantID, &d.CanonicalKey, &d.VersionNumber, &d.Title,
		&d.SourceURL, &d.SourceType, &d.IsPrimarySource, &d.Licence, &d.AccessPolicy, &d.PublishedAt,
		&d.Maturity, &d.ConfidenceBand, &d.Status, &d.SupersededBy,
		&d.ContentHash, &metaRaw, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return Document{}, err
	}
	_ = json.Unmarshal(metaRaw, &d.Metadata)
	return d, nil
}

// likeEscape escapes LIKE metacharacters so a key prefix matches literally.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}
