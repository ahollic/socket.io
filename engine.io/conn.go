/**
 * Golang socket.io
 * Copyright (C) 2024 Kevin Z <zyxkad@gmail.com>
 * All rights reserved
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Affero General Public License as published
 *  by the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU Affero General Public License for more details.
 *
 *  You should have received a copy of the GNU Affero General Public License
 *  along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ahollic/socket.io/internal/utils"
	"github.com/gorilla/websocket"
)

const Protocol = 4

var (
	errMultipleOpen = errors.New("Engine.IO: socket was already opened")

	ErrSocketConnected = errors.New("Engine.IO: socket was already connected")
	ErrPingTimeout     = errors.New("Engine.IO: did not receive PING packet for a long time")
)

type SocketStatus = int32

const (
	SocketClosed SocketStatus = iota
	SocketOpening
	SocketConnected
)

var WebsocketDialer *websocket.Dialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 30 * time.Second,
}

type Socket struct {
	Dialer *websocket.Dialer
	opts   Options
	url    url.URL

	mux     sync.RWMutex
	dialCtx context.Context
	ctx     context.Context
	cancel  context.CancelCauseFunc

	connectHandles    utils.HandlerList[*Socket, struct{}]
	disconnectHandles utils.HandlerList[*Socket, error]
	dialErrorHandles  utils.HandlerList[*Socket, *DialErrorContext]
	reconnectHandles  utils.HandlerList[*Socket, struct{}]
	pongHandles       utils.HandlerList[*Socket, []byte]
	binaryHandlers    utils.HandlerList[*Socket, []byte]
	messageHandles    utils.HandlerList[*Socket, []byte]
	// debug handler
	recvHandles utils.HandlerList[*Socket, []byte]
	sendHandles utils.HandlerList[*Socket, []byte]

	wsconn         *websocket.Conn
	status         atomic.Int32
	sid            string
	pingInterval   time.Duration
	pingTimeout    time.Duration
	maxPayload     int
	reDialCount    int
	reDialTimeout  time.Duration
	reconnectTimer atomic.Pointer[time.Timer]

	msgbuf []*Packet
}

type Options struct {
	Secure       bool
	Host         string // [<scheme>://]<host>:<port>
	Path         string
	ExtraQuery   url.Values
	ExtraHeaders http.Header
	DialTimeout  time.Duration
}

var DefaultOption = Options{
	Secure: true,
	Path:   "/engine.io/",
}

func NewSocket(opts Options) (s *Socket, err error) {
	if i := strings.Index(opts.Host, "://"); i > 0 {
		scheme := opts.Host[:i]
		opts.Host = opts.Host[i+len("://"):]
		opts.Secure = !(scheme == "ws" || scheme == "http")
	}
	dialURL := url.URL{
		Host: opts.Host,
		Path: opts.Path,
	}
	if opts.Secure {
		dialURL.Scheme = "wss"
	} else {
		dialURL.Scheme = "ws"
	}
	query := make(url.Values, 2+len(opts.ExtraQuery))
	for k, v := range opts.ExtraQuery {
		query[k] = v
	}
	query.Set("EIO", strconv.Itoa(Protocol))
	query.Set("transport", "websocket")
	dialURL.RawQuery = query.Encode()

	s = &Socket{
		Dialer: WebsocketDialer,
		opts:   opts,
		url:    dialURL,
	}
	return
}

func (s *Socket) Status() SocketStatus {
	return s.status.Load()
}

func (s *Socket) Connected() bool {
	return s.Status() == SocketConnected
}

func (s *Socket) ID() string {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.sid
}

func (s *Socket) Context() context.Context {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.ctx
}

func (s *Socket) Conn() *websocket.Conn {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return s.wsconn
}

func (s *Socket) URL() *url.URL {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return &s.url
}

type DialErrorContext struct {
	count  int
	err    error
	reDial bool
}

func (ctx *DialErrorContext) Count() int {
	return ctx.count
}

func (ctx *DialErrorContext) Err() error {
	return ctx.err
}

func (ctx *DialErrorContext) ReDial() bool {
	return ctx.reDial
}

func (ctx *DialErrorContext) CancelReDial() {
	ctx.reDial = true
}

func (s *Socket) dial(ctx context.Context) (err error) {
	var wsconn *websocket.Conn
	if s.opts.DialTimeout > 0 {
		tctx, cancel := context.WithTimeout(ctx, s.opts.DialTimeout)
		wsconn, _, err = s.Dialer.DialContext(tctx, s.url.String(), s.opts.ExtraHeaders)
		cancel()
	} else {
		wsconn, _, err = s.Dialer.DialContext(ctx, s.url.String(), s.opts.ExtraHeaders)
	}
	if err != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancelCause(s.dialCtx)
	s.wsconn = wsconn
	s.msgbuf = s.msgbuf[:0]
	s.reDialCount = 0
	s.reDialTimeout = time.Second

	return
}

func (s *Socket) Dial(ctx context.Context) (err error) {
	if s.status.Load() != SocketClosed {
		return ErrSocketConnected
	}

	s.mux.Lock()
	defer s.mux.Unlock()

	if !s.status.CompareAndSwap(SocketClosed, SocketOpening) || s.wsconn != nil {
		return ErrSocketConnected
	}

	s.dialCtx = ctx
	if err = s.dial(ctx); err != nil {
		s.status.Store(SocketClosed)
		s.dialErrorHandles.Call(s, &DialErrorContext{
			count: -1,
			err:   err,
		})
		return
	}

	go s._reader(s.ctx, s.wsconn)

	return
}

func (s *Socket) reDial() (err error) {
	if s.status.Load() != SocketClosed {
		return ErrSocketConnected
	}

	s.mux.Lock()
	defer s.mux.Unlock()

	if !s.status.CompareAndSwap(SocketClosed, SocketOpening) {
		return ErrSocketConnected
	}

	if err = s.dial(s.dialCtx); err != nil {
		s.reDialCount++
		s.status.Store(SocketClosed)
		s.dialErrorHandles.Call(s, &DialErrorContext{
			count: s.reDialCount,
			err:   err,
		})
		return
	}

	go s._reader(s.ctx, s.wsconn)

	s.reconnectHandles.Call(s, struct{}{})

	return
}

func (s *Socket) onMessage(data []byte) {
	s.messageHandles.Call(s, data)
}

func (s *Socket) nextReconnect(ctx context.Context) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if timer := s.reconnectTimer.Swap(nil); timer != nil {
		timer.Stop()
	}

	if s.reDialTimeout < time.Minute*5 {
		s.reDialTimeout = s.reDialTimeout * 2
	}
	stop := context.AfterFunc(ctx, func() {
		if timer := s.reconnectTimer.Swap(nil); timer != nil {
			timer.Stop()
		}
	})
	s.reconnectTimer.Store(time.AfterFunc(s.reDialTimeout, func() {
		s.reconnectTimer.Store(nil)
		stop()
		if err := s.reDial(); err != nil {
			s.nextReconnect(ctx)
		}
	}))
}

func (s *Socket) onClose(err error) {
	if s.status.Swap(SocketClosed) == SocketClosed {
		return
	}

	s.mux.RLock()
	s.wsconn.Close()
	s.cancel(err)
	dialCtx := s.dialCtx
	s.mux.RUnlock()

	s.disconnectHandles.Call(s, err)
	if err != nil {
		s.nextReconnect(dialCtx)
	}
}

func (s *Socket) OnConnect(cb func(s *Socket)) {
	s.connectHandles.On(func(s *Socket, _ struct{}) {
		cb(s)
	})
}

func (s *Socket) OnceConnect(cb func(s *Socket)) {
	s.connectHandles.Once(func(s *Socket, _ struct{}) {
		cb(s)
	})
}

func (s *Socket) OnDisconnect(cb func(s *Socket, err error)) {
	s.disconnectHandles.On(cb)
}

func (s *Socket) OnceDisconnect(cb func(s *Socket, err error)) {
	s.disconnectHandles.Once(cb)
}

func (s *Socket) OnDialError(cb func(s *Socket, err *DialErrorContext)) {
	s.dialErrorHandles.On(cb)
}

func (s *Socket) OnReconnect(cb func(s *Socket)) {
	s.reconnectHandles.On(func(s *Socket, _ struct{}) {
		cb(s)
	})
}

func (s *Socket) OnPong(cb func(s *Socket, data []byte)) {
	s.pongHandles.On(cb)
}

func (s *Socket) OncePong(cb func(s *Socket, data []byte)) {
	s.pongHandles.Once(cb)
}

func (s *Socket) OnBinary(cb func(s *Socket, data []byte)) {
	s.binaryHandlers.On(cb)
}

func (s *Socket) OnceBinary(cb func(s *Socket, data []byte)) {
	s.binaryHandlers.Once(cb)
}

func (s *Socket) OnMessage(cb func(s *Socket, data []byte)) {
	s.messageHandles.On(cb)
}

func (s *Socket) OnceMessage(cb func(s *Socket, data []byte)) {
	s.messageHandles.Once(cb)
}

func (s *Socket) OnRecv(cb func(s *Socket, data []byte)) {
	s.recvHandles.On(cb)
}

func (s *Socket) OnSend(cb func(s *Socket, data []byte)) {
	s.sendHandles.On(cb)
}

func (s *Socket) _reader(ctx context.Context, wsconn *websocket.Conn) {
	defer wsconn.Close()
	defer s.status.Store(SocketClosed)

	openCh := make(chan struct{}, 0)

	pingTimer := time.NewTimer(time.Minute)
	pingTimer.Stop()

	go func() {
		defer wsconn.Close()
		select {
		case <-ctx.Done():
			return
		case <-openCh: // wait for the open packet
		}

		// clear timer
		pingTimer.Stop()
		select {
		case <-pingTimer.C:
		default:
		}

		select {
		case <-ctx.Done():
		case <-pingTimer.C:
			s.onClose(ErrPingTimeout)
		}
	}()

	pkt := new(Packet)
	var buf []byte
	for {
		code, r, err := wsconn.NextReader()
		if err != nil {
			s.mux.RLock()
			ok := wsconn == s.wsconn
			s.mux.RUnlock()
			if ok {
				s.onClose(err)
			}
			return
		}

		// reset ping timer
		pingTimer.Reset(s.pingInterval + s.pingTimeout)

		switch code {
		case websocket.BinaryMessage:
			if buf, err = utils.ReadAllTo(r, buf[:0]); err != nil {
				s.onClose(err)
				return
			}
			s.binaryHandlers.Call(s, buf)
			continue
		case websocket.TextMessage:
			if buf, err = utils.ReadAllTo(r, buf[:0]); err != nil {
				s.onClose(err)
				return
			}
		default:
			continue
		}

		s.recvHandles.Call(s, buf)

		if err = pkt.UnmarshalBinary(buf); err != nil {
			s.onClose(err)
			return
		}

		switch pkt.typ {
		case BINARY:
			s.binaryHandlers.Call(s, pkt.body)
		case OPEN:
			if s.Status() != SocketOpening {
				s.onClose(errMultipleOpen)
				return
			}
			var obj struct {
				Sid          string   `json:"sid"`
				Upgrades     []string `json:"upgrades"`
				PingInterval int      `json:"pingInterval"`
				PingTimeout  int      `json:"pingTimeout"`
				MaxPayload   int      `json:"maxPayload"`
			}
			if err := pkt.UnmarshalBody(&obj); err != nil {
				s.onClose(err)
				continue
			}

			s.mux.Lock()
			s.sid = obj.Sid
			s.pingInterval = (time.Duration)(obj.PingInterval) * time.Millisecond
			s.pingTimeout = (time.Duration)(obj.PingTimeout) * time.Millisecond
			s.maxPayload = obj.MaxPayload
			for _, pkt := range s.msgbuf {
				s.sendPkt(wsconn, pkt)
			}
			s.msgbuf = s.msgbuf[:0]
			s.status.Store(SocketConnected)
			s.mux.Unlock()

			close(openCh)

			s.connectHandles.Call(s, struct{}{})
		case CLOSE:
			s.onClose(nil)
			return
		case PING:
			pkt.typ = PONG
			s.send(pkt)
		case PONG:
			s.pongHandles.Call(s, pkt.body)
		case MESSAGE:
			s.onMessage(pkt.body)
		default:
			s.onClose(fmt.Errorf("Engine.IO: unsupported packet type %s", pkt.typ))
		}
	}
}

func (s *Socket) sendPkt(wsconn *websocket.Conn, pkt *Packet) (err error) {
	if pkt.typ == BINARY {
		return wsconn.WriteMessage(websocket.BinaryMessage, pkt.body)
	}
	var buf []byte
	if buf, err = pkt.MarshalBinary(); err != nil {
		return
	}
	s.sendHandles.Call(s, buf)
	return wsconn.WriteMessage(websocket.TextMessage, buf)
}

func (s *Socket) Close() error {
	reconnectTimer := s.reconnectTimer.Swap(nil)
	if reconnectTimer != nil {
		reconnectTimer.Stop()
	}
	if s.Status() != SocketClosed {
		s.send(&Packet{
			typ: CLOSE,
		})
		return nil
	}
	s.status.Store(SocketClosed)
	return nil
}

func (s *Socket) send(pkt *Packet) {
	if s.Status() != SocketConnected {
		s.mux.Lock()
		defer s.mux.Unlock()
		s.msgbuf = append(s.msgbuf, pkt)
		return
	}

	if err := s.sendPkt(s.Conn(), pkt); err != nil {
		s.onClose(err)
	}
	return
}

func (s *Socket) Emit(body []byte) {
	s.send(&Packet{
		typ:  MESSAGE,
		body: body,
	})
}
