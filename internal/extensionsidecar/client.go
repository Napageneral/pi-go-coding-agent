package extensionsidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

var ErrToolNotFound = errors.New("extension tool not found")

type Options struct {
	Command string
	Args    []string
	CWD     string
	Env     []string
	Stderr  io.Writer
}

type requestEnvelope struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type responseEnvelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan responseEnvelope

	nextID int64

	doneOnce sync.Once
	done     chan struct{}
	doneErr  error

	notifyMu             sync.Mutex
	onExtensionUIRequest func(ExtensionUIRequest)
}

func Start(opts Options) (*Client, error) {
	if opts.Command == "" {
		return nil, errors.New("sidecar command is required")
	}

	cmd := exec.Command(opts.Command, opts.Args...)
	if opts.CWD != "" {
		cmd.Dir = opts.CWD
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[string]chan responseEnvelope{},
		done:    make(chan struct{}),
	}

	go c.readLoop(stdout)
	go c.waitLoop()
	return c, nil
}

func (c *Client) Initialize(ctx context.Context, req InitializeRequest) (InitializeResponse, error) {
	var resp InitializeResponse
	if err := c.call(ctx, "initialize", req, &resp); err != nil {
		return InitializeResponse{}, err
	}
	return resp, nil
}

func (c *Client) Emit(ctx context.Context, event Event) (EmitResponse, error) {
	var resp EmitResponse
	if err := c.call(ctx, "emit", EmitRequest{Event: event}, &resp); err != nil {
		return EmitResponse{}, err
	}
	return resp, nil
}

func (c *Client) ExecuteTool(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, error) {
	var result types.ToolResult
	req := ExecuteToolRequest{
		Name:       name,
		ToolCallID: callID,
		Arguments:  args,
	}
	err := c.call(ctx, "tool.execute", req, &result)
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && rpcErr.Code == "tool_not_found" {
			return types.ToolResult{IsError: true}, ErrToolNotFound
		}
		return types.ToolResult{IsError: true}, err
	}
	return result, nil
}

func (c *Client) ExecuteCommand(ctx context.Context, name, args string) (ExecuteCommandResponse, error) {
	return c.ExecuteCommandWithRequest(ctx, ExecuteCommandRequest{Name: name, Args: args})
}

func (c *Client) ExecuteCommandWithRequest(ctx context.Context, req ExecuteCommandRequest) (ExecuteCommandResponse, error) {
	var result ExecuteCommandResponse
	if err := c.call(ctx, "command.execute", req, &result); err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && rpcErr.Code == "command_not_found" {
			return ExecuteCommandResponse{Handled: false}, nil
		}
		return ExecuteCommandResponse{}, err
	}
	return result, nil
}

func (c *Client) SetExtensionUIRequestHandler(handler func(ExtensionUIRequest)) {
	c.notifyMu.Lock()
	c.onExtensionUIRequest = handler
	c.notifyMu.Unlock()
}

func (c *Client) RespondExtensionUI(ctx context.Context, response ExtensionUIResponse) error {
	response.ID = strings.TrimSpace(response.ID)
	if response.ID == "" {
		return errors.New("extension ui response id is required")
	}
	return c.call(ctx, "ui.respond", response, nil)
}

func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	_ = c.call(ctx, "shutdown", nil, nil)
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.done
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	id := strconv.FormatInt(atomic.AddInt64(&c.nextID, 1), 10)

	respCh := make(chan responseEnvelope, 1)
	c.pendingMu.Lock()
	if c.isDoneLocked() {
		err := c.doneErr
		c.pendingMu.Unlock()
		return c.closedError(err)
	}
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	req := requestEnvelope{
		ID:     id,
		Method: method,
		Params: params,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		c.removePending(id)
		return err
	}
	payload = append(payload, '\n')

	c.writeMu.Lock()
	_, err = c.stdin.Write(payload)
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(id)
		return err
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-c.done:
		c.removePending(id)
		return c.closedError(c.doneErr)
	}
}

func (c *Client) removePending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if req, ok := parseExtensionUIRequest(line); ok {
			c.notifyExtensionUIRequest(req)
			continue
		}
		var resp responseEnvelope
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.ID == "" {
			continue
		}
		c.pendingMu.Lock()
		ch := c.pending[resp.ID]
		if ch != nil {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	if err := scanner.Err(); err != nil {
		c.finish(err)
	}
}

func parseExtensionUIRequest(line []byte) (ExtensionUIRequest, bool) {
	var req ExtensionUIRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return ExtensionUIRequest{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(req.Type), "extension_ui_request") {
		return ExtensionUIRequest{}, false
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Method = strings.TrimSpace(req.Method)
	if req.ID == "" || req.Method == "" {
		return ExtensionUIRequest{}, false
	}
	return req, true
}

func (c *Client) notifyExtensionUIRequest(req ExtensionUIRequest) {
	c.notifyMu.Lock()
	handler := c.onExtensionUIRequest
	c.notifyMu.Unlock()
	if handler == nil {
		return
	}
	go handler(req)
}

func (c *Client) waitLoop() {
	err := c.cmd.Wait()
	c.finish(err)
}

func (c *Client) finish(err error) {
	c.doneOnce.Do(func() {
		if err == nil {
			err = io.EOF
		}
		c.pendingMu.Lock()
		c.doneErr = err
		for id, ch := range c.pending {
			delete(c.pending, id)
			ch <- responseEnvelope{ID: id, Error: &rpcError{Code: "sidecar_closed", Message: err.Error()}}
		}
		c.pendingMu.Unlock()
		close(c.done)
	})
}

func (c *Client) isDoneLocked() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) closedError(err error) error {
	if err == nil {
		err = io.EOF
	}
	return fmt.Errorf("extension sidecar closed: %w", err)
}

const defaultTimeout = 2 * time.Second
