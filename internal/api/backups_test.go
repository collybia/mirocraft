package api

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

func (e *testEnv) backupToken() string {
	return e.mintToken(e.user.ID, []string{
		ScopeServersRead, ScopeBackupsRead, ScopeBackupsWrite,
	})
}

// seedForBackup fills the server directory with the mix a real one has: data
// worth keeping, and re-downloadable artifacts that must not be archived.
func (e *testEnv) seedForBackup(t *testing.T) string {
	t.Helper()

	dir := e.api.serverDir(e.serverRecord)
	files := map[string]string{
		"server.properties":      "motd=hi\n",
		"world/level.dat":        "world data",
		"world/region/r.0.0.mca": "region data",
		"plugins/Essentials.jar": "a plugin the operator installed",
		"ops.json":               "[]",

		// Re-downloadable: excluded from backups.
		"server.jar":              strings.Repeat("x", 4096),
		"libraries/net/thing.jar": "library",
		"versions/1.21.4/x.json":  "version metadata",
		"cache/blob":              "cache",
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
	return dir
}

// createBackup runs a backup and returns its finished record.
func (e *testEnv) createBackup(t *testing.T, token, note string) backupResponse {
	t.Helper()

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/backups",
		createBackupRequest{Note: note}, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	taskID := decodeJSON[taskAcceptedResponse](t, resp).TaskID

	task := e.awaitTask(taskID, token)
	if task.Status != TaskDone {
		t.Fatalf("the backup task failed: %s", task.Error)
	}

	list := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups", nil, token)
	items := decodeJSON[listResponse[backupResponse]](t, list).Items
	if len(items) == 0 {
		t.Fatal("no backups were recorded")
	}
	return items[0]
}

func TestCreateAndListBackup(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	record := e.createBackup(t, token, "before the update")

	if record.State != store.BackupDone {
		t.Fatalf("state = %q, want done", record.State)
	}
	if record.Note != "before the update" {
		t.Errorf("note = %q", record.Note)
	}
	if record.SizeBytes <= 0 {
		t.Errorf("size = %d, want a real archive", record.SizeBytes)
	}
}

// The jar and libraries are re-downloaded on start, so archiving them would
// multiply every backup's size for nothing.
func TestBackupExcludesRedownloadableArtifacts(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	record := e.createBackup(t, token, "")
	archive := e.api.backups.Path(testServerID, record.ID)

	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatalf("opening the archive: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var names []string
	for _, entry := range reader.File {
		names = append(names, entry.Name)
	}
	joined := strings.Join(names, "\n")

	for _, want := range []string{"server.properties", "world/level.dat", "plugins/Essentials.jar"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the archive is missing %q", want)
		}
	}
	for _, unwanted := range []string{"server.jar", "libraries/", "versions/", "cache/"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("the archive contains the re-downloadable %q", unwanted)
		}
	}
}

func TestDownloadBackup(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	record := e.createBackup(t, token, "")

	resp := e.do(http.MethodGet,
		"/api/v1/servers/"+testServerID+"/backups/"+record.ID+"/download", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if disp := resp.Header.Get("Content-Disposition"); !strings.Contains(disp, ".zip") {
		t.Errorf("Content-Disposition = %q", disp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	// A zip starts with PK.
	if len(body) < 4 || body[0] != 'P' || body[1] != 'K' {
		t.Fatalf("the download is not a zip (%d bytes)", len(body))
	}
}

// Restoring must return the directory to the archived state, not merge with
// whatever is there now.
func TestRestoreBackup(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	dir := e.seedForBackup(t)

	record := e.createBackup(t, token, "")

	// Change the world and add something that was not in the backup.
	if err := os.WriteFile(filepath.Join(dir, "world", "level.dat"), []byte("changed"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "afterwards.txt"), []byte("later"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	resp := e.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/backups/"+record.ID+"/restore", nil, token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore status = %d, want 202", resp.StatusCode)
	}
	task := e.awaitTask(decodeJSON[taskAcceptedResponse](t, resp).TaskID, token)
	if task.Status != TaskDone {
		t.Fatalf("the restore task failed: %s", task.Error)
	}

	restored, err := os.ReadFile(filepath.Join(dir, "world", "level.dat"))
	if err != nil {
		t.Fatalf("reading the restored world: %v", err)
	}
	if string(restored) != "world data" {
		t.Fatalf("level.dat = %q, want the archived contents", restored)
	}

	// A file created after the backup must be gone; a merge would leave it.
	if _, err := os.Stat(filepath.Join(dir, "afterwards.txt")); !os.IsNotExist(err) {
		t.Error("a file created after the backup survived the restore")
	}

	// The excluded artifacts are not in the archive and must survive.
	if _, err := os.Stat(filepath.Join(dir, "server.jar")); err != nil {
		t.Errorf("the jar was removed by the restore: %v", err)
	}
}

// Restoring wipes a directory the running server has open.
func TestRestoreRefusesWhileTheServerRuns(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	record := e.createBackup(t, token, "")
	e.startServer(testServerID)

	resp := e.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/backups/"+record.ID+"/restore", nil, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "server_already_running" {
		t.Fatalf("error code = %q", code)
	}
}

func TestDeleteBackup(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	record := e.createBackup(t, token, "")
	archive := e.api.backups.Path(testServerID, record.ID)

	resp := e.do(http.MethodDelete,
		"/api/v1/servers/"+testServerID+"/backups/"+record.ID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Error("the archive survived the deletion")
	}
	if _, err := e.db.Backups.GetByID(t.Context(), record.ID); err == nil {
		t.Error("the record survived the deletion")
	}
}

// Two archivers walking the same directory produce two half-caught archives.
func TestSecondBackupIsRefusedWhileOneRuns(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	// A record left in the running state, as an in-flight backup would leave.
	stuck := &store.Backup{ServerID: testServerID, State: store.BackupRunning}
	if err := e.db.Backups.Create(t.Context(), stuck); err != nil {
		t.Fatalf("creating the record: %v", err)
	}

	resp := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/backups",
		createBackupRequest{}, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "backup_in_progress" {
		t.Fatalf("error code = %q, want backup_in_progress", code)
	}
}

// A backup id from another server must not be reachable through this
// server's endpoints.
func TestBackupOfAnotherServerIsHidden(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	foreign := &store.Backup{ServerID: otherServerID, State: store.BackupDone}
	if err := e.db.Backups.Create(t.Context(), foreign); err != nil {
		t.Fatalf("creating the record: %v", err)
	}

	for _, path := range []string{
		"/api/v1/servers/" + testServerID + "/backups/" + foreign.ID + "/download",
	} {
		resp := e.do(http.MethodGet, path, nil, token)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s gave %d, want 404", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	resp := e.do(http.MethodDelete,
		"/api/v1/servers/"+testServerID+"/backups/"+foreign.ID, nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete gave %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- schedules ---

func TestScheduleRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	keep := 3
	enabled := true
	resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
		putScheduleRequest{Cron: "0 4 * * *", KeepLast: &keep, Enabled: &enabled}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[backupScheduleResponse](t, resp)
	if body.Cron != "0 4 * * *" || body.KeepLast != 3 || !body.Enabled {
		t.Fatalf("schedule = %+v", body)
	}
	if body.NextRunAt == nil || !body.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v, want a future time", body.NextRunAt)
	}

	read := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups/schedule", nil, token)
	if got := decodeJSON[backupScheduleResponse](t, read); got.Cron != "0 4 * * *" {
		t.Fatalf("read back = %+v", got)
	}
}

// A cron expression that cannot be parsed would be accepted and then silently
// never fire.
func TestScheduleRejectsBadCron(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	for _, expr := range []string{"", "not a cron", "99 * * * *", "* * *"} {
		resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
			putScheduleRequest{Cron: expr}, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("cron %q gave %d, want 400", expr, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestScheduleAcceptsDescriptors(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	for _, expr := range []string{"@daily", "@hourly", "@every 6h"} {
		resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
			putScheduleRequest{Cron: expr}, token)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("cron %q gave %d, want 200", expr, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestScheduleRejectsAbsurdRetention(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	for _, keep := range []int{0, -1, 100000} {
		k := keep
		resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
			putScheduleRequest{Cron: "@daily", KeepLast: &k}, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("keep_last %d gave %d, want 400", keep, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// A server with no schedule is a normal state, not an error.
func TestScheduleAbsentIsNotAnError(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups/schedule", nil, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeJSON[backupScheduleResponse](t, resp)
	if body.Enabled {
		t.Error("a server with no schedule reports one as enabled")
	}
}

// Retention must actually delete the oldest archives, or a nightly backup
// fills the disk.
func TestRetentionPrunesOldBackups(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	keep := 2
	enabled := true
	resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
		putScheduleRequest{Cron: "@daily", KeepLast: &keep, Enabled: &enabled}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting the schedule gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, e.createBackup(t, token, "").ID)
	}

	list := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups", nil, token)
	items := decodeJSON[listResponse[backupResponse]](t, list).Items
	if len(items) != keep {
		t.Fatalf("%d backups remain, want %d", len(items), keep)
	}

	// The two that remain must be the newest.
	remaining := map[string]bool{}
	for _, b := range items {
		remaining[b.ID] = true
	}
	for _, id := range ids[:2] {
		if remaining[id] {
			t.Errorf("the old backup %s was not pruned", id)
		}
	}

	// And their archives must be gone from disk, not just the records.
	for _, id := range ids[:2] {
		if _, err := os.Stat(e.api.backups.Path(testServerID, id)); !os.IsNotExist(err) {
			t.Errorf("the archive for the pruned backup %s is still on disk", id)
		}
	}
}

// The scheduler must fire a due backup and record that it did.
func TestSchedulerFiresDueBackups(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	keep := 5
	enabled := true
	resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
		putScheduleRequest{Cron: "@every 1s", KeepLast: &keep, Enabled: &enabled}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting the schedule gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Backdate the schedule so it is already due.
	schedule, err := e.db.Backups.GetSchedule(t.Context(), testServerID)
	if err != nil {
		t.Fatalf("reading the schedule: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	schedule.LastRunAt = &past
	if err := e.db.Backups.SetSchedule(t.Context(), schedule); err != nil {
		t.Fatalf("saving: %v", err)
	}
	if err := e.db.Backups.MarkScheduleRun(t.Context(), testServerID, past); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	e.api.fireDueBackups(t.Context(), time.Now())

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		list := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups", nil, token)
		items := decodeJSON[listResponse[backupResponse]](t, list).Items
		if len(items) > 0 && items[0].State == store.BackupDone {
			if items[0].Note != "scheduled" {
				t.Errorf("note = %q, want scheduled", items[0].Note)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the scheduler did not produce a backup")
}

// A schedule that is not yet due must not fire on every tick.
func TestSchedulerDoesNotFireEarly(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)

	enabled := true
	resp := e.do(http.MethodPut, "/api/v1/servers/"+testServerID+"/backups/schedule",
		putScheduleRequest{Cron: "0 4 * * *", Enabled: &enabled}, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting the schedule gave %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Just ran, so the next daily run is a long way off.
	if err := e.db.Backups.MarkScheduleRun(t.Context(), testServerID, time.Now()); err != nil {
		t.Fatalf("marking: %v", err)
	}

	e.api.fireDueBackups(t.Context(), time.Now())
	time.Sleep(200 * time.Millisecond)

	list := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups", nil, token)
	if items := decodeJSON[listResponse[backupResponse]](t, list).Items; len(items) != 0 {
		t.Fatalf("the scheduler produced %d backups when none was due", len(items))
	}
}

// Backup scopes must be separate from the file scopes.
func TestBackupScopes(t *testing.T) {
	e := newTestEnv(t)
	e.seedForBackup(t)

	readOnly := e.mintToken(e.user.ID, []string{ScopeServersRead, ScopeBackupsRead})

	resp := e.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/backups", nil, readOnly)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing with backups:read gave %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	created := e.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/backups",
		createBackupRequest{}, readOnly)
	if created.StatusCode != http.StatusForbidden {
		t.Fatalf("creating with only backups:read gave %d, want 403", created.StatusCode)
	}
	_ = created.Body.Close()
}

// A live Minecraft server holds world/session.lock open, and on Windows that
// makes it unreadable — which failed the whole backup the first time this ran
// against a real server.
func TestBackupSkipsUnreadableFilesInsteadOfFailing(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	dir := e.seedForBackup(t)

	// A file held open exclusively, the way a running server holds its lock.
	locked := filepath.Join(dir, "world", "session.lock")
	handle, err := os.OpenFile(locked, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatalf("creating the lock file: %v", err)
	}
	if _, err := handle.WriteString("locked"); err != nil {
		t.Fatalf("writing: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	record := e.createBackup(t, token, "with a locked file")
	if record.State != store.BackupDone {
		t.Fatalf("state = %q, want done — a locked file must not fail the backup", record.State)
	}

	// And the rest of the world must still be in there.
	reader, err := zip.OpenReader(e.api.backups.Path(testServerID, record.ID))
	if err != nil {
		t.Fatalf("opening the archive: %v", err)
	}
	defer func() { _ = reader.Close() }()

	found := false
	for _, entry := range reader.File {
		if entry.Name == "world/region/r.0.0.mca" {
			found = true
		}
	}
	if !found {
		t.Fatal("the archive is missing world data that was readable")
	}
}

// Saving must be switched back on after a backup, whatever happened, or the
// server silently loses everything since the backup when it next stops.
func TestBackupReenablesSaving(t *testing.T) {
	e := newTestEnv(t)
	token := e.backupToken()
	e.seedForBackup(t)
	e.startServer(testServerID)

	// The fake server echoes commands, so the console history records what it
	// was asked to do.
	e.createBackup(t, token, "")

	history, err := e.runner.History(t.Context(), testServerID, 200)
	if err != nil {
		t.Fatalf("reading the console: %v", err)
	}

	var sawOff, sawFlush, sawOn bool
	for _, line := range history {
		switch {
		case strings.Contains(line.Text, "echo: save-off"):
			sawOff = true
		case strings.Contains(line.Text, "echo: save-all"):
			sawFlush = true
		case strings.Contains(line.Text, "echo: save-on"):
			sawOn = true
		}
	}

	if !sawOff || !sawFlush {
		t.Errorf("the world was not quiesced before archiving (off=%v flush=%v)", sawOff, sawFlush)
	}
	if !sawOn {
		t.Error("saving was not re-enabled after the backup")
	}
}
