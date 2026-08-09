package api

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/collybia/mirocraft/internal/fileman"
	"github.com/collybia/mirocraft/internal/store"
)

// multipartMemory is how much of an upload is buffered in memory before the
// rest spills to a temporary file.
const multipartMemory = 8 << 20 // 8 MiB

// --- wire types ---

type listFilesResponse struct {
	Path  string          `json:"path"`
	Items []fileman.Entry `json:"items"`
}

type fileContentResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeContentRequest struct {
	Content string `json:"content"`
}

type pathRequest struct {
	Path string `json:"path"`
}

type movePathsRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type archiveRequest struct {
	Paths []string `json:"paths"`
	Dest  string   `json:"dest"`
}

type archiveResponse struct {
	Path string `json:"path"`
}

type unarchiveRequest struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
}

type uploadResponse struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// --- plumbing ---

// serverRoot authorizes the request and returns a sandbox over the server's
// directory.
func (a *API) serverRoot(w http.ResponseWriter, r *http.Request, scope string) (*fileman.Root, *store.Server, bool) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, scope); !ok {
		return nil, nil, false
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return nil, nil, false
	}

	root, err := fileman.NewRoot(a.serverDir(server))
	if err != nil {
		a.log.Error("opening the server directory failed",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"the server directory is not available")
		return nil, nil, false
	}

	return root, server, true
}

// writeFileError maps file manager errors onto the documented codes.
//
// Every path rejection collapses to path_traversal_denied with 403, whatever
// the specific reason. A client that is guessing at paths learns only that it
// was refused, not which of its guesses were structurally invalid.
func (a *API) writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fileman.ErrEscapes), errors.Is(err, fileman.ErrInvalidPath):
		writeErrorDetails(w, http.StatusForbidden, "path_traversal_denied",
			"the path is outside the server directory or is not allowed",
			map[string]any{"field": "path"})

	case errors.Is(err, fileman.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeServerNotFound, "path does not exist")

	case errors.Is(err, fileman.ErrTooLarge):
		writeErrorDetails(w, http.StatusRequestEntityTooLarge, "file_too_large", err.Error(),
			map[string]any{
				"max_text_bytes":   fileman.MaxTextBytes,
				"max_upload_bytes": fileman.MaxUploadBytes,
			})

	case errors.Is(err, fileman.ErrExists):
		writeError(w, http.StatusConflict, CodeValidationFailed, "the path already exists")

	case errors.Is(err, fileman.ErrIsADirectory),
		errors.Is(err, fileman.ErrNotADirEntry),
		errors.Is(err, fileman.ErrBinary):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, err.Error())

	default:
		a.log.Error("file operation failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError, "the file operation failed")
	}
}

// audited records a mutating file action.
func (a *API) auditFile(r *http.Request, action, target string) {
	if principal, ok := principalFrom(r.Context()); ok {
		a.audit(r, principal.UserID, action, r.PathValue("id"), target)
	}
}

// --- handlers ---

// handleListFiles serves GET /servers/{id}/files?path=/.
func (a *API) handleListFiles(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesRead)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	entries, err := root.List(rel)
	if err != nil {
		a.writeFileError(w, err)
		return
	}

	if rel == "" {
		rel = "/"
	}
	writeJSON(w, http.StatusOK, listFilesResponse{Path: rel, Items: entries})
}

// handleReadFile serves GET /servers/{id}/files/content?path=.
func (a *API) handleReadFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesRead)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	content, err := root.ReadText(rel)
	if err != nil {
		a.writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileContentResponse{Path: rel, Content: content})
}

// handleWriteFile serves PUT /servers/{id}/files/content?path=.
func (a *API) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req writeContentRequest
	if !decodeBody(w, r, &req) {
		return
	}

	rel := r.URL.Query().Get("path")
	if err := root.WriteText(rel, req.Content); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.write", rel)
	w.WriteHeader(http.StatusNoContent)
}

// handleDownloadFile serves GET /servers/{id}/files/download?path=.
func (a *API) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesRead)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	file, info, err := root.Open(rel)
	if err != nil {
		a.writeFileError(w, err)
		return
	}
	defer func() { _ = file.Close() }()

	name := path.Base(strings.ReplaceAll(rel, "\\", "/"))
	// Encoded rather than interpolated: a file name is operator-controlled
	// and a quote or newline in it would let the header be rewritten.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/octet-stream")

	// ServeContent handles range requests, so a large download can resume.
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// handleUploadFile serves POST /servers/{id}/files/upload.
func (a *API) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	// The whole request is capped before parsing, so a hostile multipart body
	// cannot fill the disk with temporary files while being read.
	r.Body = http.MaxBytesReader(w, r.Body, fileman.MaxUploadBytes+multipartMemory)

	if err := r.ParseMultipartForm(multipartMemory); err != nil { // #nosec G120 -- capped by MaxBytesReader above
		writeError(w, http.StatusBadRequest, CodeValidationFailed,
			"the request must be multipart with a file field")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "no file field in the request")
		return
	}
	defer func() { _ = file.Close() }()

	// The destination is the directory field plus the client's file name. The
	// name goes through the same sandbox as everything else, because a
	// browser will happily send "../../evil.jar" as a filename.
	dir := r.FormValue("path")
	target := path.Join("/", strings.ReplaceAll(dir, "\\", "/"),
		path.Base(strings.ReplaceAll(header.Filename, "\\", "/")))

	written, err := root.Upload(target, file)
	if err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.upload", target)
	writeJSON(w, http.StatusCreated, uploadResponse{Path: target, Bytes: written})
}

// handleMkdir serves POST /servers/{id}/files/mkdir.
func (a *API) handleMkdir(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req pathRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := root.Mkdir(req.Path); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.mkdir", req.Path)
	w.WriteHeader(http.StatusCreated)
}

// handleMoveFile serves POST /servers/{id}/files/move.
func (a *API) handleMoveFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req movePathsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := root.Move(req.From, req.To); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.move", fmt.Sprintf("%s -> %s", req.From, req.To))
	w.WriteHeader(http.StatusNoContent)
}

// handleCopyFile serves POST /servers/{id}/files/copy.
func (a *API) handleCopyFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req movePathsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := root.Copy(req.From, req.To); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.copy", fmt.Sprintf("%s -> %s", req.From, req.To))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteFile serves DELETE /servers/{id}/files?path=.
func (a *API) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	if err := root.Remove(rel); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.delete", rel)
	w.WriteHeader(http.StatusNoContent)
}

// handleArchive serves POST /servers/{id}/files/archive.
func (a *API) handleArchive(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req archiveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Paths) == 0 {
		writeFieldError(w, "paths", "at least one path is required")
		return
	}
	dest := req.Dest
	if dest == "" {
		dest = "/archive.zip"
	}

	name, err := root.Archive(req.Paths, dest)
	if err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.archive", name)
	writeJSON(w, http.StatusCreated, archiveResponse{Path: name})
}

// handleUnarchive serves POST /servers/{id}/files/unarchive.
func (a *API) handleUnarchive(w http.ResponseWriter, r *http.Request) {
	root, _, ok := a.serverRoot(w, r, ScopeFilesWrite)
	if !ok {
		return
	}

	var req unarchiveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeFieldError(w, "path", "an archive path is required")
		return
	}
	destination := req.Destination
	if destination == "" {
		destination = "/"
	}

	if err := root.Unarchive(req.Path, destination); err != nil {
		a.writeFileError(w, err)
		return
	}

	a.auditFile(r, "file.unarchive", req.Path)
	w.WriteHeader(http.StatusNoContent)
}
