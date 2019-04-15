package taskrunner

import (
	"context"
	"fmt"
	"time"

	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// NewDriverHandle returns a handle for task operations on a specific task
func NewDriverHandle(driver drivers.DriverPlugin, taskID string, task *structs.Task, net *drivers.DriverNetwork) *DriverHandle {
	return &DriverHandle{
		driver: driver,
		net:    net,
		taskID: taskID,
		task:   task,
	}
}

// DriverHandle encapsulates a driver plugin client and task identifier and exposes
// an api to perform driver operations on the task
type DriverHandle struct {
	driver drivers.DriverPlugin
	net    *drivers.DriverNetwork
	task   *structs.Task
	taskID string
}

func (h *DriverHandle) ID() string {
	return h.taskID
}

func (h *DriverHandle) WaitCh(ctx context.Context) (<-chan *drivers.ExitResult, error) {
	return h.driver.WaitTask(ctx, h.taskID)
}

func (h *DriverHandle) Update(task *structs.Task) error {
	return nil
}

func (h *DriverHandle) Kill() error {
	return h.driver.StopTask(h.taskID, h.task.KillTimeout, h.task.KillSignal)
}

func (h *DriverHandle) Stats(ctx context.Context, interval time.Duration) (<-chan *cstructs.TaskResourceUsage, error) {
	return h.driver.TaskStats(ctx, h.taskID, interval)
}

func (h *DriverHandle) Signal(s string) error {
	return h.driver.SignalTask(h.taskID, s)
}

func (h *DriverHandle) Exec(timeout time.Duration, cmd string, args []string) ([]byte, int, error) {
	command := append([]string{cmd}, args...)
	res, err := h.driver.ExecTask(h.taskID, command, timeout)
	if err != nil {
		return nil, 0, err
	}
	return res.Stdout, res.ExitResult.ExitCode, res.ExitResult.Err
}

func (h *DriverHandle) ExecStreaming(ctx context.Context,
	command []string,
	tty bool,
	requests <-chan *drivers.ExecTaskStreamingRequestMsg,
	responses chan<- *drivers.ExecTaskStreamingResponseMsg) error {

	if impl, ok := h.driver.(drivers.ExecTaskStreamingRaw); ok {
		return impl.ExecTaskStreamingRaw(ctx, h.taskID, command, tty, requests, responses)
	}

	execOpts, doneCh := drivers.StreamsToExecOptions(
		ctx, command, tty, requests, responses)

	result, err := h.driver.ExecTaskStreaming(ctx, h.taskID, execOpts)
	if err != nil {
		return err
	}

	execOpts.Stdout.Close()
	execOpts.Stderr.Close()

	select {
	case err = <-doneCh:
	case <-ctx.Done():
		err = fmt.Errorf("exec task timed out: %v", ctx.Err())
	}

	if err != nil {
		return err
	}

	responses <- drivers.NewExecStreamingResponseExit(result.ExitCode)
	close(responses)

	return nil
}

func (h *DriverHandle) Network() *drivers.DriverNetwork {
	return h.net
}
