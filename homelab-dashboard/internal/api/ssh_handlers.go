package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"labdeck/internal/store"
)

type hostView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	SSH     bool   `json:"ssh"`
	SSHUser string `json:"ssh_user,omitempty"`
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	out := []hostView{}
	for _, h := range s.cfg.Hosts {
		hv := hostView{ID: h.ID, Name: h.Name, Address: h.Address, SSH: h.SSH != nil && s.gw.Enabled()}
		if h.SSH != nil {
			hv.SSHUser = h.SSH.User
		}
		out = append(out, hv)
	}
	writeJSON(w, out)
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListCredentials()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleSaveCredential(w http.ResponseWriter, r *http.Request) {
	if !s.gw.Enabled() {
		http.Error(w, "credential store disabled: LABDECK_MASTER_KEY not set", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Username string `json:"username"`
		Secret   string `json:"secret"` // password, or PEM private key for type=key
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" || req.Secret == "" {
		http.Error(w, "id and secret are required", http.StatusBadRequest)
		return
	}
	if req.Type != "password" && req.Type != "key" {
		http.Error(w, `type must be "password" or "key"`, http.StatusBadRequest)
		return
	}
	nonce, sealed, err := s.gw.Seal(req.ID, []byte(req.Secret))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = s.st.SaveCredential(store.Credential{
		ID: req.ID, Type: req.Type, Username: req.Username, Nonce: nonce, Secret: sealed,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	err := s.st.DeleteCredential(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSSHSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.st.RecentSSHSessions(intParam(r, "limit", 100))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessions)
}
