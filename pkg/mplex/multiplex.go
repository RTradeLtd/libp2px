package multiplex

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	pool "github.com/RTradeLtd/libp2px/pkg/buffer-pool"
	"go.uber.org/zap"
)

// MaxMessageSize defines the maximum message size
var MaxMessageSize = 1 << 20

// ReceiveTimeout is time to block waiting for a slow reader to read from a stream before
// resetting it. Preferably, we'd have some form of back-pressure mechanism but
// we don't have that in this protocol.
var ReceiveTimeout = 5 * time.Second

// ErrShutdown is returned when operating on a shutdown session
var ErrShutdown = errors.New("session shut down")

// ErrTwoInitiators is returned when both sides think they're the initiator
var ErrTwoInitiators = errors.New("two initiators")

// ErrInvalidState is returned when the other side does something it shouldn't.
// In this case, we close the connection to be safe.
var ErrInvalidState = errors.New("received an unexpected message from the peer")

var errTimeout = timeout{}
var errStreamClosed = errors.New("stream closed")

var (
	// NewStreamTimeout is the default new stream timeout
	NewStreamTimeout = time.Minute
	// ResetStreamTimeout is the default stream reset timeout
	ResetStreamTimeout = 2 * time.Minute
	// WriteCoalesceDelay is the default coalesce delay
	WriteCoalesceDelay = 100 * time.Microsecond
)

type timeout struct{}

func (t timeout) Error() string {
	return "i/o deadline exceeded"
}

func (t timeout) Temporary() bool {
	return true
}

func (t timeout) Timeout() bool {
	return true
}

// +1 for initiator
const (
	newStreamTag = 0
	messageTag   = 2
	closeTag     = 4
	resetTag     = 6
)

// Multiplex is a mplex session.
type Multiplex struct {
	con       net.Conn
	buf       *bufio.Reader
	nextID    uint64
	initiator bool

	closed       chan struct{}
	shutdown     chan struct{}
	shutdownErr  error
	shutdownLock sync.Mutex

	writeCh    chan []byte
	writeTimer *time.Timer

	nstreams chan *Stream

	channels map[streamID]*Stream
	chLock   sync.Mutex
	logger   *zap.Logger
}

// NewMultiplex creates a new multiplexer session.
func NewMultiplex(logger *zap.Logger, con net.Conn, initiator bool) *Multiplex {
	mp := &Multiplex{
		con:        con,
		initiator:  initiator,
		buf:        bufio.NewReader(con),
		channels:   make(map[streamID]*Stream),
		closed:     make(chan struct{}),
		shutdown:   make(chan struct{}),
		writeCh:    make(chan []byte, 16),
		writeTimer: time.NewTimer(0),
		nstreams:   make(chan *Stream, 16),
	}

	go mp.handleIncoming()
	go mp.handleOutgoing()

	return mp
}

func (mp *Multiplex) newStream(id streamID, name string) (s *Stream) {
	s = &Stream{
		id:        id,
		name:      name,
		dataIn:    make(chan []byte, 8),
		reset:     make(chan struct{}),
		rDeadline: makePipeDeadline(),
		wDeadline: makePipeDeadline(),
		mp:        mp,
	}

	s.closedLocal, s.doCloseLocal = context.WithCancel(context.Background())
	return
}

// Accept accepts the next stream from the connection.
func (mp *Multiplex) Accept() (*Stream, error) {
	select {
	case s, ok := <-mp.nstreams:
		if !ok {
			return nil, errors.New("multiplex closed")
		}
		return s, nil
	case <-mp.closed:
		return nil, mp.shutdownErr
	}
}

// Close closes the session.
func (mp *Multiplex) Close() error {
	mp.closeNoWait()

	// Wait for the receive loop to finish.
	<-mp.closed

	return nil
}

func (mp *Multiplex) closeNoWait() {
	mp.shutdownLock.Lock()
	select {
	case <-mp.shutdown:
	default:
		mp.con.Close()
		close(mp.shutdown)
	}
	mp.shutdownLock.Unlock()
}

// IsClosed returns true if the session is closed.
func (mp *Multiplex) IsClosed() bool {
	select {
	case <-mp.closed:
		return true
	default:
		return false
	}
}

func (mp *Multiplex) sendMsg(done <-chan struct{}, header uint64, data []byte) error {
	buf := pool.Get(len(data) + 20)

	n := 0
	n += binary.PutUvarint(buf[n:], header)
	n += binary.PutUvarint(buf[n:], uint64(len(data)))
	n += copy(buf[n:], data)

	select {
	case mp.writeCh <- buf[:n]:
		return nil
	case <-mp.shutdown:
		return ErrShutdown
	case <-done:
		return errTimeout
	}
}

func (mp *Multiplex) handleOutgoing() {
	for {
		select {
		case <-mp.shutdown:
			return

		case data := <-mp.writeCh:
			// FIXME: https://github.com/libp2p/go-libp2p/issues/644
			// write coalescing disabled until this can be fixed.
			//err := mp.writeMsg(data)
			err := mp.doWriteMsg(data)
			pool.Put(data)
			if err != nil {
				mp.logger.Warn("error writing data", zap.Error(err))
				return
			}
		}
	}
}

func (mp *Multiplex) doWriteMsg(data []byte) error {
	if mp.isShutdown() {
		return ErrShutdown
	}

	_, err := mp.con.Write(data)
	if err != nil {
		mp.closeNoWait()
	}

	return err
}

func (mp *Multiplex) nextChanID() uint64 {
	out := mp.nextID
	mp.nextID++
	return out
}

// NewStream creates a new stream.
func (mp *Multiplex) NewStream() (*Stream, error) {
	return mp.NewNamedStream("")
}

// NewNamedStream creates a new named stream.
func (mp *Multiplex) NewNamedStream(name string) (*Stream, error) {
	mp.chLock.Lock()

	// We could call IsClosed but this is faster (given that we already have
	// the lock).
	if mp.channels == nil {
		mp.chLock.Unlock()
		return nil, ErrShutdown
	}

	sid := mp.nextChanID()
	header := (sid << 3) | newStreamTag

	if name == "" {
		name = fmt.Sprint(sid)
	}
	s := mp.newStream(streamID{
		id:        sid,
		initiator: true,
	}, name)
	mp.channels[s.id] = s
	mp.chLock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), NewStreamTimeout)
	defer cancel()

	err := mp.sendMsg(ctx.Done(), header, []byte(name))
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (mp *Multiplex) cleanup() {
	mp.closeNoWait()
	mp.chLock.Lock()
	defer mp.chLock.Unlock()
	for _, msch := range mp.channels {
		msch.clLock.Lock()
		if !msch.closedRemote {
			msch.closedRemote = true
			// Cancel readers
			close(msch.reset)
		}

		msch.doCloseLocal()
		msch.clLock.Unlock()
	}
	// Don't remove this nil assignment. We check if this is nil to check if
	// the connection is closed when we already have the lock (faster than
	// checking if the stream is closed).
	mp.channels = nil
	if mp.shutdownErr == nil {
		mp.shutdownErr = ErrShutdown
	}
	close(mp.closed)
}

func (mp *Multiplex) handleIncoming() {
	defer mp.cleanup()

	recvTimeout := time.NewTimer(0)
	defer recvTimeout.Stop()

	if !recvTimeout.Stop() {
		<-recvTimeout.C
	}

	for {
		chID, tag, err := mp.readNextHeader()
		if err != nil {
			mp.shutdownErr = err
			return
		}

		remoteIsInitiator := tag&1 == 0
		ch := streamID{
			// true if *I'm* the initiator.
			initiator: !remoteIsInitiator,
			id:        chID,
		}
		// Rounds up the tag:
		// 0 -> 0
		// 1 -> 2
		// 2 -> 2
		// 3 -> 4
		// etc...
		tag += (tag & 1)

		b, err := mp.readNext()
		if err != nil {
			mp.shutdownErr = err
			return
		}

		mp.chLock.Lock()
		msch, ok := mp.channels[ch]
		mp.chLock.Unlock()

		switch tag {
		case newStreamTag:
			if ok {
				mp.logger.Debug("received message for existing stream", zap.Uint64("stream.id", ch.id))
				mp.shutdownErr = ErrInvalidState
				return
			}

			name := string(b)
			pool.Put(b)

			msch = mp.newStream(ch, name)
			mp.chLock.Lock()
			mp.channels[ch] = msch
			mp.chLock.Unlock()
			select {
			case mp.nstreams <- msch:
			case <-mp.shutdown:
				return
			}

		case resetTag:
			if !ok {
				// This is *ok*. We forget the stream on reset.
				continue
			}
			msch.clLock.Lock()

			isClosed := msch.isClosed()

			if !msch.closedRemote {
				close(msch.reset)
				msch.closedRemote = true
			}

			if !isClosed {
				msch.doCloseLocal()
			}

			msch.clLock.Unlock()

			msch.cancelDeadlines()

			mp.chLock.Lock()
			delete(mp.channels, ch)
			mp.chLock.Unlock()
		case closeTag:
			if !ok {
				continue
			}

			msch.clLock.Lock()

			if msch.closedRemote {
				msch.clLock.Unlock()
				// Technically a bug on the other side. We
				// should consider killing the connection.
				continue
			}

			close(msch.dataIn)
			msch.closedRemote = true

			cleanup := msch.isClosed()

			msch.clLock.Unlock()

			if cleanup {
				msch.cancelDeadlines()
				mp.chLock.Lock()
				delete(mp.channels, ch)
				mp.chLock.Unlock()
			}
		case messageTag:
			if !ok {
				// reset stream, return b
				pool.Put(b)

				// This is a perfectly valid case when we reset
				// and forget about the stream.
				// go mp.sendResetMsg(ch.header(resetTag), false)
				continue
			}

			msch.clLock.Lock()
			remoteClosed := msch.closedRemote
			msch.clLock.Unlock()
			if remoteClosed {
				// closed stream, return b
				pool.Put(b)
				mp.logger.Warn("received data after stream was closed by peer")
				// go mp.sendResetMsg(msch.id.header(resetTag), false)
				continue
			}

			recvTimeout.Reset(ReceiveTimeout)
			select {
			case msch.dataIn <- b:
			case <-msch.reset:
				pool.Put(b)
			case <-recvTimeout.C:
				pool.Put(b)
				mp.logger.Warn("timeout receiving message in stream queue")
				// Do not do this asynchronously. Otherwise, we
				// could drop a message, then receive a message,
				// then reset.
				msch.Reset()
				continue
			case <-mp.shutdown:
				pool.Put(b)
				return
			}
			if !recvTimeout.Stop() {
				<-recvTimeout.C
			}
		default:
			mp.logger.Debug("message with unknown header on stream", zap.Uint64("stream.id", ch.id))
			if ok {
				msch.Reset()
			}
		}
	}
}

func (mp *Multiplex) isShutdown() bool {
	select {
	case <-mp.shutdown:
		return true
	default:
		return false
	}
}

func (mp *Multiplex) sendResetMsg(header uint64, hard bool) {
	ctx, cancel := context.WithTimeout(context.Background(), ResetStreamTimeout)
	defer cancel()

	err := mp.sendMsg(ctx.Done(), header, nil)
	if err != nil && !mp.isShutdown() {
		if hard {
			mp.logger.Warn("error sending reset message killing connection", zap.Error(err))
			mp.Close()
		} else {
			mp.logger.Debug("error sending reset message", zap.Error(err))
		}
	}
}

func (mp *Multiplex) readNextHeader() (uint64, uint64, error) {
	h, err := binary.ReadUvarint(mp.buf)
	if err != nil {
		return 0, 0, err
	}

	// get channel ID
	ch := h >> 3

	rem := h & 7

	return ch, rem, nil
}

func (mp *Multiplex) readNext() ([]byte, error) {
	// get length
	l, err := binary.ReadUvarint(mp.buf)
	if err != nil {
		return nil, err
	}

	if l > uint64(MaxMessageSize) {
		return nil, fmt.Errorf("message size too large")
	}

	if l == 0 {
		return nil, nil
	}

	buf := pool.Get(int(l))
	n, err := io.ReadFull(mp.buf, buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}
