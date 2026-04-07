package server

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	prompts, err := db.ListPrompts(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompts == nil {
		prompts = []domain.Prompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prompt, err := db.GetPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	bindings, _ := db.GetBindingsForPrompt(s.db, id)
	if bindings == nil {
		bindings = []domain.PromptBinding{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"prompt":   prompt,
		"bindings": bindings,
	})
}

type createPromptRequest struct {
	Name     string                 `json:"name"`
	Body     string                 `json:"body"`
	Bindings []domain.PromptBinding `json:"bindings"`
}

func (s *Server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	var req createPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}

	id := uuid.New().String()
	prompt := domain.Prompt{
		ID:     id,
		Name:   req.Name,
		Body:   req.Body,
		Source: "user",
	}

	if err := db.CreatePrompt(s.db, prompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if len(req.Bindings) > 0 {
		for i := range req.Bindings {
			req.Bindings[i].PromptID = id
		}
		if err := db.SetBindingsForPrompt(s.db, id, req.Bindings); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	created, _ := db.GetPrompt(s.db, id)
	writeJSON(w, http.StatusCreated, created)
}

type updatePromptRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updatePromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}

	if err := db.UpdatePrompt(s.db, id, req.Name, req.Body); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	updated, _ := db.GetPrompt(s.db, id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Prevent deleting system prompts
	prompt, err := db.GetPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}
	if prompt.Source == "system" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot delete system prompts"})
		return
	}

	if err := db.DeletePrompt(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePromptBindingsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bindings, err := db.GetBindingsForPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if bindings == nil {
		bindings = []domain.PromptBinding{}
	}
	writeJSON(w, http.StatusOK, bindings)
}

func (s *Server) handlePromptBindingsSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var bindings []domain.PromptBinding
	if err := json.NewDecoder(r.Body).Decode(&bindings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	for i := range bindings {
		bindings[i].PromptID = id
	}

	if err := db.SetBindingsForPrompt(s.db, id, bindings); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, bindings)
}
