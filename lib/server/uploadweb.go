package server

import (
	"encoding/base64"
	"encoding/json"
	"indicer/lib/cnst"
	"indicer/lib/service"
	"indicer/lib/util"
	"indicer/pb"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// uploadSession holds an in-progress web upload.
type uploadSession struct {
	file    *os.File
	meta    *pb.StreamFileMeta
	created time.Time
}

var uploadSessions sync.Map

func init() {
	const sessionTTL = 30 * time.Minute
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			uploadSessions.Range(func(key, value any) bool {
				sess := value.(*uploadSession)
				if now.Sub(sess.created) > sessionTTL {
					sess.file.Close()
					os.Remove(sess.file.Name())
					uploadSessions.Delete(key)
				}
				return true
			})
		}
	}()
}

type webBaseFile struct {
	FilePath string           `json:"file_path"`
	FileId   string           `json:"file_id"`
	FileSize int64            `json:"file_size"`
	ChunkMap map[string]int64 `json:"chunk_map"`
}

type webUploadRes struct {
	Done    bool         `json:"done"`
	Err     string       `json:"err,omitempty"`
	EviFile *webBaseFile `json:"evi_file,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// HandleUploadStart creates an upload session for a browser client.
//
//	POST /web/upload/start
//	Body JSON: {"file_path":"...","file_type":"...","file_hash":"..."}
//	Response JSON: {"upload_id":"..."}
func HandleUploadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var meta pb.StreamFileMeta
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if meta.FileHash == "" {
		http.Error(w, cnst.ErrHashNotFound.Error(), http.StatusBadRequest)
		return
	}

	fh, err := getFileHandle(cnst.DB)
	if err != nil {
		http.Error(w, "failed to create upload file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sid, err := uuid.NewV7()
	if err != nil {
		fh.Close()
		os.Remove(fh.Name())
		http.Error(w, "failed to generate session id: "+err.Error(), http.StatusInternalServerError)
		return
	}

	uploadSessions.Store(sid.String(), &uploadSession{
		file:    fh,
		meta:    &meta,
		created: time.Now(),
	})

	writeJSON(w, http.StatusOK, map[string]string{"upload_id": sid.String()})
}

// HandleUploadChunk appends a binary chunk to an active upload session.
//
//	POST /web/upload/chunk
//	Header: Upload-Id: <upload_id>
//	Body: raw binary chunk bytes
//	Response JSON: {"ok":true}
func HandleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sid := r.Header.Get("Upload-Id")
	if sid == "" {
		http.Error(w, "missing Upload-Id header", http.StatusBadRequest)
		return
	}

	val, ok := uploadSessions.Load(sid)
	if !ok {
		http.Error(w, "upload session not found or expired", http.StatusNotFound)
		return
	}
	sess := val.(*uploadSession)

	if _, err := io.Copy(sess.file, r.Body); err != nil {
		http.Error(w, "failed to write chunk: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// HandleUploadFinalize closes the upload, stores and indexes the file, and returns the result.
//
//	POST /web/upload/finalize
//	Body JSON: {"upload_id":"..."}
//	Response JSON: webUploadRes
func HandleUploadFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.UploadID == "" {
		http.Error(w, "missing upload_id", http.StatusBadRequest)
		return
	}

	val, ok := uploadSessions.LoadAndDelete(body.UploadID)
	if !ok {
		http.Error(w, "upload session not found or already finalized", http.StatusNotFound)
		return
	}
	sess := val.(*uploadSession)
	fpath := sess.file.Name()

	if err := sess.file.Close(); err != nil {
		os.Remove(fpath)
		http.Error(w, "error closing upload file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := service.StoreStreamedFile(fpath); err != nil {
		os.Remove(fpath)
		http.Error(w, "store failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	efile, err := service.AddEvidenceMetadata(sess.meta)
	if err != nil {
		os.Remove(fpath)
		http.Error(w, "metadata failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fhash, err := base64.StdEncoding.DecodeString(sess.meta.FileHash)
	if err != nil {
		os.Remove(fpath)
		http.Error(w, "hash decode failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	chunkMap, err := service.GetFileChunkMap(efile.Start, efile.Size, fhash)
	if err != nil {
		os.Remove(fpath)
		http.Error(w, "chunk map failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.Remove(fpath); err != nil {
		log.Printf("Warning: could not remove temp file %s: %v\n", fpath, err)
	}

	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fhash)
	fileId := base64.StdEncoding.EncodeToString(eid)

	writeJSON(w, http.StatusOK, webUploadRes{
		Done: true,
		EviFile: &webBaseFile{
			FilePath: sess.meta.FilePath,
			FileId:   fileId,
			FileSize: efile.Size,
			ChunkMap: chunkMap,
		},
	})
}
