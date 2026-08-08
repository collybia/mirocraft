package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/fileman"
)

// filesToken mints a token with both file scopes.
func (e *testEnv) filesToken() string {
	return e.mintToken(e.user.ID, []string{
		ScopeServersRead, ScopeFilesRead, ScopeFilesWrite,
	})
}

// seedFiles puts a small tree in the fixture server's directory and returns
// the directory that sits outside it, which the escape attempts target.
func (e *testEnv) seedFiles(t *testing.T) string {
	t.Helper()

	dir := e.api.serverDir(e.serverRecord)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	files := map[string]string{
		"server.properties": "motd=hi\nmax-players=20\n",
		"eula.txt":          "eula=true\n",
		"world/level.dat":   "not really nbt",
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o640); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}

	// A sibling directory with something worth stealing.
	outside := filepath.Join(filepath.Dir(dir), "outside-secrets")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "passwords.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	return outside
}

func escapePath(p string) string { return url.QueryEscape(p) }

// --- traversal through the API, the mandatory part ---

// Every file endpoint must refuse a path that leaves the server directory.
// Covering only one of them would leave the others as the way in.
func TestFileEndpointsRefuseTraversal(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	hostile := []string{
		"../outside-secrets",
		"../outside-secrets/passwords.txt",
		"../../../etc/passwd",
		"..\\outside-secrets",
		"world/../../outside-secrets",
		"/../outside-secrets",
		"C:\\Windows\\System32",
	}

	base := "/api/v1/servers/" + testServerID

	for _, p := range hostile {
		t.Run(p, func(t *testing.T) {
			// Reads.
			for _, path := range []string{
				base + "/files?path=" + escapePath(p),
				base + "/files/content?path=" + escapePath(p),
				base + "/files/download?path=" + escapePath(p),
			} {
				resp := e.do(http.MethodGet, path, nil, token)
				assertRefused(t, resp, "GET "+path)
			}

			// Writes.
			resp := e.do(http.MethodPut, base+"/files/content?path="+escapePath(p),
				writeContentRequest{Content: "owned"}, token)
			assertRefused(t, resp, "PUT content")

			resp = e.do(http.MethodDelete, base+"/files?path="+escapePath(p), nil, token)
			assertRefused(t, resp, "DELETE")

			resp = e.do(http.MethodPost, base+"/files/mkdir", pathRequest{Path: p}, token)
			assertRefused(t, resp, "mkdir")

			resp = e.do(http.MethodPost, base+"/files/move",
				movePathsRequest{From: "/eula.txt", To: p}, token)
			assertRefused(t, resp, "move out")

			resp = e.do(http.MethodPost, base+"/files/copy",
				movePathsRequest{From: "/eula.txt", To: p}, token)
			assertRefused(t, resp, "copy out")

			resp = e.do(http.MethodPost, base+"/files/archive",
				archiveRequest{Paths: []string{p}, Dest: "/out.zip"}, token)
			assertRefused(t, resp, "archive from outside")
		})
	}
}

// assertRefused checks that a response is a refusal rather than a success.
func assertRefused(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 400 {
		t.Errorf("%s succeeded with %d, want it refused", what, resp.StatusCode)
		return
	}
	// A traversal must be reported as such rather than as a generic error, so
	// the client can tell a refusal from a server fault.
	if resp.StatusCode == http.StatusInternalServerError {
		t.Errorf("%s gave 500; a refused path must be a client error", what)
	}
}

// Nothing outside the server directory may be readable, and nothing may be
// written there.
func TestFileEndpointsCannotTouchTheOutside(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	outside := e.seedFiles(t)

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodGet,
		base+"/files/content?path="+escapePath("../outside-secrets/passwords.txt"), nil, token)
	body := decodeJSONRaw(t, resp)
	if strings.Contains(body, "secret") {
		t.Fatal("the response leaked a file from outside the server directory")
	}

	resp = e.do(http.MethodPut,
		base+"/files/content?path="+escapePath("../outside-secrets/planted.txt"),
		writeContentRequest{Content: "owned"}, token)
	_ = resp.Body.Close()

	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !os.IsNotExist(err) {
		t.Fatal("a file was written outside the server directory")
	}
}

// A traversing path is reported as path_traversal_denied, which docs/API.md
// documents and clients branch on.
func TestTraversalUsesTheDocumentedCode(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/files?path="+escapePath("../outside-secrets"), nil, token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "path_traversal_denied" {
		t.Fatalf("error code = %q, want path_traversal_denied", code)
	}
}

// --- ordinary use ---

func TestListFiles(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/files?path=/", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[listFilesResponse](t, resp)
	var names []string
	for _, item := range body.Items {
		names = append(names, item.Name)
	}
	// Directories first, then files alphabetically.
	if strings.Join(names, ",") != "world,eula.txt,server.properties" {
		t.Fatalf("listing = %v", names)
	}
}

func TestReadAndWriteFileThroughTheAPI(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodGet, base+"/files/content?path=/server.properties", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if content := decodeJSON[fileContentResponse](t, resp).Content; !strings.Contains(content, "max-players=20") {
		t.Fatalf("content = %q", content)
	}

	written := e.do(http.MethodPut, base+"/files/content?path=/server.properties",
		writeContentRequest{Content: "motd=changed\n"}, token)
	if written.StatusCode != http.StatusNoContent {
		t.Fatalf("write status = %d, want 204", written.StatusCode)
	}
	_ = written.Body.Close()

	again := e.do(http.MethodGet, base+"/files/content?path=/server.properties", nil, token)
	if content := decodeJSON[fileContentResponse](t, again).Content; content != "motd=changed\n" {
		t.Fatalf("content after write = %q", content)
	}
}

func TestDownloadFile(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/files/download?path=/eula.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer func() { _ = resp.Body.Close() }()

	if disp := resp.Header.Get("Content-Disposition"); !strings.Contains(disp, "eula.txt") {
		t.Errorf("Content-Disposition = %q", disp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(body) != "eula=true\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestUploadFile(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	resp := e.uploadFile(t, token, "/plugins", "Essentials.jar", "jar contents")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	body := decodeJSON[uploadResponse](t, resp)
	if body.Path != "/plugins/Essentials.jar" {
		t.Fatalf("path = %q", body.Path)
	}

	stored := filepath.Join(e.api.serverDir(e.serverRecord), "plugins", "Essentials.jar")
	content, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("the upload is not on disk: %v", err)
	}
	if string(content) != "jar contents" {
		t.Fatalf("stored content = %q", content)
	}
}

// A browser will send whatever filename it is given, including one designed
// to climb out of the upload directory.
func TestUploadRefusesATraversingFilename(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	outside := e.seedFiles(t)

	for _, name := range []string{"../../planted.jar", "..\\..\\planted.jar", "/etc/planted"} {
		resp := e.uploadFile(t, token, "/plugins", name, "owned")
		// Either refused, or the name was reduced to its base — never written
		// outside.
		_ = resp.Body.Close()

		if _, err := os.Stat(filepath.Join(outside, "planted.jar")); !os.IsNotExist(err) {
			t.Fatalf("filename %q escaped the server directory", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(e.api.serverDir(e.serverRecord)), "planted.jar")); !os.IsNotExist(err) {
			t.Fatalf("filename %q escaped the server directory", name)
		}
	}
}

// uploadFile posts a multipart upload.
func (e *testEnv) uploadFile(t *testing.T, token, dir, name, content string) *http.Response {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("path", dir); err != nil {
		t.Fatalf("writing the path field: %v", err)
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("creating the file part: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("writing the file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		e.server.URL+"/api/v1/servers/"+testServerID+"/files/upload", &buf)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return resp
}

func TestMkdirMoveCopyDelete(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	base := "/api/v1/servers/" + testServerID

	created := e.do(http.MethodPost, base+"/files/mkdir", pathRequest{Path: "/backup"}, token)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir status = %d, want 201", created.StatusCode)
	}
	_ = created.Body.Close()

	moved := e.do(http.MethodPost, base+"/files/move",
		movePathsRequest{From: "/eula.txt", To: "/backup/eula.txt"}, token)
	if moved.StatusCode != http.StatusNoContent {
		t.Fatalf("move status = %d, want 204", moved.StatusCode)
	}
	_ = moved.Body.Close()

	copied := e.do(http.MethodPost, base+"/files/copy",
		movePathsRequest{From: "/world", To: "/backup/world"}, token)
	if copied.StatusCode != http.StatusNoContent {
		t.Fatalf("copy status = %d, want 204", copied.StatusCode)
	}
	_ = copied.Body.Close()

	if _, err := os.Stat(filepath.Join(e.api.serverDir(e.serverRecord), "backup", "world", "level.dat")); err != nil {
		t.Fatalf("the copied tree is incomplete: %v", err)
	}

	deleted := e.do(http.MethodDelete, base+"/files?path=/backup", nil, token)
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", deleted.StatusCode)
	}
	_ = deleted.Body.Close()

	if _, err := os.Stat(filepath.Join(e.api.serverDir(e.serverRecord), "backup")); !os.IsNotExist(err) {
		t.Fatal("the directory survived deletion")
	}
}

func TestArchiveAndUnarchiveThroughTheAPI(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodPost, base+"/files/archive",
		archiveRequest{Paths: []string{"/world", "/eula.txt"}, Dest: "/backup.zip"}, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("archive status = %d, want 201", resp.StatusCode)
	}
	if got := decodeJSON[archiveResponse](t, resp).Path; got != "/backup.zip" {
		t.Fatalf("archive path = %q", got)
	}

	extracted := e.do(http.MethodPost, base+"/files/unarchive",
		unarchiveRequest{Path: "/backup.zip", Destination: "/restored"}, token)
	if extracted.StatusCode != http.StatusNoContent {
		t.Fatalf("unarchive status = %d, want 204", extracted.StatusCode)
	}
	_ = extracted.Body.Close()

	if _, err := os.Stat(filepath.Join(e.api.serverDir(e.serverRecord), "restored", "world", "level.dat")); err != nil {
		t.Fatalf("the restored tree is incomplete: %v", err)
	}
}

// --- limits and scopes ---

func TestReadFileRefusesOversizedAndBinary(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	big := filepath.Join(e.api.serverDir(e.serverRecord), "huge.log")
	if err := os.WriteFile(big, make([]byte, fileman.MaxTextBytes+1), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}
	jar := filepath.Join(e.api.serverDir(e.serverRecord), "plugin.jar")
	if err := os.WriteFile(jar, []byte{0x50, 0x4B, 0x00, 0x01}, 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodGet, base+"/files/content?path=/huge.log", nil, token)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized read gave %d, want 413", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "file_too_large" {
		t.Errorf("error code = %q, want file_too_large", code)
	}

	binary := e.do(http.MethodGet, base+"/files/content?path=/plugin.jar", nil, token)
	if binary.StatusCode != http.StatusBadRequest {
		t.Errorf("binary read gave %d, want 400", binary.StatusCode)
	}
	_ = binary.Body.Close()

	// A binary file must still be downloadable — only the editor refuses it.
	download := e.do(http.MethodGet, base+"/files/download?path=/plugin.jar", nil, token)
	if download.StatusCode != http.StatusOK {
		t.Errorf("binary download gave %d, want 200", download.StatusCode)
	}
	_ = download.Body.Close()
}

// Reading must not require the write scope, and writing must require it.
func TestFileScopesAreSeparate(t *testing.T) {
	e := newTestEnv(t)
	e.seedFiles(t)

	readOnly := e.mintToken(e.user.ID, []string{ScopeServersRead, ScopeFilesRead})
	base := "/api/v1/servers/" + testServerID

	resp := e.do(http.MethodGet, base+"/files?path=/", nil, readOnly)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read with files:read gave %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	for _, attempt := range []func() *http.Response{
		func() *http.Response {
			return e.do(http.MethodPut, base+"/files/content?path=/eula.txt",
				writeContentRequest{Content: "x"}, readOnly)
		},
		func() *http.Response {
			return e.do(http.MethodDelete, base+"/files?path=/eula.txt", nil, readOnly)
		},
		func() *http.Response {
			return e.do(http.MethodPost, base+"/files/mkdir", pathRequest{Path: "/x"}, readOnly)
		},
	} {
		resp := attempt()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a write with only files:read gave %d, want 403", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// Another user's server must be invisible through the file API too.
func TestFileEndpointsHideOtherUsersServers(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+otherServerID+"/files?path=/", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Mutating file actions must leave an audit trail.
func TestFileActionsAreAudited(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	e.seedFiles(t)

	resp := e.do(http.MethodPut,
		"/api/v1/servers/"+testServerID+"/files/content?path=/eula.txt",
		writeContentRequest{Content: "eula=true\n"}, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	entries, err := e.db.Audit.List(t.Context(), e.user.ID, 20)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	for _, entry := range entries {
		if entry.Action == "file.write" {
			return
		}
	}
	t.Fatal("file.write is missing from the audit log")
}

// The listing must describe a symlink as a symlink rather than following it,
// so the panel cannot be made to show a file from outside the root.
func TestListingMarksSymlinks(t *testing.T) {
	e := newTestEnv(t)
	token := e.filesToken()
	outside := e.seedFiles(t)

	link := filepath.Join(e.api.serverDir(e.serverRecord), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/files?path=/", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[listFilesResponse](t, resp)
	for _, item := range body.Items {
		if item.Name == "escape" && item.Type != fileman.TypeSymlink {
			t.Fatalf("the symlink is typed %q, want %q", item.Type, fileman.TypeSymlink)
		}
	}

	// And it must not be traversable.
	through := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/files/content?path="+escapePath("/escape/passwords.txt"),
		nil, token)
	if through.StatusCode < 400 {
		t.Fatalf("reading through the symlink gave %d, want a refusal", through.StatusCode)
	}
	_ = through.Body.Close()
}
