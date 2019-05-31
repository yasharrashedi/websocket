package websocket

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/xerrors"
)

// Conn represents a WebSocket connection.
// All methods may be called concurrently.
//
// Please be sure to call Close on the connection when you
// are finished with it to release resources.
//
// Control (ping, pong, close) frames will be responded to in a separate goroutine
// so if you do not expect any data messages, you do not need
// to read from the connection. However, if the peer
// sends a data message, further pings, pongs and close frames will not
// be read if you do not read the message from the connection.
type Conn struct {
	subprotocol string
	br          *bufio.Reader
	bw          *bufio.Writer
	closer      io.Closer
	client      bool

	msgReadLimit int64

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}

	writeMsgLock   chan struct{}
	writeFrameLock chan struct{}

	readMsgLock   chan struct{}
	readFrameLock chan struct{}
	readMsg       chan header
	readMsgDone   chan struct{}

	setReadTimeout  chan context.Context
	setWriteTimeout chan context.Context
	setConnContext  chan context.Context
	getConnContext  chan context.Context

	activePingsMu sync.Mutex
	activePings   map[string]chan<- struct{}
}

// Context returns a context derived from parent that will be cancelled
// when the connection is closed or broken.
// If the parent context is cancelled, the connection will be closed.
//
// This is an experimental API that may be removed in the future.
// Please let me know how you feel about it in https://github.com/nhooyr/websocket/issues/79
func (c *Conn) Context(parent context.Context) context.Context {
	select {
	case <-c.closed:
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx
	case c.setConnContext <- parent:
	}

	select {
	case <-c.closed:
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx
	case ctx := <-c.getConnContext:
		return ctx
	}
}

func (c *Conn) close(err error) {
	c.closeOnce.Do(func() {
		runtime.SetFinalizer(c, nil)

		cerr := c.closer.Close()
		if err != nil {
			cerr = err
		}

		c.closeErr = xerrors.Errorf("websocket closed: %w", cerr)

		close(c.closed)

		// This ensures every goroutine that interacts
		// with the conn returns before it can actually do anything and
		// receives c.closeErr.
		c.readFrameLock <- struct{}{}
		c.writeFrameLock <- struct{}{}

		// See comment in dial.go
		if c.client {
			returnBufioReader(c.br)
			returnBufioWriter(c.bw)
		}
	})
}

// Subprotocol returns the negotiated subprotocol.
// An empty string means the default protocol.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

func (c *Conn) init() {
	c.closed = make(chan struct{})

	c.msgReadLimit = 32768

	c.writeMsgLock = make(chan struct{}, 1)
	c.writeFrameLock = make(chan struct{}, 1)

	c.readMsgLock = make(chan struct{}, 1)
	c.readFrameLock = make(chan struct{}, 1)
	c.readMsg = make(chan header)
	c.readMsgDone = make(chan struct{})

	c.setReadTimeout = make(chan context.Context)
	c.setWriteTimeout = make(chan context.Context)
	c.setConnContext = make(chan context.Context)
	c.getConnContext = make(chan context.Context)

	c.activePings = make(map[string]chan<- struct{})

	runtime.SetFinalizer(c, func(c *Conn) {
		c.close(xerrors.New("connection garbage collected"))
	})

	go c.timeoutLoop()
	go c.readLoop()
}

// We never mask inside here because our mask key is always 0,0,0,0.
// See comment on secWebSocketKey.
func (c *Conn) writeFrame(ctx context.Context, h header, p []byte) (err error) {
	err = c.acquireLock(ctx, c.writeFrameLock)
	if err != nil {
		return err
	}
	defer c.releaseLock(c.writeFrameLock)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.closeErr
	case c.setWriteTimeout <- ctx:
	}
	defer func() {
		// We have to remove the write timeout, even if ctx is cancelled.
		select {
		case <-c.closed:
			return
		case c.setWriteTimeout <- context.Background():
		}
	}()

	defer func() {
		if err != nil {
			// We need to always release the lock first before closing the connection to ensure
			// the lock can be acquired inside close.
			c.releaseLock(c.writeFrameLock)
			c.close(err)
		}
	}()

	h.masked = c.client
	h.payloadLength = int64(len(p))

	b2 := marshalHeader(h)
	_, err = c.bw.Write(b2)
	if err != nil {
		return xerrors.Errorf("failed to write to connection: %w", err)
	}
	_, err = c.bw.Write(p)
	if err != nil {
		return xerrors.Errorf("failed to write to connection: %w", err)

	}

	if h.fin {
		err := c.bw.Flush()
		if err != nil {
			return xerrors.Errorf("failed to write to connection: %w", err)
		}
	}

	return nil
}

func (c *Conn) timeoutLoop() {
	readCtx := context.Background()
	writeCtx := context.Background()
	parentCtx := context.Background()

	for {
		select {
		case <-c.closed:
			return
		case writeCtx = <-c.setWriteTimeout:
		case readCtx = <-c.setReadTimeout:
		case <-readCtx.Done():
			c.close(xerrors.Errorf("data read timed out: %w", readCtx.Err()))
		case <-writeCtx.Done():
			c.close(xerrors.Errorf("data write timed out: %w", writeCtx.Err()))
		case <-parentCtx.Done():
			c.close(xerrors.Errorf("parent context cancelled: %w", parentCtx.Err()))
			return
		case parentCtx = <-c.setConnContext:
			ctx, cancelCtx := context.WithCancel(parentCtx)
			defer cancelCtx()

			select {
			case <-c.closed:
				return
			case c.getConnContext <- ctx:
			}
		}
	}
}

func (c *Conn) handleControl(h header) {
	if h.payloadLength > maxControlFramePayload {
		c.Close(StatusProtocolError, "control frame too large")
		return
	}

	if !h.fin {
		c.Close(StatusProtocolError, "control frame cannot be fragmented")
		return
	}

	b := make([]byte, h.payloadLength)
	_, err := io.ReadFull(c.br, b)
	if err != nil {
		c.close(xerrors.Errorf("failed to read control frame payload: %w", err))
		return
	}

	if h.masked {
		fastXOR(h.maskKey, 0, b)
	}

	switch h.opcode {
	case opPing:
		c.writePong(b)
	case opPong:
		c.activePingsMu.Lock()
		pong, ok := c.activePings[string(b)]
		c.activePingsMu.Unlock()
		if ok {
			close(pong)
		}
	case opClose:
		ce, err := parseClosePayload(b)
		if err != nil {
			c.close(xerrors.Errorf("received invalid close payload: %w", err))
			return
		}
		if ce.Code == StatusNoStatusRcvd {
			c.writeClose(nil, ce)
		} else {
			c.Close(ce.Code, ce.Reason)
		}
	default:
		panic(fmt.Sprintf("websocket: unexpected control opcode: %#v", h))
	}
}

func (c *Conn) readTillMsg() (header, error) {
	for {
		h, err := c.readHeader()
		if err != nil {
			return header{}, err
		}

		if h.rsv1 || h.rsv2 || h.rsv3 {
			ce := CloseError{
				Code:   StatusProtocolError,
				Reason: fmt.Sprintf("received header with rsv bits set: %v:%v:%v", h.rsv1, h.rsv2, h.rsv3),
			}
			c.Close(ce.Code, ce.Reason)
			return header{}, ce
		}

		if h.opcode.controlOp() {
			c.handleControl(h)
			continue
		}

		switch h.opcode {
		case opBinary, opText, opContinuation:
			return h, nil
		default:
			ce := CloseError{
				Code:   StatusProtocolError,
				Reason: fmt.Sprintf("unknown opcode %v", h.opcode),
			}
			c.Close(ce.Code, ce.Reason)
			return header{}, ce
		}
	}
}

func (c *Conn) readHeader() (header, error) {
	err := c.acquireLock(context.Background(), c.readFrameLock)
	if err != nil {
		return header{}, err
	}
	defer c.releaseLock(c.readFrameLock)

	h, err := readHeader(c.br)
	if err != nil {
		return header{}, xerrors.Errorf("failed to read header: %w", err)
	}

	return h, nil
}

func (c *Conn) readLoop() {
	for {
		h, err := c.readTillMsg()
		if err != nil {
			c.close(err)
			return
		}

		if h.opcode == opContinuation &&
			h.fin &&
			h.payloadLength == 0 {
			c.releaseLock(c.readMsgLock)
		}

		select {
		case <-c.closed:
			return
		case c.readMsg <- h:
		}

		select {
		case <-c.closed:
			return
		case <-c.readMsgDone:
		}
	}
}

func (c *Conn) writePong(p []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	err := c.writeMessage(ctx, opPong, p)
	return err
}

// Close closes the WebSocket connection with the given status code and reason.
//
// It will write a WebSocket close frame with a timeout of 5 seconds.
// The connection can only be closed once. Additional calls to Close
// are no-ops.
//
// The maximum length of reason must be 125 bytes otherwise an internal
// error will be sent to the peer. For this reason, you should avoid
// sending a dynamic reason.
//
// Close will unblock all goroutines interacting with the connection.
func (c *Conn) Close(code StatusCode, reason string) error {
	err := c.exportedClose(code, reason)
	if err != nil {
		return xerrors.Errorf("failed to close connection: %w", err)
	}
	return nil
}

func (c *Conn) exportedClose(code StatusCode, reason string) error {
	ce := CloseError{
		Code:   code,
		Reason: reason,
	}

	// This function also will not wait for a close frame from the peer like the RFC
	// wants because that makes no sense and I don't think anyone actually follows that.
	// Definitely worth seeing what popular browsers do later.
	p, err := ce.bytes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "websocket: failed to marshal close frame: %v\n", err)
		ce = CloseError{
			Code: StatusInternalError,
		}
		p, _ = ce.bytes()
	}

	return c.writeClose(p, ce)
}

func (c *Conn) writeClose(p []byte, cerr CloseError) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	err := c.writeMessage(ctx, opClose, p)

	c.close(cerr)

	if err != nil {
		return err
	}

	if !xerrors.Is(c.closeErr, cerr) {
		return c.closeErr
	}

	return nil
}

func (c *Conn) acquireLock(ctx context.Context, lock chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.closeErr
	case lock <- struct{}{}:
		return nil
	}
}

func (c *Conn) releaseLock(lock chan struct{}) {
	// Allow multiple releases.
	select {
	case <-lock:
	default:
	}
}

func (c *Conn) writeMessage(ctx context.Context, opcode opcode, p []byte) error {
	if !opcode.controlOp() {
		err := c.acquireLock(ctx, c.writeMsgLock)
		if err != nil {
			return err
		}
		defer c.releaseLock(c.writeMsgLock)
	}

	err := c.writeFrame(ctx, header{
		fin:    true,
		opcode: opcode,
	}, p)
	if err != nil {
		return xerrors.Errorf("failed to write frame: %w", err)
	}
	return nil
}

// Writer returns a writer bounded by the context that will write
// a WebSocket message of type dataType to the connection.
//
// Ensure you close the writer once you have written the entire message.
// Concurrent calls to Writer are ok.
// Only one writer can be open at a time so Writer will block if there is
// another goroutine with an open writer until that writer is closed.
func (c *Conn) Writer(ctx context.Context, typ MessageType) (io.WriteCloser, error) {
	wc, err := c.writer(ctx, typ)
	if err != nil {
		return nil, xerrors.Errorf("failed to get writer: %w", err)
	}
	return wc, nil
}

func (c *Conn) writer(ctx context.Context, typ MessageType) (io.WriteCloser, error) {
	err := c.acquireLock(ctx, c.writeMsgLock)
	if err != nil {
		return nil, err
	}
	return &messageWriter{
		ctx:    ctx,
		opcode: opcode(typ),
		c:      c,
	}, nil
}

// Read is a convenience method to read a single message from the connection.
//
// See the Reader method if you want to be able to reuse buffers or want to stream a message.
//
// This is an experimental API, please let me know how you feel about it in
// https://github.com/nhooyr/websocket/issues/62
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	typ, r, err := c.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}

	b, err := ioutil.ReadAll(r)
	if err != nil {
		return typ, b, err
	}

	return typ, b, nil
}

// Write is a convenience method to write a message to the connection.
//
// See the Writer method if you want to stream a message.
//
// This is an experimental API, please let me know how you feel about it in
// https://github.com/nhooyr/websocket/issues/62
func (c *Conn) Write(ctx context.Context, typ MessageType, p []byte) error {
	return c.writeMessage(ctx, opcode(typ), p)
}

// messageWriter enables writing to a WebSocket connection.
type messageWriter struct {
	ctx    context.Context
	opcode opcode
	c      *Conn
	closed bool
}

// Write writes the given bytes to the WebSocket connection.
func (w *messageWriter) Write(p []byte) (int, error) {
	n, err := w.write(p)
	if err != nil {
		return n, xerrors.Errorf("failed to write: %w", err)
	}
	return n, nil
}

func (w *messageWriter) write(p []byte) (int, error) {
	if w.closed {
		return 0, xerrors.Errorf("cannot use closed writer")
	}
	err := w.c.writeFrame(w.ctx, header{
		opcode: w.opcode,
	}, p)
	if err != nil {
		return 0, err
	}
	w.opcode = opContinuation
	return len(p), nil
}

// Close flushes the frame to the connection.
// This must be called for every messageWriter.
func (w *messageWriter) Close() error {
	err := w.close()
	if err != nil {
		return xerrors.Errorf("failed to close writer: %w", err)
	}
	return nil
}

func (w *messageWriter) close() error {
	if w.closed {
		return xerrors.Errorf("cannot use closed writer")
	}
	w.closed = true

	err := w.c.writeFrame(w.ctx, header{
		fin:    true,
		opcode: w.opcode,
	}, nil)
	if err != nil {
		return err
	}

	w.c.releaseLock(w.c.writeMsgLock)
	return nil
}

// Reader will wait until there is a WebSocket data message to read from the connection.
// It returns the type of the message and a reader to read it.
// The passed context will also bound the reader.
//
// If you do not read from the reader till EOF, the connection will hang.
//
// You do not need to explicitly read from the connection to reply to control frames.
// Please see the docs on the Conn type.
func (c *Conn) Reader(ctx context.Context) (MessageType, io.Reader, error) {
	typ, r, err := c.reader(ctx)
	if err != nil {
		return 0, nil, xerrors.Errorf("failed to get reader: %w", err)
	}
	readLimit := atomic.LoadInt64(&c.msgReadLimit)
	return typ, &limitedReader{
		c:    c,
		r:    r,
		left: readLimit,
	}, nil
}

func (c *Conn) reader(ctx context.Context) (_ MessageType, _ io.Reader, err error) {
	err = c.acquireLock(ctx, c.readMsgLock)
	if err != nil {
		return 0, nil, err
	}

	select {
	case <-c.closed:
		return 0, nil, c.closeErr
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case h := <-c.readMsg:
		if h.opcode == opContinuation {
			ce := CloseError{
				Code:   StatusProtocolError,
				Reason: "continuation frame not after data or text frame",
			}
			c.Close(ce.Code, ce.Reason)
			return 0, nil, ce
		}
		return MessageType(h.opcode), &messageReader{
			ctx: ctx,
			h:   &h,
			c:   c,
		}, nil
	}
}

// messageReader enables reading a data frame from the WebSocket connection.
type messageReader struct {
	ctx     context.Context
	maskPos int
	h       *header
	c       *Conn
	eofed   bool
}

// Read reads as many bytes as possible into p.
func (r *messageReader) Read(p []byte) (int, error) {
	n, err := r.read(p)
	if err != nil {
		// Have to return io.EOF directly for now, we cannot wrap as xerrors
		// isn't used in stdlib.
		if xerrors.Is(err, io.EOF) {
			return n, io.EOF
		}
		return n, xerrors.Errorf("failed to read: %w", err)
	}
	return n, nil
}

func (r *messageReader) read(p []byte) (int, error) {
	if r.eofed {
		return 0, xerrors.Errorf("cannot use EOFed reader")
	}

	if r.h == nil {
		select {
		case <-r.c.closed:
			return 0, r.c.closeErr
		case h := <-r.c.readMsg:
			if h.opcode != opContinuation {
				ce := CloseError{
					Code:   StatusProtocolError,
					Reason: "cannot read new data frame when previous frame is not finished",
				}
				r.c.Close(ce.Code, ce.Reason)
				return 0, ce
			}
			r.h = &h
		}
	}

	if int64(len(p)) > r.h.payloadLength {
		p = p[:r.h.payloadLength]
	}

	select {
	case <-r.c.closed:
		return 0, r.c.closeErr
	case r.c.setReadTimeout <- r.ctx:
	}

	err := r.c.acquireLock(r.ctx, r.c.readFrameLock)
	if err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.c.br, p)
	r.c.releaseLock(r.c.readFrameLock)

	select {
	case <-r.c.closed:
		return 0, r.c.closeErr
	case r.c.setReadTimeout <- context.Background():
	}

	r.h.payloadLength -= int64(n)
	if r.h.masked {
		r.maskPos = fastXOR(r.h.maskKey, r.maskPos, p)
	}

	if err != nil {
		r.c.close(xerrors.Errorf("failed to read control frame payload: %w", err))
		return n, r.c.closeErr
	}

	if r.h.payloadLength == 0 {
		select {
		case <-r.c.closed:
			return n, r.c.closeErr
		case r.c.readMsgDone <- struct{}{}:
		}
		if r.h.fin {
			r.eofed = true
			r.c.releaseLock(r.c.readMsgLock)
			return n, io.EOF
		}
		r.maskPos = 0
		r.h = nil
	}

	return n, nil
}

// SetReadLimit sets the max number of bytes to read for a single message.
// It applies to the Reader and Read methods.
//
// By default, the connection has a message read limit of 32768 bytes.
//
// When the limit is hit, the connection will be closed with StatusPolicyViolation.
func (c *Conn) SetReadLimit(n int64) {
	atomic.StoreInt64(&c.msgReadLimit, n)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Ping sends a ping to the peer and waits for a pong.
// Use this to measure latency or ensure the peer is responsive.
//
// This API is experimental and subject to change.
// Please provide feedback in https://github.com/nhooyr/websocket/issues/1.
func (c *Conn) Ping(ctx context.Context) error {
	err := c.ping(ctx)
	if err != nil {
		return xerrors.Errorf("failed to ping: %w", err)
	}
	return nil
}

func (c *Conn) ping(ctx context.Context) error {
	id := rand.Uint64()
	p := strconv.FormatUint(id, 10)

	pong := make(chan struct{})

	c.activePingsMu.Lock()
	c.activePings[p] = pong
	c.activePingsMu.Unlock()

	defer func() {
		c.activePingsMu.Lock()
		delete(c.activePings, p)
		c.activePingsMu.Unlock()
	}()

	err := c.writeMessage(ctx, opPing, []byte(p))
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.closeErr
	case <-pong:
		return nil
	}
}
