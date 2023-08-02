package proclib

import (
	"fmt"
	"sync"
)

// Task records the unique coordinates of a task from the perspective of a Nomad
// client running the task, that is to say (alloc_id, task_name).
type Task struct {
	AllocID string
	Task    string
}

func (task Task) String() string {
	return fmt.Sprintf("%s/%s", task.AllocID[0:8], task.Task)
}

type create func(Task) ProcessWrangler

type Wranglers struct {
	configs *Configs
	create  create

	lock sync.Mutex
	m    map[Task]ProcessWrangler
}

// Setup any process management technique relavent to the operating system and
// its particular configuration.
func (w *Wranglers) Setup(task Task) error {
	w.configs.Logger.Trace("setup client process management", "task", task)

	// create process wrangler for task
	pw := w.create(task)

	// perform any initialization if necessary
	pw.Initialize()

	w.lock.Lock()
	defer w.lock.Unlock()

	// keep track of the process wrangler for task
	w.m[task] = pw

	return nil
}

// Destroy any processes still running that were spanwed by task. Ideally the
// task driver should be implemented well enough for this to not be necessary,
// but we protect the Client as best we can regardless.
func (w *Wranglers) Destroy(task Task) error {
	w.configs.Logger.Trace("destroy and cleanup remnant task processes", "task", task)

	w.lock.Lock()
	defer w.lock.Unlock()

	w.m[task].Kill()
	w.m[task].Cleanup()

	delete(w.m, task)

	return nil
}

// A ProcessWrangler "owns" a particular Task on a client, enabling the client
// to kill and cleanup processes created by that Task, without help from the
// task driver. Currently we have implementations only for Linux (via cgroups).
type ProcessWrangler interface {
	Initialize() error
	Kill() error
	Cleanup() error
}