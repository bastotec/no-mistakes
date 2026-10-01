package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const repairAgentDescriptorEnv = "NM_REPAIR_AGENT_DESCRIPTOR"
const repairRunEnvEnv = "NM_REPAIR_RUN_ENV"
const repairWrapperTokenEnv = "NM_REPAIR_WRAPPER_TOKEN"

type repairInvocationDescriptor struct {
	Prompt                string                    `json:"prompt"`
	CWD                   string                    `json:"cwd"`
	JSONSchema            json.RawMessage           `json:"json_schema,omitempty"`
	Session               *agent.SessionRef         `json:"session,omitempty"`
	SessionFallback       bool                      `json:"session_fallback,omitempty"`
	Purpose               string                    `json:"purpose"`
	SessionFallbackReason string                    `json:"session_fallback_reason,omitempty"`
	Workload              *agent.InvocationWorkload `json:"workload,omitempty"`
	DeadlineUnixNano      int64                     `json:"deadline_unix_nano,omitempty"`
	DeadlineClass         string                    `json:"deadline_class,omitempty"`
}

type repairInvocationResultFile struct {
	Result        *agent.Result `json:"result,omitempty"`
	ResultPresent bool          `json:"result_present"`
	ErrorClass    string        `json:"error_class,omitempty"`
	ErrorMessage  string        `json:"error_message,omitempty"`
}

type repairWrapperHeartbeat struct {
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func repairWrapperHeartbeatPath(resultPath string) string {
	return resultPath + ".heartbeat"
}

func repairWrapperIdentityLive(token string, pid int, resultPath string) bool {
	path := repairWrapperHeartbeatPath(resultPath)
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > 3*time.Second {
		return false
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var heartbeat repairWrapperHeartbeat
	return json.Unmarshal(payload, &heartbeat) == nil && heartbeat.Token == token && heartbeat.PID == pid
}

func writePrivateJSON(path string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func repairProcessToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

var ErrDaemonShutdown = fmt.Errorf("daemon shutting down")

func launchIndependentRepair(ctx context.Context, database *db.DB, root, invocationID string, inner agent.Agent, opts agent.RunOpts) (*agent.Result, error) {
	descriptor, environment, err := agent.DescribeRepairAgent(inner, opts.Purpose)
	if err != nil {
		return nil, err
	}
	factoryJSON, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	runEnvJSON, err := json.Marshal(opts.Env)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "repair-invocations")
	descriptorPath := filepath.Join(dir, invocationID+".json")
	resultPath := filepath.Join(dir, invocationID+".result.json")
	wire := repairInvocationDescriptor{
		Prompt: opts.Prompt, CWD: opts.CWD, JSONSchema: opts.JSONSchema,
		Session: opts.Session, SessionFallback: opts.SessionFallback, Purpose: opts.Purpose,
		SessionFallbackReason: opts.SessionFallbackReason, Workload: opts.Workload,
	}
	if deadline, ok := ctx.Deadline(); ok {
		wire.DeadlineUnixNano = deadline.UnixNano()
		wire.DeadlineClass = "agent_timeout"
		if opts.Purpose == "review-fix" {
			wire.DeadlineClass = "review_agent_timeout"
		}
	}
	if err := writePrivateJSON(descriptorPath, wire); err != nil {
		return nil, fmt.Errorf("write repair invocation descriptor: %w", err)
	}
	if err := database.SetRepairInvocationFiles(invocationID, descriptorPath, resultPath); err != nil {
		return nil, err
	}
	token, err := repairProcessToken()
	if err != nil {
		return nil, err
	}
	if err := database.PrepareRepairInvocationWrapper(invocationID, token); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, "repair-invocation-wrapper", invocationID)
	cmd.Env = append(environment.Apply(nil),
		repairAgentDescriptorEnv+"="+base64.RawStdEncoding.EncodeToString(factoryJSON),
		repairRunEnvEnv+"="+base64.RawStdEncoding.EncodeToString(runEnvJSON),
		repairWrapperTokenEnv+"="+token,
	)
	cmd.Dir = opts.CWD
	shellenv.ConfigureShellCommand(cmd)
	if err := shellenv.StartShellCommand(cmd); err != nil {
		if resetErr := database.ResetUnstartedRepairInvocation(invocationID); resetErr != nil {
			return nil, fmt.Errorf("start repair invocation wrapper: %w (reset failed: %v)", err, resetErr)
		}
		return nil, fmt.Errorf("start repair invocation wrapper: %w", err)
	}
	if opts.OnLifecycle != nil {
		opts.OnLifecycle(agent.LifecycleEvent{Agent: inner.Name(), Phase: agent.LifecyclePhaseStart, PID: cmd.Process.Pid, Message: "repair invocation wrapper started"})
	}
	waited := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		shellenv.TerminateShellCommandGroup(cmd)
		waited <- waitErr
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	cancelRequested := false
	for {
		invocation, readErr := database.GetRepairInvocation(invocationID)
		if readErr != nil {
			return nil, fmt.Errorf("read repair invocation result: %w", readErr)
		}
		if invocation.State == "terminal" {
			if waitErr := <-waited; waitErr != nil {
				return nil, fmt.Errorf("repair invocation wrapper exited after publishing a result: %w", waitErr)
			}
			return repairInvocationResult(invocation)
		}
		select {
		case waitErr := <-waited:
			if _, recoveredErr := database.RecoverRepairInvocationResultByID(invocationID); recoveredErr != nil {
				return nil, recoveredErr
			}
			invocation, readErr = database.GetRepairInvocation(invocationID)
			if readErr == nil && invocation.State == "terminal" {
				return repairInvocationResult(invocation)
			}
			if waitErr != nil {
				return nil, fmt.Errorf("repair invocation wrapper exited before publishing a result: %w", waitErr)
			}
			return nil, fmt.Errorf("repair invocation wrapper exited before publishing a result")
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if errors.Is(cause, ErrDaemonShutdown) {
				return nil, ErrDaemonShutdown
			}
			if !cancelRequested {
				if err := database.RequestRepairInvocationCancellation(invocationID); err != nil {
					return nil, fmt.Errorf("request repair cancellation: %w", err)
				}
				cancelRequested = true
			}
		case <-ticker.C:
		}
	}
}

func repairInvocationResult(invocation *db.RepairInvocation) (*agent.Result, error) {
	var result *agent.Result
	if invocation.ResultPresent {
		result = &agent.Result{}
		if err := json.Unmarshal(invocation.ResultJSON, result); err != nil {
			return nil, err
		}
	}
	if invocation.ErrorText != nil {
		return result, restoreRepairInvocationError(invocation.ErrorClass, *invocation.ErrorText)
	}
	return result, nil
}

func RunRepairInvocationWrapper(invocationID string) error {
	p, err := paths.New()
	if err != nil {
		return err
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return err
	}
	defer database.Close()
	descriptorPath, resultPath, err := database.RepairInvocationFiles(invocationID)
	if err != nil {
		return err
	}
	payload, err := os.ReadFile(descriptorPath)
	if err != nil {
		return err
	}
	var invocation repairInvocationDescriptor
	if err := json.Unmarshal(payload, &invocation); err != nil {
		return err
	}
	factoryPayload, err := base64.RawStdEncoding.DecodeString(os.Getenv(repairAgentDescriptorEnv))
	if err != nil {
		return err
	}
	var factory agent.RepairAgentDescriptor
	if err := json.Unmarshal(factoryPayload, &factory); err != nil {
		return err
	}
	runEnvPayload, err := base64.RawStdEncoding.DecodeString(os.Getenv(repairRunEnvEnv))
	if err != nil {
		return err
	}
	var runEnvironment []string
	if err := json.Unmarshal(runEnvPayload, &runEnvironment); err != nil {
		return err
	}
	token := os.Getenv(repairWrapperTokenEnv)
	if err := database.BindRepairInvocationWrapper(invocationID, token, os.Getpid()); err != nil {
		return err
	}
	heartbeatPath := repairWrapperHeartbeatPath(resultPath)
	heartbeat := repairWrapperHeartbeat{Token: token, PID: os.Getpid()}
	if err := writePrivateJSON(heartbeatPath, heartbeat); err != nil {
		return err
	}
	heartbeatDone := make(chan struct{})
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				_ = writePrivateJSON(heartbeatPath, heartbeat)
			}
		}
	}()
	defer func() {
		close(heartbeatDone)
		<-heartbeatStopped
		_ = os.Remove(heartbeatPath)
	}()
	ag, err := agent.BuildRepairAgent(factory)
	if err != nil {
		return err
	}
	opts := agent.RunOpts{
		Prompt: invocation.Prompt, Env: runEnvironment, CWD: invocation.CWD, JSONSchema: invocation.JSONSchema,
		Session: invocation.Session, SessionFallback: invocation.SessionFallback, Purpose: invocation.Purpose,
		SessionFallbackReason: invocation.SessionFallbackReason, Workload: invocation.Workload,
		OnLifecycle: func(event agent.LifecycleEvent) {
			if event.Phase == agent.LifecyclePhaseStart {
				activity := event.Message
				if activity == "" {
					activity = fmt.Sprintf("%s %s", event.Agent, event.Phase)
				}
				_ = database.BindRepairInvocationProcess(invocationID, activity, event.PID)
			}
		},
	}
	runCtx, cancelCause := context.WithCancelCause(context.Background())
	cancelDeadline := func() {}
	if invocation.DeadlineUnixNano != 0 {
		deadlineCause := ErrAgentTimeout
		if invocation.DeadlineClass == "review_agent_timeout" {
			deadlineCause = ErrReviewAgentTimeout
		}
		var deadlineCtx context.Context
		deadlineCtx, cancelDeadline = context.WithDeadlineCause(runCtx, time.Unix(0, invocation.DeadlineUnixNano), deadlineCause)
		runCtx = deadlineCtx
	}
	defer cancelDeadline()
	controlDone := make(chan struct{})
	controlStopped := make(chan struct{})
	go func() {
		defer close(controlStopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-controlDone:
				return
			case <-ticker.C:
				requested, requestErr := database.RepairInvocationCancellationRequested(invocationID)
				if requestErr == nil && requested {
					cancelCause(context.Canceled)
					return
				}
			}
		}
	}()
	if requested, requestErr := database.RepairInvocationCancellationRequested(invocationID); requestErr != nil {
		return requestErr
	} else if requested {
		cancelCause(context.Canceled)
	}
	result, runErr := ag.Run(runCtx, opts)
	close(controlDone)
	<-controlStopped
	if cause := context.Cause(runCtx); cause != nil {
		runErr = cause
	}
	if closeErr := ag.Close(); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	cancelCause(nil)
	class, message := "", ""
	if runErr != nil {
		class = repairInvocationErrorClass(runErr)
		message = intent.RedactSecrets(runErr.Error())
	}
	resultFile := repairInvocationResultFile{Result: result, ResultPresent: result != nil, ErrorClass: class, ErrorMessage: message}
	if err := writePrivateJSON(resultPath, resultFile); err != nil {
		return err
	}
	var resultJSON []byte
	if result != nil {
		resultJSON, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	var classPtr, messagePtr *string
	if runErr != nil {
		classPtr, messagePtr = &class, &message
	}
	return database.FinishRepairInvocation(invocationID, resultJSON, result != nil, classPtr, messagePtr)
}
