package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/systera/control-plane/internal/knowledge"
	"github.com/systera/control-plane/internal/memory"
	"github.com/systera/control-plane/internal/tenancy"
)

// KnowledgeService manages versioned knowledge ingestion, hybrid search, and conflicts.
type KnowledgeService interface {
	Ingest(ctx context.Context, tenantID string, in knowledge.IngestInput) (knowledge.Document, bool, error)
	Search(ctx context.Context, tenantID string, q knowledge.SearchQuery) ([]knowledge.SearchResult, error)
	RecordConflict(ctx context.Context, tenantID, docID, conflictingDocID, note string) error
	GetDocument(ctx context.Context, tenantID, id string) (knowledge.Document, error)
}

// MemoryService manages tenant- and project-scoped distilled memories.
type MemoryService interface {
	Record(ctx context.Context, tenantID string, in memory.RecordInput) (memory.Record, error)
	Retrieve(ctx context.Context, tenantID string, q memory.RetrieveQuery) ([]memory.Record, error)
}

func (s *Server) registerKnowledgeRoutes() {
	if s.opts.Knowledge != nil {
		s.mux.Handle("POST /v1/knowledge/documents", s.authenticated(true, s.handleIngestKnowledge))
		s.mux.Handle("POST /v1/knowledge/search", s.authenticated(false, s.handleSearchKnowledge))
		s.mux.Handle("POST /v1/knowledge/documents/{id}/conflict", s.authenticated(true, s.handleRecordKnowledgeConflict))
		s.mux.Handle("GET /v1/knowledge/documents/{id}", s.authenticated(false, s.handleGetKnowledgeDocument))
	}
	if s.opts.Memory != nil {
		s.mux.Handle("POST /v1/projects/{id}/memory", s.authenticated(true, s.handleRecordMemory))
		s.mux.Handle("GET /v1/projects/{id}/memory", s.authenticated(false, s.handleRetrieveMemory))
	}
}

func (s *Server) handleIngestKnowledge(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in knowledge.IngestInput
	if !decodeJSON(w, r, &in) {
		return
	}
	doc, created, err := s.opts.Knowledge.Ingest(r.Context(), p.TenantID, in)
	if err != nil {
		if errors.Is(err, knowledge.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.internalError(w, r, "ingest knowledge", err)
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK
	}
	writeJSON(w, code, doc)
}

func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var q knowledge.SearchQuery
	if !decodeJSON(w, r, &q) {
		return
	}
	res, err := s.opts.Knowledge.Search(r.Context(), p.TenantID, q)
	if err != nil {
		if errors.Is(err, knowledge.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.internalError(w, r, "search knowledge", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

func (s *Server) handleRecordKnowledgeConflict(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in struct {
		ConflictingDocumentID string `json:"conflicting_document_id"`
		Note                  string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.opts.Knowledge.RecordConflict(r.Context(), p.TenantID, r.PathValue("id"), in.ConflictingDocumentID, in.Note); err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "knowledge document not found")
			return
		}
		s.internalError(w, r, "record knowledge conflict", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "CONFLICTED"})
}

func (s *Server) handleRecordMemory(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	var in memory.RecordInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.ProjectID = r.PathValue("id")
	rec, err := s.opts.Memory.Record(r.Context(), p.TenantID, in)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "project or run not found")
			return
		}
		if errors.Is(err, memory.ErrInvalidInput) || errors.Is(err, memory.ErrTranscriptTooLarge) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.internalError(w, r, "record memory", err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleRetrieveMemory(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	q := memory.RetrieveQuery{
		ProjectID: r.PathValue("id"),
		Query:     r.URL.Query().Get("q"),
	}
	recs, err := s.opts.Memory.Retrieve(r.Context(), p.TenantID, q)
	if err != nil {
		s.internalError(w, r, "retrieve memory", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": recs})
}

func (s *Server) handleGetKnowledgeDocument(w http.ResponseWriter, r *http.Request, p tenancy.Principal) {
	doc, err := s.opts.Knowledge.GetDocument(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "knowledge document not found")
			return
		}
		s.internalError(w, r, "get knowledge document", err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
