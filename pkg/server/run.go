package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/gptscript-ai/go-gptscript"
)

const callTypeConfirm = "callConfirm"

// parse will parse the file and return the corresponding Document.
func parse(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, path, input string) {
	l.Debug("parsing file", "file", path, "input", input)
	var (
		out []gptscript.Node
		err error
	)

	if input != "" {
		out, err = gptscript.ParseTool(ctx, input)
	} else {
		out, err = gptscript.Parse(ctx, path, opts)
	}
	if err != nil {
		l.Error("failed to parse file", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to parse file: %w", err))
		return
	}

	writeResponse(w, map[string]any{"stdout": map[string]any{"nodes": out}})
}

// execTool runs the tool with the given options, and writes the output to the response.
func execTool(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, tool fmt.Stringer) {
	out, err := gptscript.ExecTool(ctx, opts, tool)
	if err != nil {
		l.Error("failed to execute tool", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to execute tool: %w", err))
		return
	}

	writeResponse(w, map[string]string{"stdout": out})
}

// execFile runs the file with the given options, and writes the output to the response.
func execFile(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, path, input string) {
	out, err := gptscript.ExecFile(ctx, path, input, opts)
	if err != nil {
		l.Error("failed to execute file", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to execute file: %w", err))
		return
	}

	writeResponse(w, map[string]string{"stdout": out})
}

// execToolStream runs the tool with the given options, and streams the stdout and stderr of the tool to the response as server sent events.
func execToolStream(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, tool fmt.Stringer) {
	stdout, stderr, wait := gptscript.StreamExecTool(ctx, opts, tool)
	processOutputStream(l, w, stdout, stderr, wait)
}

// execFile runs the file with the given options, and streams the stdout and stderr of the file to the response as server sent events.
func execFileStream(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, path, input string) {
	stdout, stderr, wait := gptscript.StreamExecFile(ctx, path, input, opts)
	processOutputStream(l, w, stdout, stderr, wait)
}

// execToolStreamWithEvents runs the tool with the given options, and streams the events to the response as server sent events.
func execToolStreamWithEvents(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, tool fmt.Stringer) {
	stdout, stderr, events, wait := gptscript.StreamExecToolWithEvents(ctx, opts, tool)
	processEventStreamOutput(l, w, stdout, stderr, events, wait)
}

// execFileStreamWithEvents runs the file with the given options, and streams the events to the response as server sent events.
func execFileStreamWithEvents(ctx context.Context, l *slog.Logger, w http.ResponseWriter, opts gptscript.Opts, path, input string) {
	stdout, stderr, events, wait := gptscript.StreamExecFileWithEvents(ctx, path, input, opts)
	processEventStreamOutput(l, w, stdout, stderr, events, wait)
}

// processOutputStream will stream the stdout and stderr of the tool to the response as server sent events.
func processOutputStream(l *slog.Logger, w http.ResponseWriter, stdout, stderr io.Reader, wait func() error) {
	setStreamingHeaders(w)

	lock := new(sync.Mutex)
	wg := new(sync.WaitGroup)
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamOutput(lock, l, w, stdout, "stdout")
	}()

	go func() {
		defer wg.Done()
		streamOutput(lock, l, w, stderr, "stderr")
	}()

	waitAndFinishStream(l, w, "", func() error {
		wg.Wait()
		return wait()
	})
}

// streamOutput will stream the output of the tool to the response as server sent events.
func streamOutput(lock *sync.Mutex, l *slog.Logger, w http.ResponseWriter, stream io.Reader, key string) {
	s := bufio.NewScanner(stream)
	s.Split(scan)
	for s.Scan() {
		if len(s.Bytes()) == 0 {
			continue
		}

		// Lock the mutex and write the event to ensure that only one event is written at a time.
		lock.Lock()
		writeServerSentEvent(l, w, map[string]string{key: s.Text()})
		lock.Unlock()

		l.Debug("wrote event", "event", s.Text(), "key", key)
	}
}

// processEventStreamOutput will stream the events of the tool to the response as server sent events.
// If an error occurs, then an event with the error will also be sent.
func processEventStreamOutput(l *slog.Logger, w http.ResponseWriter, stdout, stderr, events io.Reader, wait func() error) {
	setStreamingHeaders(w)

	streamEvents(l, w, events)

	// Read the output of the script.
	out, err := io.ReadAll(stdout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to read stdout: %w", err))
		return
	}

	stdErr, err := io.ReadAll(stderr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to read stderr: %w", err))
		return
	}

	writeServerSentEvent(l, w, map[string]any{
		"time":   time.Now(),
		"stderr": string(stdErr),
	})
	writeServerSentEvent(l, w, map[string]any{
		"time":   time.Now(),
		"stdout": string(out),
	})

	waitAndFinishStream(l, w, string(stdErr), wait)
}

// streamEvents will stream the events of the tool to the response as server sent events.
// This looks for and tries to handle confirm events as well. However, that currently is not implemented in the SDK.
func streamEvents(l *slog.Logger, w http.ResponseWriter, events io.Reader) {
	var (
		lastRunID   string
		eventBuffer []map[string]any
		buffer      = bufio.NewScanner(events)
	)

	l.Debug("receiving events")
	for buffer.Scan() {
		if len(buffer.Bytes()) == 0 {
			// If there is no event, then continue.
			continue
		}

		var e map[string]any
		err := json.Unmarshal(buffer.Bytes(), &e)
		if err != nil {
			l.Error("failed to unmarshal event", "error", err, "event", buffer.Text())
			continue
		}

		// Ensure that the callConfirm event is after an event with the same runID.
		if (len(eventBuffer) > 0 || e["type"] == callTypeConfirm) && lastRunID != e["runID"] {
			eventBuffer = append(eventBuffer, e)
			lastRunID = fmt.Sprint(e["runID"])
			continue
		}

		for _, ev := range eventBuffer {
			writeServerSentEvent(l, w, ev)
		}

		eventBuffer = nil
		lastRunID = fmt.Sprint(e["runID"])

		writeServerSentEvent(l, w, e)
	}

	l.Debug("done receiving events")
}

// waitAndFinishStream will wait for the tool to finish running, and will send any error events, if necessary.
// Finally, it will send the DONE event after everything has finished.
func waitAndFinishStream(l *slog.Logger, w http.ResponseWriter, stdErr string, wait func() error) {
	var execErrOutput string
	err := wait()
	if errors.Is(err, context.DeadlineExceeded) {
		execErrOutput = "The tool call took too long to complete, aborting"
	} else if execErr := new(exec.ExitError); errors.As(err, &execErr) {
		execErrOutput = fmt.Sprintf("The tool call returned an exit code of %d with message %q and output %q", execErr.ExitCode(), execErr.String(), stdErr)
	} else if err != nil {
		execErrOutput = fmt.Sprintf("failed to wait: %v, error output: %s", err, stdErr)
	}

	if execErrOutput != "" {
		writeServerSentEvent(l, w, map[string]any{
			"time": time.Now(),
			"err":  execErrOutput,
		})
	}

	// Now that we have received all events, send the DONE event.
	_, err = w.Write([]byte("data: [DONE]\n\n"))
	if err == nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	l.Debug("wrote DONE event")
}

func writeResponse(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to marshal response: %w", err))
		return
	}

	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.WriteHeader(code)
	resp := map[string]any{
		"error": err.Error(),
	}

	b, err := json.Marshal(resp)
	if err != nil {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error": "%s"}`, err.Error())))
		return
	}

	_, _ = w.Write(b)
}

func writeServerSentEvent(l *slog.Logger, w http.ResponseWriter, event any) {
	ev, err := json.Marshal(event)
	if err != nil {
		l.Warn("failed to marshal event", "error", err)
		return
	}

	_, err = w.Write([]byte(fmt.Sprintf("data: %s\n\n", ev)))
	if err == nil {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	l.Debug("wrote event", "event", string(ev))
}

func setStreamingHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// scan is a split function for a bufio.Scanner that returns whatever data is in the buffer.
func scan(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	return len(data), dropCR(data), nil
}

// dropCR drops a terminal \r from the data.
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}
