// Package rpc is the wire protocol between horch (and the plugin panes) and the daemon:
// newline-delimited JSON over $STATE/orch.sock, one request per connection. Long-polling
// requests (check --wait, ask) simply hold the connection open.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"
)

type Request struct {
	Op     string          `json:"op"`
	Caller string          `json:"caller"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// ErrUnavailable means the daemon was never reached: the request was not delivered.
var ErrUnavailable = errors.New("daemon unavailable")

// ErrLost means the connection dropped after the request was sent (e.g. the daemon
// restarted): the request may or may not have been carried out.
var ErrLost = errors.New("connection to daemon lost after the request was sent")

// Stream sends a streaming request and calls fn with each result line until the daemon
// ends the stream, fn returns an error, or ctx is done. A daemon that goes away mid-stream
// yields ErrLost; the caller reconnects (streams are keyed by a cursor, so this is safe).
func Stream(ctx context.Context, sock, op, caller string, args any, fn func(json.RawMessage) error) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	conn, err := d.DialContext(dctx, "unix", sock)
	cancel()
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	req, _ := json.Marshal(Request{Op: op, Caller: caller, Args: raw})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.Join(ErrLost, err)
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if !resp.OK {
			if resp.Error == nil {
				resp.Error = &Error{Code: "error", Message: "unknown error"}
			}
			return resp.Error
		}
		if err := fn(resp.Result); err != nil {
			return err
		}
	}
}

// Call sends one request to the daemon at sock and decodes the result into out.
func Call(ctx context.Context, sock, op, caller string, args any, out any) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	conn, err := d.DialContext(dctx, "unix", sock)
	cancel()
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	req, _ := json.Marshal(Request{Op: op, Caller: caller, Args: raw})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	line, err := bufio.NewReaderSize(conn, 1<<20).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrLost, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if !resp.OK {
		if resp.Error == nil {
			resp.Error = &Error{Code: "error", Message: "unknown error"}
		}
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}
