// backup.go mounts the backup & restore endpoints (§17):
//
//	POST /api/v1/backups              start a backup job → {id}
//	GET  /api/v1/backups              list backup jobs
//	GET  /api/v1/backups/{id}         one job's status/progress
//	POST /api/v1/backups/{id}/cancel  request cancellation
//	POST /api/v1/restore              restore into an EMPTY cluster → {id}
//
// All admin-gated. Destination credentials arrive in the request body and
// are handed to the server, which holds them AES-GCM-encrypted in the
// system keyspace for the job's lifetime and purges them on completion or
// cancellation (§17; see pkg/server/backup.go). Resuming a job (body
// includes "id") therefore needs no re-supplied credentials — and may even
// omit the URL.
package v1api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/hyperkubeorg/databox/pkg/backup"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// destRequest is the shared body shape for backup create and restore:
// where to write/read and how to authenticate there.
type destRequest struct {
	To           string `json:"to"`   // backups: destination URL
	From         string `json:"from"` // restore: source URL
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	S3Endpoint   string `json:"s3_endpoint"`
	SFTPPassword string `json:"sftp_password"`
	// ID resumes a previous job: checkpoints (shard pins, cursors) come
	// from the job record and credentials/URL from the sealed record in
	// the system keyspace, so the other fields may be omitted (§17
	// resumable, coordinator-failure tolerant).
	ID string `json:"id"`
}

// creds converts the request's credential fields to the backup package's.
func (d destRequest) creds() backup.Credentials {
	return backup.Credentials{
		AccessKey:    d.AccessKey,
		SecretKey:    d.SecretKey,
		S3Endpoint:   d.S3Endpoint,
		SFTPPassword: d.SFTPPassword,
	}
}

// MountBackup attaches the backup/restore API. Registered from
// cmd/databox alongside the main Mount.
func MountBackup(r *mux.Router, s *server.Server) {
	a := &api{s: s}
	v1 := r.PathPrefix("/api/v1").Subrouter()
	v1.HandleFunc("/backups", a.backupCreate).Methods(http.MethodPost)
	v1.HandleFunc("/backups", a.backupList).Methods(http.MethodGet)
	v1.HandleFunc("/backups/{id}", a.backupStatus).Methods(http.MethodGet)
	v1.HandleFunc("/backups/{id}/cancel", a.backupCancel).Methods(http.MethodPost)
	v1.HandleFunc("/restore", a.restoreCreate).Methods(http.MethodPost)
}

// backupCreate starts a backup job.
func (a *api) backupCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var req destRequest
	// "to" may be omitted only when resuming: the server then falls back
	// to the URL sealed alongside the job's stored credentials.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.To == "" && req.ID == "") {
		fail(w, fmt.Errorf(`body must include "to" (s3://bucket/prefix or sftp://user@host/path), or "id" to resume`))
		return
	}
	id, err := a.s.StartBackup(req.To, req.creds(), req.ID)
	if err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "backup-start", "id="+id+" to="+req.To)
	jsonOut(w, http.StatusOK, map[string]string{"id": id})
}

// backupList returns all backup job records (newest data lives in the
// job records themselves; they are small).
func (a *api) backupList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	jobs, err := a.s.JobList("backup")
	if err != nil {
		fail(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"backups": jobs})
}

// backupStatus returns one job's progress.
func (a *api) backupStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.adminOnly(w, r); !ok {
		return
	}
	job, found, err := a.s.JobGet("backup", mux.Vars(r)["id"])
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		fail(w, server.ErrNotFound)
		return
	}
	jsonOut(w, http.StatusOK, job)
}

// backupCancel requests cancellation; the job observes it at its next
// checkpoint (§17 cancellable).
func (a *api) backupCancel(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	id := mux.Vars(r)["id"]
	if err := a.s.JobCancel(r.Context(), "backup", id); err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "backup-cancel", "id="+id)
	jsonOut(w, http.StatusOK, map[string]any{"ok": true})
}

// restoreCreate starts a restore job into an empty cluster.
func (a *api) restoreCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminOnly(w, r)
	if !ok {
		return
	}
	var req destRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.From == "" && req.ID == "") {
		fail(w, fmt.Errorf(`body must include "from" (s3://bucket/prefix or sftp://user@host/path), or "id" to resume`))
		return
	}
	id, err := a.s.StartRestore(req.From, req.creds(), req.ID)
	if err != nil {
		fail(w, err)
		return
	}
	a.s.Audit(r.Context(), u.Name, "restore-start", "id="+id+" from="+req.From)
	jsonOut(w, http.StatusOK, map[string]string{"id": id})
}
