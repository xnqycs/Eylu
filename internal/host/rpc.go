package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type reply struct {
	result json.RawMessage
	err    error
}
type frame struct {
	data []byte
	done chan error
}
type peer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	max     atomic.Int64
	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan reply
	out     chan frame
	err     chan error
}

func newPeer(ctx context.Context, output io.Writer) *peer {
	ctx, cancel := context.WithCancel(ctx)
	p := &peer{ctx: ctx, cancel: cancel, pending: map[string]chan reply{}, out: make(chan frame, 16), err: make(chan error, 1)}
	p.max.Store(MaxMessageBytes)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-p.out:
				n, err := output.Write(f.data)
				if err == nil && n != len(f.data) {
					err = io.ErrShortWrite
				}
				if f.done != nil {
					f.done <- err
				}
				if err != nil {
					p.fail(fault("HOST_UNAVAILABLE"))
					return
				}
			}
		}
	}()
	return p
}
func (p *peer) fail(err error) {
	select {
	case p.err <- err:
	default:
	}
	p.cancel()
}
func (p *peer) send(value any, flush bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fault("INVALID_PARAMS")
	}
	data = append(data, '\n')
	if int64(len(data)) > p.max.Load() {
		return fault("LIMIT_EXCEEDED")
	}
	f := frame{data: data}
	if flush {
		f.done = make(chan error, 1)
	}
	select {
	case <-p.ctx.Done():
		return fault("HOST_UNAVAILABLE")
	case p.out <- f:
	default:
		p.fail(fault("LIMIT_EXCEEDED"))
		return fault("LIMIT_EXCEEDED")
	}
	if flush {
		select {
		case err := <-f.done:
			return err
		case <-p.ctx.Done():
			return fault("HOST_UNAVAILABLE")
		}
	}
	return nil
}
func (p *peer) notify(method string, params any) error {
	raw, _ := json.Marshal(params)
	return p.send(envelope{JSONRPC: "2.0", Method: method, Params: raw}, false)
}
func (p *peer) call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, ch, err := p.beginCall(method, params)
	if err != nil {
		return err
	}
	return p.awaitCall(ctx, id, ch, result)
}
func (p *peer) beginCall(method string, params any) (string, chan reply, error) {
	id := fmt.Sprintf("e:%d", p.seq.Add(1))
	rawID, _ := json.Marshal(id)
	raw, _ := json.Marshal(params)
	ch := make(chan reply, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	if err := p.send(envelope{JSONRPC: "2.0", ID: rawID, Method: method, Params: raw}, false); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return "", nil, err
	}
	return id, ch, nil
}
func (p *peer) awaitCall(ctx context.Context, id string, ch chan reply, result any) error {
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return fault("HOST_UNAVAILABLE")
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result != nil {
			if err := decode(r.result, result); err != nil {
				return asFault(err, "INVALID_PARAMS")
			}
		}
		return nil
	}
}
func (p *peer) respond(id json.RawMessage, result any, rpcErr *RPCError, flush bool) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	e := envelope{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if rpcErr == nil {
		e.Result, _ = json.Marshal(result)
	}
	return p.send(e, flush)
}
func (p *peer) read(input io.Reader, dispatch func(envelope)) {
	r := bufio.NewReaderSize(input, 64<<10)
	for {
		var line []byte
		for {
			part, err := r.ReadSlice('\n')
			if int64(len(line)+len(part)) > p.max.Load() {
				p.fail(fault("LIMIT_EXCEEDED"))
				return
			}
			line = append(line, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if errors.Is(err, io.EOF) && len(line) == 0 {
					p.fail(io.EOF)
				} else {
					p.fail(fault("INVALID_REQUEST"))
				}
				return
			}
			break
		}
		if p.ctx.Err() != nil {
			return
		}
		var e envelope
		if !json.Valid(line) || !utf8.Valid(line) {
			_ = p.respond(nil, nil, fault("PARSE_ERROR"), false)
			continue
		}
		if decodeEnvelope(line, &e) != nil || e.JSONRPC != "2.0" {
			_ = p.respond(nil, nil, fault("INVALID_REQUEST"), false)
			continue
		}
		var id string
		if len(e.ID) > 0 && (json.Unmarshal(e.ID, &id) != nil || id == "") {
			_ = p.respond(nil, nil, fault("INVALID_REQUEST"), false)
			continue
		}
		if e.Method != "" {
			if (id != "" && !strings.HasPrefix(id, "h:")) || len(e.Result) > 0 || e.Error != nil || (len(e.Params) > 0 && !bytes.HasPrefix(bytes.TrimSpace(e.Params), []byte("{"))) {
				_ = p.respond(e.ID, nil, fault("INVALID_REQUEST"), false)
				continue
			}
			dispatch(e)
		} else {
			if !strings.HasPrefix(id, "e:") || len(e.Params) > 0 || (len(e.Result) > 0) == (e.Error != nil) {
				p.fail(fault("INVALID_REQUEST"))
				return
			}
			p.mu.Lock()
			ch := p.pending[id]
			p.mu.Unlock()
			if ch != nil {
				r := reply{result: e.Result}
				if e.Error != nil {
					r.err = e.Error
				}
				select {
				case ch <- r:
				default:
				}
			}
		}
	}
}

// The envelope owns only its top-level keys. Parameter validation belongs to
// the method, so even malformed nested parameters receive a matched RPC error.
func decodeEnvelope(raw []byte, e *envelope) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fault("INVALID_REQUEST")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return fault("INVALID_REQUEST")
		}
		seen[name] = true
		switch name {
		case "jsonrpc", "id", "method", "params", "result", "error":
		default:
			return fault("INVALID_REQUEST")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
	}
	if !seen["jsonrpc"] {
		return fault("INVALID_REQUEST")
	}
	return json.Unmarshal(raw, e)
}
