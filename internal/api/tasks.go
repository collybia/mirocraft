package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// Task states from docs/API.md.
const (
	TaskQueued  = "queued"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// taskRetention is how long a finished task stays queryable. Long enough for a
// client to poll the result, short enough that the map stays small.
const taskRetention = 30 * time.Minute

// Task is an asynchronous operation a client can poll.
type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ServerID  string    `json:"server_id,omitempty"`
	UserID    string    `json:"-"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// taskRegistry tracks running operations in memory.
//
// Tasks are deliberately not persisted: they describe work owned by this
// process, and a task that was running when the daemon stopped is not
// resumable, so a row claiming otherwise after a restart would be a lie.
type taskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*Task

	// onFinish is called once a task settles, so the event bus can report it
	// without the registry having to know what a bus is.
	onFinish func(Task)
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{tasks: make(map[string]*Task)}
}

// start registers a task and runs fn in the background, recording the outcome.
func (reg *taskRegistry) start(kind, serverID, userID string, fn func(context.Context) error) *Task {
	now := time.Now().UTC()
	task := &Task{
		ID: store.NewID(), Kind: kind, ServerID: serverID, UserID: userID,
		Status: TaskQueued, CreatedAt: now, UpdatedAt: now,
	}

	reg.mu.Lock()
	reg.sweepLocked(now)
	reg.tasks[task.ID] = task
	reg.mu.Unlock()

	go func() {
		reg.update(task.ID, func(t *Task) {
			t.Status = TaskRunning
			t.Progress = 0
		})

		// The task outlives the request that created it, so it gets a fresh
		// context rather than the request's, which is cancelled on response.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		err := fn(ctx)

		reg.update(task.ID, func(t *Task) {
			t.Progress = 100
			if err != nil {
				t.Status = TaskFailed
				t.Error = err.Error()
				return
			}
			t.Status = TaskDone
		})

		if reg.onFinish != nil {
			finished, _ := reg.get(task.ID)
			reg.onFinish(finished)
		}
	}()

	return task
}

func (reg *taskRegistry) update(id string, mutate func(*Task)) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	task, ok := reg.tasks[id]
	if !ok {
		return
	}
	mutate(task)
	task.UpdatedAt = time.Now().UTC()
}

func (reg *taskRegistry) get(id string) (Task, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	task, ok := reg.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *task, true
}

// listForServer returns a server's tasks, newest first.
func (reg *taskRegistry) listForServer(serverID string) []Task {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var out []Task
	for _, task := range reg.tasks {
		if task.ServerID == serverID {
			out = append(out, *task)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// sweepLocked drops finished tasks past the retention window.
func (reg *taskRegistry) sweepLocked(now time.Time) {
	for id, task := range reg.tasks {
		finished := task.Status == TaskDone || task.Status == TaskFailed
		if finished && now.Sub(task.UpdatedAt) > taskRetention {
			delete(reg.tasks, id)
		}
	}
}

// --- handlers ---

type taskAcceptedResponse struct {
	TaskID string `json:"task_id"`
}

// handleGetTask serves GET /tasks/{id}.
func (a *API) handleGetTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}

	task, found := a.tasks.get(r.PathValue("id"))
	// Another user's task is reported as missing rather than forbidden, so
	// task ids cannot be probed.
	if !found || (task.UserID != principal.UserID && !principal.IsAdmin()) {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "task not found")
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// handleListServerTasks serves GET /servers/{id}/tasks.
func (a *API) handleListServerTasks(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	tasks := a.tasks.listForServer(serverID)
	if tasks == nil {
		tasks = []Task{}
	}
	writeJSON(w, http.StatusOK, listResponse[Task]{Items: tasks})
}
