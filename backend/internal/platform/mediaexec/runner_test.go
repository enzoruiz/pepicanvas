package mediaexec

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

type beginFunc func(context.Context, media.Limits) (Execution, Result)

func (begin beginFunc) Begin(ctx context.Context, limits media.Limits) (Execution, Result) {
	return begin(ctx, limits)
}

type scriptedExecution struct {
	runResults  []Result
	closeResult Result
	commands    []Command
	closeCalls  int
}

func (execution *scriptedExecution) Run(command Command, _, _ io.Writer) Result {
	execution.commands = append(execution.commands, command)
	if len(execution.runResults) == 0 {
		return SuccessfulResult()
	}
	result := execution.runResults[0]
	execution.runResults = execution.runResults[1:]
	return result
}

func (execution *scriptedExecution) Close() Result {
	execution.closeCalls++
	return execution.closeResult
}

type typedNilExecution struct{}

func (*typedNilExecution) Run(Command, io.Writer, io.Writer) Result {
	panic("typed nil execution invoked")
}
func (*typedNilExecution) Close() Result { panic("typed nil execution closed") }

func TestResultValidationRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result Result
		valid  bool
	}{
		{name: "success", result: SuccessfulResult(), valid: true},
		{name: "supported failure", result: FailedResult(FailureMemoryLimit), valid: true},
		{name: "zero result", result: Result{}},
		{name: "zero failure kind", result: FailedResult(FailureKind(0))},
		{name: "unknown failure kind", result: FailedResult(FailureKind(255))},
		{name: "success carrying failure", result: Result{state: resultSuccess, failure: FailureTaskLimit}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.valid(); got != tt.valid {
				t.Fatalf("Result.valid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestGuardedExecutionRejectsUseAfterCloseAndDuplicateClose(t *testing.T) {
	t.Parallel()

	raw := &scriptedExecution{closeResult: SuccessfulResult()}
	execution, ok := guardExecution(raw)
	if !ok {
		t.Fatal("guardExecution() rejected valid execution")
	}
	if result := execution.Run(Command{}, io.Discard, io.Discard); !result.succeeded() {
		t.Fatalf("first Run() result = %#v, want success", result)
	}
	if result := execution.Close(); !result.succeeded() {
		t.Fatalf("first Close() result = %#v, want success", result)
	}
	if result := execution.Run(Command{}, io.Discard, io.Discard); result.failureKind() != FailureInternal {
		t.Fatalf("Run() after Close() = %#v, want internal failure", result)
	}
	if result := execution.Close(); result.failureKind() != FailureInternal {
		t.Fatalf("duplicate Close() = %#v, want internal failure", result)
	}
	if len(raw.commands) != 1 || raw.closeCalls != 1 {
		t.Fatalf("raw calls = %d runs/%d closes, want 1/1", len(raw.commands), raw.closeCalls)
	}
}

func TestInspectMapsEveryExecutionResultAtEveryLifecycleStage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failure FailureKind
		code    ErrorCode
	}{
		{name: "containment unavailable", failure: FailureContainmentUnavailable, code: ErrorContainmentUnavailable},
		{name: "containment setup", failure: FailureContainmentSetup, code: ErrorContainmentSetup},
		{name: "containment cleanup", failure: FailureContainmentCleanup, code: ErrorContainmentCleanup},
		{name: "timeout", failure: FailureTimeout, code: ErrorTimeout},
		{name: "cancellation", failure: FailureCancelled, code: ErrorCancelled},
		{name: "memory limit", failure: FailureMemoryLimit, code: ErrorMemoryLimit},
		{name: "task limit", failure: FailureTaskLimit, code: ErrorTaskLimit},
		{name: "CPU limit", failure: FailureCPULimit, code: ErrorCPULimit},
		{name: "tool exit", failure: FailureToolExit, code: ErrorCommandFailed},
		{name: "internal execution", failure: FailureInternal, code: ErrorExecutionFailed},
	}
	for _, stage := range []string{"begin", "run", "close"} {
		for _, tt := range tests {
			t.Run(stage+"/"+tt.name, func(t *testing.T) {
				config := testConfig(t)
				execution := &scriptedExecution{closeResult: SuccessfulResult()}
				beginResult := SuccessfulResult()
				switch stage {
				case "begin":
					beginResult = FailedResult(tt.failure)
				case "run":
					execution.runResults = []Result{FailedResult(tt.failure)}
				case "close":
					execution.closeResult = FailedResult(tt.failure)
				}
				runner := beginFunc(func(context.Context, media.Limits) (Execution, Result) {
					if stage == "begin" {
						return nil, beginResult
					}
					return execution, beginResult
				})
				adapter, err := New(config, runner)
				if err != nil {
					t.Fatal(err)
				}
				_, err = adapter.Inspect(context.Background(), strings.NewReader("x"))
				assertAdapterError(t, err, tt.code)
			})
		}
	}
}

func TestInspectUsesOneExecutionWithLimitsContextAndSequentialCommands(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	parent := context.WithValue(context.Background(), struct{}{}, "identity")
	execution := &scriptedExecution{closeResult: SuccessfulResult()}
	beginCalls := 0
	// Supply valid metadata from the first command while recording exact order.
	executionWithOutput := &outputExecution{scriptedExecution: execution, output: imageProbe()}
	runner := beginFunc(func(ctx context.Context, limits media.Limits) (Execution, Result) {
		beginCalls++
		if ctx.Value(struct{}{}) != "identity" || !reflect.DeepEqual(limits, config.Limits) {
			t.Fatal("Begin did not receive the inspection context and selected limits")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("Begin context has no inspection-wide deadline")
		}
		return executionWithOutput, SuccessfulResult()
	})
	adapter, err := New(config, runner)
	if err != nil {
		t.Fatal(err)
	}
	beginCalls = 0
	if _, err := adapter.Inspect(parent, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if beginCalls != 1 || execution.closeCalls != 1 {
		t.Fatalf("lifecycle calls = %d begins/%d closes, want 1/1", beginCalls, execution.closeCalls)
	}
	if len(execution.commands) != 2 || execution.commands[0].Path != config.FFprobePath || execution.commands[1].Path != config.FFmpegPath {
		t.Fatalf("commands = %#v, want sequential ffprobe then ffmpeg", execution.commands)
	}
}

type callbackExecution struct {
	run        func(int, Command, io.Writer, io.Writer) Result
	runCalls   int
	closeCalls int
}

func (execution *callbackExecution) Run(command Command, stdout, stderr io.Writer) Result {
	execution.runCalls++
	return execution.run(execution.runCalls, command, stdout, stderr)
}

func (execution *callbackExecution) Close() Result {
	execution.closeCalls++
	return SuccessfulResult()
}

type outputExecution struct {
	*scriptedExecution
	output string
}

func (execution *outputExecution) Run(command Command, stdout, stderr io.Writer) Result {
	result := execution.scriptedExecution.Run(command, stdout, stderr)
	if result.succeeded() && command.Path != "" && !contains(command.Args, "-f") {
		_, _ = io.WriteString(stdout, execution.output)
	}
	return result
}

func TestInspectStopsAfterFailureAndAlwaysClosesAfterBegin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		source    io.Reader
		execution *scriptedExecution
	}{
		{name: "staging failure", source: failingReader{}, execution: &scriptedExecution{closeResult: SuccessfulResult()}},
		{name: "run failure", source: strings.NewReader("x"), execution: &scriptedExecution{runResults: []Result{FailedResult(FailureToolExit)}, closeResult: SuccessfulResult()}},
		{name: "parse failure", source: strings.NewReader("x"), execution: &scriptedExecution{closeResult: SuccessfulResult()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig(t)
			adapter, err := New(config, beginFunc(func(context.Context, media.Limits) (Execution, Result) {
				return tt.execution, SuccessfulResult()
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = adapter.Inspect(context.Background(), tt.source)
			if tt.execution.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", tt.execution.closeCalls)
			}
			if len(tt.execution.commands) > 1 {
				t.Fatalf("commands = %d, want no command after first failure", len(tt.execution.commands))
			}
		})
	}
}

func TestInspectClosesExecutionOnEveryPostBeginEarlyExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*Config)
		run       func(int, Command, io.Writer, io.Writer) Result
		wantRuns  int
	}{
		{
			name: "probe parse failure",
			run: func(_ int, _ Command, stdout, _ io.Writer) Result {
				_, _ = io.WriteString(stdout, "not JSON")
				return SuccessfulResult()
			},
			wantRuns: 1,
		},
		{
			name:      "domain validation failure",
			configure: func(config *Config) { config.Limits.MaxWidth = 10 },
			run: func(_ int, _ Command, stdout, _ io.Writer) Result {
				_, _ = io.WriteString(stdout, strings.Replace(imageProbe(), `"width":10`, `"width":11`, 1))
				return SuccessfulResult()
			},
			wantRuns: 1,
		},
		{
			name: "GIF frame parse failure",
			run: func(call int, _ Command, stdout, _ io.Writer) Result {
				if call == 1 {
					_, _ = io.WriteString(stdout, gifProbe())
				} else {
					_, _ = io.WriteString(stdout, `{"frames":`)
				}
				return SuccessfulResult()
			},
			wantRuns: 2,
		},
		{
			name: "complete decode failure",
			run: func(call int, _ Command, stdout, _ io.Writer) Result {
				if call == 1 {
					_, _ = io.WriteString(stdout, imageProbe())
					return SuccessfulResult()
				}
				return FailedResult(FailureToolExit)
			},
			wantRuns: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig(t)
			if tt.configure != nil {
				tt.configure(&config)
			}
			execution := &callbackExecution{run: tt.run}
			adapter, err := New(config, beginFunc(func(context.Context, media.Limits) (Execution, Result) {
				return execution, SuccessfulResult()
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Inspect(context.Background(), strings.NewReader("x")); err == nil {
				t.Fatal("Inspect() error = nil, want early-exit failure")
			}
			if execution.runCalls != tt.wantRuns || execution.closeCalls != 1 {
				t.Fatalf("lifecycle calls = %d runs/%d closes, want %d/1", execution.runCalls, execution.closeCalls, tt.wantRuns)
			}
		})
	}
}

func TestInspectFailsClosedForNilExecutionAndMalformedResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		execution Execution
		result    Result
	}{
		{name: "nil execution", execution: nil, result: SuccessfulResult()},
		{name: "typed nil execution", execution: (*typedNilExecution)(nil), result: SuccessfulResult()},
		{name: "zero result", execution: &scriptedExecution{}, result: Result{}},
		{name: "unknown failure", execution: nil, result: FailedResult(FailureKind(255))},
		{name: "failure with execution", execution: &scriptedExecution{}, result: FailedResult(FailureContainmentSetup)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig(t)
			adapter, err := New(config, beginFunc(func(context.Context, media.Limits) (Execution, Result) {
				return tt.execution, tt.result
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Inspect(context.Background(), strings.NewReader("private input"))
			assertAdapterError(t, err, ErrorExecutionFailed)
		})
	}
}

func TestInspectFailsClosedForMalformedResultAtEveryLifecycleStage(t *testing.T) {
	t.Parallel()

	for _, stage := range []string{"begin", "run", "close"} {
		t.Run(stage, func(t *testing.T) {
			config := testConfig(t)
			execution := &scriptedExecution{closeResult: SuccessfulResult()}
			beginResult := SuccessfulResult()
			switch stage {
			case "begin":
				beginResult = Result{}
			case "run":
				execution.runResults = []Result{{}}
			case "close":
				execution.closeResult = Result{}
			}
			adapter, err := New(config, beginFunc(func(context.Context, media.Limits) (Execution, Result) {
				if stage == "begin" {
					return nil, beginResult
				}
				return execution, beginResult
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Inspect(context.Background(), strings.NewReader("x"))
			assertAdapterError(t, err, ErrorExecutionFailed)
		})
	}
}

func TestCloseFailureOverridesEarlierFailureWithoutSensitiveLeakage(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	execution := &scriptedExecution{
		runResults:  []Result{FailedResult(FailureToolExit)},
		closeResult: FailedResult(FailureContainmentCleanup),
	}
	adapter, err := New(config, beginFunc(func(context.Context, media.Limits) (Execution, Result) {
		return execution, SuccessfulResult()
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Inspect(context.Background(), strings.NewReader("/private/input secret stderr"))
	assertAdapterError(t, err, ErrorContainmentCleanup)
	for _, secret := range []string{"/private/input", "secret", "stderr"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("public error %q contains sensitive value %q", err, secret)
		}
	}
}
