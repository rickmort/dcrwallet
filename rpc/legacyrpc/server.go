// Copyright (c) 2013-2015 The btcsuite developers
// Copyright (c) 2017 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package legacyrpc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/dcrjson"
	"github.com/decred/dcrwallet/errors"
	"github.com/decred/dcrwallet/loader"
	"github.com/decred/dcrwallet/ticketbuyer"
	"github.com/gorilla/websocket"
)

type websocketClient struct {
	conn          *websocket.Conn
	authenticated bool
	allRequests   chan []byte
	responses     chan []byte
	quit          chan struct{} // closed on disconnect
	wg            sync.WaitGroup
}

func newWebsocketClient(c *websocket.Conn, authenticated bool) *websocketClient {
	return &websocketClient{
		conn:          c,
		authenticated: authenticated,
		allRequests:   make(chan []byte),
		responses:     make(chan []byte),
		quit:          make(chan struct{}),
	}
}

func (c *websocketClient) send(b []byte) error {
	select {
	case c.responses <- b:
		return nil
	case <-c.quit:
		return errors.New("websocket client disconnected")
	}
}

// Server holds the items the RPC server may need to access (auth,
// config, shutdown, etc.)
type Server struct {
	httpServer        http.Server
	walletLoader      *loader.Loader
	ticketbuyerConfig *ticketbuyer.Config
	listeners         []net.Listener
	authsha           [sha256.Size]byte
	upgrader          websocket.Upgrader

	maxPostClients      int64 // Max concurrent HTTP POST clients.
	maxWebsocketClients int64 // Max concurrent websocket clients.

	wg      sync.WaitGroup
	quit    chan struct{}
	quitMtx sync.Mutex

	requestShutdownChan chan struct{}

	activeNet *chaincfg.Params
}

type handler struct {
	fn     func(*Server, interface{}) (interface{}, error)
	noHelp bool
}

// jsonAuthFail sends a message back to the client if the http auth is rejected.
func jsonAuthFail(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Basic realm="btcwallet RPC"`)
	http.Error(w, "401 Unauthorized.", http.StatusUnauthorized)
}

// NewServer creates a new server for serving legacy RPC client connections,
// both HTTP POST and websocket.
func NewServer(opts *Options, activeNet *chaincfg.Params, walletLoader *loader.Loader, ticketBuyerConfig *ticketbuyer.Config, listeners []net.Listener) *Server {
	serveMux := http.NewServeMux()
	const rpcAuthTimeoutSeconds = 10
	server := &Server{
		httpServer: http.Server{
			Handler: serveMux,

			// Timeout connections which don't complete the initial
			// handshake within the allowed timeframe.
			ReadTimeout: time.Second * rpcAuthTimeoutSeconds,
		},
		walletLoader:        walletLoader,
		maxPostClients:      opts.MaxPOSTClients,
		maxWebsocketClients: opts.MaxWebsocketClients,
		listeners:           listeners,
		ticketbuyerConfig:   ticketBuyerConfig,
		// A hash of the HTTP basic auth string is used for a constant
		// time comparison.
		authsha: sha256.Sum256(httpBasicAuth(opts.Username, opts.Password)),
		upgrader: websocket.Upgrader{
			// Allow all origins.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		quit:                make(chan struct{}),
		requestShutdownChan: make(chan struct{}, 1),
		activeNet:           activeNet,
	}

	serveMux.Handle("/", throttledFn(opts.MaxPOSTClients,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "close")
			w.Header().Set("Content-Type", "application/json")
			r.Close = true

			if err := server.checkAuthHeader(r); err != nil {
				log.Warnf("Failed authentication attempt from client %s",
					r.RemoteAddr)
				jsonAuthFail(w)
				return
			}
			server.wg.Add(1)
			server.postClientRPC(w, r)
			server.wg.Done()
		}))

	serveMux.Handle("/ws", throttledFn(opts.MaxWebsocketClients,
		func(w http.ResponseWriter, r *http.Request) {
			ctx := withRemoteAddr(r.Context(), r.RemoteAddr)
			authenticated := false
			switch server.checkAuthHeader(r) {
			case nil:
				authenticated = true
			case errNoAuth:
				// nothing
			default:
				// If auth was supplied but incorrect, rather than simply
				// being missing, immediately terminate the connection.
				log.Warnf("Failed authentication attempt from client %s",
					r.RemoteAddr)
				jsonAuthFail(w)
				return
			}

			conn, err := server.upgrader.Upgrade(w, r, nil)
			if err != nil {
				log.Warnf("Cannot websocket upgrade client %s: %v",
					r.RemoteAddr, err)
				return
			}
			wsc := newWebsocketClient(conn, authenticated)
			server.websocketClientRPC(ctx, wsc)
		}))

	for _, lis := range listeners {
		server.serve(lis)
	}

	return server
}

// httpBasicAuth returns the UTF-8 bytes of the HTTP Basic authentication
// string:
//
//   "Basic " + base64(username + ":" + password)
func httpBasicAuth(username, password string) []byte {
	const header = "Basic "
	base64 := base64.StdEncoding

	b64InputLen := len(username) + len(":") + len(password)
	b64Input := make([]byte, 0, b64InputLen)
	b64Input = append(b64Input, username...)
	b64Input = append(b64Input, ':')
	b64Input = append(b64Input, password...)

	output := make([]byte, len(header)+base64.EncodedLen(b64InputLen))
	copy(output, header)
	base64.Encode(output[len(header):], b64Input)
	return output
}

// serve serves HTTP POST and websocket RPC for the legacy JSON-RPC RPC server.
// This function does not block on lis.Accept.
func (s *Server) serve(lis net.Listener) {
	s.wg.Add(1)
	go func() {
		log.Infof("Listening on %s", lis.Addr())
		err := s.httpServer.Serve(lis)
		log.Tracef("Finished serving RPC: %v", err)
		s.wg.Done()
	}()
}

// Stop gracefully shuts down the rpc server by stopping and disconnecting all
// clients.  This blocks until shutdown completes.
func (s *Server) Stop() {
	s.quitMtx.Lock()
	select {
	case <-s.quit:
		s.quitMtx.Unlock()
		return
	default:
	}

	// Stop all the listeners.
	for _, listener := range s.listeners {
		err := listener.Close()
		if err != nil {
			log.Errorf("Cannot close listener `%s`: %v",
				listener.Addr(), err)
		}
	}

	// Signal the remaining goroutines to stop.
	close(s.quit)
	s.quitMtx.Unlock()

	// Wait for all remaining goroutines to exit.
	s.wg.Wait()
}

// handlerClosure creates a closure function for handling requests of the given
// method.  This may be a request that is handled directly by dcrwallet, or
// a chain server request that is handled by passing the request down to dcrd.
//
// NOTE: These handlers do not handle special cases, such as the authenticate
// method.  Each of these must be checked beforehand (the method is already
// known) and handled accordingly.
func (s *Server) handlerClosure(ctx context.Context, request *dcrjson.Request) lazyHandler {
	log.Infof("RPC method %v invoked by %v", request.Method, remoteAddr(ctx))
	return lazyApplyHandler(s, request)
}

// errNoAuth represents an error where authentication could not succeed
// due to a missing Authorization HTTP header.
var errNoAuth = errors.E("missing Authorization header")

// checkAuthHeader checks the HTTP Basic authentication supplied by a client
// in the HTTP request r.
//
// The authentication comparison is time constant.
func (s *Server) checkAuthHeader(r *http.Request) error {
	authhdr := r.Header["Authorization"]
	if len(authhdr) == 0 {
		return errNoAuth
	}

	authsha := sha256.Sum256([]byte(authhdr[0]))
	cmp := subtle.ConstantTimeCompare(authsha[:], s.authsha[:])
	if cmp != 1 {
		return errors.New("invalid Authorization header")
	}
	return nil
}

// throttledFn wraps an http.HandlerFunc with throttling of concurrent active
// clients by responding with an HTTP 429 when the threshold is crossed.
func throttledFn(threshold int64, f http.HandlerFunc) http.Handler {
	return throttled(threshold, f)
}

// throttled wraps an http.Handler with throttling of concurrent active
// clients by responding with an HTTP 429 when the threshold is crossed.
func throttled(threshold int64, h http.Handler) http.Handler {
	var active int64

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt64(&active, 1)
		defer atomic.AddInt64(&active, -1)

		if current-1 >= threshold {
			log.Warnf("Reached threshold of %d concurrent active clients", threshold)
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// idPointer returns a pointer to the passed ID, or nil if the interface is nil.
// Interface pointers are usually a red flag of doing something incorrectly,
// but this is only implemented here to work around an oddity with dcrjson,
// which uses empty interface pointers for response IDs.
func idPointer(id interface{}) (p *interface{}) {
	if id != nil {
		p = &id
	}
	return
}

// invalidAuth checks whether a websocket request is a valid (parsable)
// authenticate request and checks the supplied username and passphrase
// against the server auth.
func (s *Server) invalidAuth(req *dcrjson.Request) bool {
	cmd, err := dcrjson.UnmarshalCmd(req)
	if err != nil {
		return false
	}
	authCmd, ok := cmd.(*dcrjson.AuthenticateCmd)
	if !ok {
		return false
	}
	// Check credentials.
	login := authCmd.Username + ":" + authCmd.Passphrase
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(login))
	authSha := sha256.Sum256([]byte(auth))
	return subtle.ConstantTimeCompare(authSha[:], s.authsha[:]) != 1
}

func (s *Server) websocketClientRead(ctx context.Context, wsc *websocketClient) {
	for {
		_, request, err := wsc.conn.ReadMessage()
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Warnf("Websocket receive failed from client %s: %v",
					remoteAddr(ctx), err)
			}
			close(wsc.allRequests)
			break
		}
		wsc.allRequests <- request
	}
}

func (s *Server) websocketClientRespond(ctx context.Context, wsc *websocketClient) {
	// A for-select with a read of the quit channel is used instead of a
	// for-range to provide clean shutdown.  This is necessary due to
	// WebsocketClientRead (which sends to the allRequests chan) not closing
	// allRequests during shutdown if the remote websocket client is still
	// connected.
out:
	for {
		select {
		case reqBytes, ok := <-wsc.allRequests:
			if !ok {
				// client disconnected
				break out
			}

			var req dcrjson.Request
			err := json.Unmarshal(reqBytes, &req)
			if err != nil {
				log.Warnf("Failed unmarshal of JSON-RPC request object "+
					"from client %s", remoteAddr(ctx))
				if !wsc.authenticated {
					// Disconnect immediately.
					break out
				}
				resp := makeResponse(req.ID, nil,
					dcrjson.ErrRPCInvalidRequest)
				mresp, err := json.Marshal(resp)
				// We expect the marshal to succeed.  If it
				// doesn't, it indicates some non-marshalable
				// type in the response.
				if err != nil {
					panic(err)
				}
				err = wsc.send(mresp)
				if err != nil {
					break out
				}
				continue
			}

			if req.Method == "authenticate" {
				log.Infof("RPC method authenticate invoked by %s",
					remoteAddr(ctx))
				switch {
				case wsc.authenticated:
					log.Warnf("Multiple authentication attempts from %s",
						remoteAddr(ctx))
					break out
				case s.invalidAuth(&req):
					log.Warnf("Failed authentication attempt from %s",
						remoteAddr(ctx))
					break out
				}
				wsc.authenticated = true
				resp := makeResponse(req.ID, nil, nil)
				// Expected to never fail.
				mresp, err := json.Marshal(resp)
				if err != nil {
					panic(err)
				}
				err = wsc.send(mresp)
				if err != nil {
					break out
				}
				continue
			}

			if !wsc.authenticated {
				// Disconnect immediately.
				break out
			}

			switch req.Method {
			case "stop":
				log.Infof("RPC method stop invoked by %s", remoteAddr(ctx))
				resp := makeResponse(req.ID,
					"dcrwallet stopping.", nil)
				mresp, err := json.Marshal(resp)
				// Expected to never fail.
				if err != nil {
					panic(err)
				}
				err = wsc.send(mresp)
				if err != nil {
					break out
				}
				s.requestProcessShutdown()
				break out

			default:
				req := req // Copy for the closure
				f := s.handlerClosure(ctx, &req)
				wsc.wg.Add(1)
				go func() {
					resp, jsonErr := f()
					mresp, err := dcrjson.MarshalResponse(req.Jsonrpc, req.ID, resp, jsonErr)
					if err != nil {
						log.Errorf("Unable to marshal response to client %s: %v",
							remoteAddr(ctx), err)
					} else {
						_ = wsc.send(mresp)
					}
					wsc.wg.Done()
				}()
			}

		case <-s.quit:
			break out
		}
	}

	// allow client to disconnect after all handler goroutines are done
	wsc.wg.Wait()
	close(wsc.responses)
	s.wg.Done()
}

func (s *Server) websocketClientSend(ctx context.Context, wsc *websocketClient) {
	const deadline time.Duration = 2 * time.Second
out:
	for {
		select {
		case response, ok := <-wsc.responses:
			if !ok {
				// client disconnected
				break out
			}
			err := wsc.conn.SetWriteDeadline(time.Now().Add(deadline))
			if err != nil {
				log.Warnf("Cannot set write deadline on "+
					"client %s: %v", remoteAddr(ctx), err)
			}
			err = wsc.conn.WriteMessage(websocket.TextMessage,
				response)
			if err != nil {
				log.Warnf("Failed websocket send to client "+
					"%s: %v", remoteAddr(ctx), err)
				break out
			}

		case <-s.quit:
			break out
		}
	}
	close(wsc.quit)
	log.Infof("Disconnected websocket client %s", remoteAddr(ctx))
	s.wg.Done()
}

// websocketClientRPC starts the goroutines to serve JSON-RPC requests over a
// websocket connection for a single client.
func (s *Server) websocketClientRPC(ctx context.Context, wsc *websocketClient) {
	log.Infof("New websocket client %s", remoteAddr(ctx))

	// Clear the read deadline set before the websocket hijacked
	// the connection.
	if err := wsc.conn.SetReadDeadline(time.Time{}); err != nil {
		log.Warnf("Cannot remove read deadline: %v", err)
	}

	// WebsocketClientRead is intentionally not run with the waitgroup
	// so it is ignored during shutdown.  This is to prevent a hang during
	// shutdown where the goroutine is blocked on a read of the
	// websocket connection if the client is still connected.
	go s.websocketClientRead(ctx, wsc)

	s.wg.Add(2)
	go s.websocketClientRespond(ctx, wsc)
	go s.websocketClientSend(ctx, wsc)

	<-wsc.quit
}

// maxRequestSize specifies the maximum number of bytes in the request body
// that may be read from a client.  This is currently limited to 4MB.
const maxRequestSize = 1024 * 1024 * 4

// postClientRPC processes and replies to a JSON-RPC client request.
func (s *Server) postClientRPC(w http.ResponseWriter, r *http.Request) {
	ctx := withRemoteAddr(r.Context(), r.RemoteAddr)

	body := http.MaxBytesReader(w, r.Body, maxRequestSize)
	rpcRequest, err := ioutil.ReadAll(body)
	if err != nil {
		// TODO: what if the underlying reader errored?
		log.Warnf("Request from client %v exceeds maximum size", r.RemoteAddr)
		http.Error(w, "413 Request Too Large.",
			http.StatusRequestEntityTooLarge)
		return
	}

	// First check whether wallet has a handler for this request's method.
	// If unfound, the request is sent to the chain server for further
	// processing.  While checking the methods, disallow authenticate
	// requests, as they are invalid for HTTP POST clients.
	var req dcrjson.Request
	err = json.Unmarshal(rpcRequest, &req)
	if err != nil {
		resp, err := dcrjson.MarshalResponse(req.Jsonrpc, req.ID, nil, dcrjson.ErrRPCInvalidRequest)
		if err != nil {
			log.Errorf("Unable to marshal response to client %s: %v",
				r.RemoteAddr, err)
			http.Error(w, "500 Internal Server Error",
				http.StatusInternalServerError)
			return
		}
		_, err = w.Write(resp)
		if err != nil {
			log.Warnf("Cannot write invalid request request to "+
				"client %s: %v", r.RemoteAddr, err)
		}
		return
	}

	// Create the response and error from the request.  Two special cases
	// are handled for the authenticate and stop request methods.
	var res interface{}
	var jsonErr *dcrjson.RPCError
	var stop bool
	switch req.Method {
	case "authenticate":
		log.Warnf("Invalid RPC method authenticate invoked by HTTP POST client %s",
			r.RemoteAddr)
		// Drop it.
		return
	case "stop":
		log.Infof("RPC method stop invoked by %s", r.RemoteAddr)
		stop = true
		res = "dcrwallet stopping"
	default:
		res, jsonErr = s.handlerClosure(ctx, &req)()
	}

	// Marshal and send.
	mresp, err := dcrjson.MarshalResponse(req.Jsonrpc, req.ID, res, jsonErr)
	if err != nil {
		log.Errorf("Unable to marshal response to client %s: %v",
			r.RemoteAddr, err)
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}
	_, err = w.Write(mresp)
	if err != nil {
		log.Warnf("Failed to write response to client %s: %v",
			r.RemoteAddr, err)
	}

	if stop {
		s.requestProcessShutdown()
	}
}

func (s *Server) requestProcessShutdown() {
	s.requestShutdownChan <- struct{}{}
}

// RequestProcessShutdown returns a channel that is sent to when an authorized
// client requests remote shutdown.
func (s *Server) RequestProcessShutdown() <-chan struct{} {
	return s.requestShutdownChan
}
