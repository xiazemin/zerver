package zerver

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosiner/gohper/attrs"
	"github.com/cosiner/gohper/crypto/tls2"
	"github.com/cosiner/gohper/defval"
	"github.com/cosiner/gohper/termcolor"
	"github.com/cosiner/ygo/resource"
	websocket "github.com/cosiner/zerver_websocket"
)

const (
	// server status
	_NORMAL    = 0
	_DESTROYED = 1

	_CONTENTTYPE_DISABLE = "-"
)

type (
	ServerOption struct {
		// server listening address, default :4000
		ListenAddr string
		// content type for each request, default application/json;charset=utf-8,
		// use "-" to disable the automation
		ContentType string

		// check websocket header, default nil
		WebSocketChecker HeaderChecker
		// logger, default use cosiner/gohper/log.Logger with ConsoleWriter
		Logger

		// path variables count, suggest set as max or average, default 3
		PathVarCount int
		// filters count for each route, RootFilters is not include, default 5
		FilterCount int

		// read timeout
		ReadTimeout time.Duration
		// write timeout
		WriteTimeout time.Duration
		// max header bytes
		MaxHeaderBytes int
		// tcp keep-alive period by minutes,
		// default 3 minute, same as predefined in standard http package
		KeepAlivePeriod time.Duration

		// CA pem files to verify client certs
		CAs []string
		// ssl config, default disable tls
		CertFile, KeyFile string
		// if not nil, cert and key will be ignored
		TLSConfig *tls.Config
	}

	// Server represent a web server
	Server struct {
		Router
		attrs.Attrs
		RootFilters RootFilters // Match Every Routes
		ResMaster   resource.Master
		Log         Logger
		ComponentManager

		checker     websocket.HandshakeChecker
		contentType string // default content type

		listener    net.Listener
		state       int32          // destroy or normal running
		activeConns sync.WaitGroup // connections in service, don't include hijacked and websocket connections
	}

	// HeaderChecker is a http header checker, it accept a function which can get
	// http header's value by name , if there is something wrong, throw an error
	// to terminate this request
	HeaderChecker func(func(string) string) error

	// Enviroment is a server enviroment, real implementation is the Server itself.
	// it can be accessed from Request/WebsocketConn
	Enviroment interface {
		Server() *Server
		ResourceMaster() *resource.Master
		Logger() Logger
		StartTask(path string, value interface{})
		Component(name string) (interface{}, error)
	}

	ComponentNotFoundError string
)

func (err ComponentNotFoundError) Name() string {
	return string(err)
}

func (err ComponentNotFoundError) Error() string {
	return "component \"" + string(err) + "\" is not found"
}

// NewServer create a new server with default router
func NewServer() *Server {
	return NewServerWith(nil, nil)
}

// NewServerWith create a new server with given router and root filters
func NewServerWith(rt Router, filters RootFilters) *Server {
	if filters == nil {
		filters = NewRootFilters(nil)
	}
	if rt == nil {
		rt = NewRouter()
	}

	return &Server{
		Router:           rt,
		Attrs:            attrs.NewLocked(),
		RootFilters:      filters,
		ResMaster:        resource.NewMaster(),
		ComponentManager: NewComponentManager(),
	}
}

func (s *Server) Server() *Server {
	return s
}

func (s *Server) Logger() Logger {
	return s.Log
}

func (s *Server) ResourceMaster() *resource.Master {
	return &s.ResMaster
}

func (s *Server) RegisterComponent(name string, component interface{}) ComponentEnviroment {
	return s.ComponentManager.RegisterComponent(s, name, component)
}

// StartTask start a task synchronously, the value will be passed to task handler
func (s *Server) StartTask(path string, value interface{}) {
	handler := s.MatchTaskHandler(&url.URL{Path: path})
	if handler == nil {
		s.Log.Panicln("No task handler found for:", path)
	}

	handler.Handle(value)
}

// ServHttp serve for http reuest
// find handler and resolve path, find filters, process
func (s *Server) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if l := len(path); l > 1 && path[l-1] == '/' {
		request.URL.Path = path[:l-1]
	}

	if websocket.IsWebSocketRequest(request) {
		s.serveWebSocket(w, request)
	} else {
		s.serveHTTP(w, request)
	}
}

// serveWebSocket serve for websocket protocal
func (s *Server) serveWebSocket(w http.ResponseWriter, request *http.Request) {
	handler, indexer := s.MatchWebSocketHandler(request.URL)
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
	} else {
		conn, err := websocket.UpgradeWebsocket(w, request, s.checker)
		if err == nil {
			handler.Handle(newWebSocketConn(s, conn, indexer))
			indexer.destroySelf()
		} // else connecion will be auto-closed when error occoured,
	}
}

// serveHTTP serve for http protocal
func (s *Server) serveHTTP(w http.ResponseWriter, request *http.Request) {
	url := request.URL
	url.Host = request.Host
	handler, indexer, filters := s.MatchHandlerFilters(url)

	reqEnv := newRequestEnvFromPool()
	res := s.ResMaster.Resource(reqEnv.req.ContentType())
	req := reqEnv.req.init(s, res, request, indexer)
	resp := reqEnv.resp.init(s, res, w)
	if s.contentType != _CONTENTTYPE_DISABLE {
		resp.SetContentType(s.contentType)
	}

	var chain FilterChain
	if handler == nil {
		resp.ReportNotFound()
	} else if chain = FilterChain(handler.Handler(req.Method())); chain == nil {
		resp.ReportMethodNotAllowed()
	}

	newFilterChain(s.RootFilters.Filters(url),
		newFilterChain(filters, chain),
	)(req, resp)

	s.warnLog(req.destroy())
	s.warnLog(resp.destroy())

	recycleRequestEnv(reqEnv)
	recycleFilters(filters)
}

func (o *ServerOption) init() {
	defval.String(&o.ListenAddr, ":4000")
	defval.String(&o.ContentType, resource.CONTENTTYPE_JSON)
	defval.Int(&o.PathVarCount, 3)
	defval.Int(&o.FilterCount, 5)
	if o.KeepAlivePeriod == 0 {
		o.KeepAlivePeriod = 3 * time.Minute // same as net/http/server.go:tcpKeepAliveListener
	}
	if o.Logger == nil {
		o.Logger = DefaultLogger()
	}
}

// all log message before server start will use standard log package
func (s *Server) config(o *ServerOption) {
	o.init()
	s.Log = o.Logger

	log.Print(termcolor.Green.Sprint("ContentType:", o.ContentType))
	s.contentType = o.ContentType
	s.checker = websocket.HeaderChecker(o.WebSocketChecker).HandshakeCheck

	if len(s.ResMaster.Resources) == 0 {
		s.ResMaster.DefUse(resource.RES_JSON, resource.JSON{})
	}

	log.Print(termcolor.Green.Sprint("VarCountPerRoute:", o.PathVarCount))
	pathVarCount = o.PathVarCount
	log.Print(termcolor.Green.Sprint("FilterCountPerRoute:", o.FilterCount))
	filterCount = o.FilterCount

	s.ComponentManager.initHook = func(name string) {
		switch name {
		case _GLOBAL_COMPONENT:
			log.Print(termcolor.Green.Sprint("Init global components"))
		case _ANONYMOUS_COMPONENT:
			log.Print(termcolor.Green.Sprint("Init anonymous components"))
		default:
			log.Print(termcolor.Green.Sprint("  " + name))
		}
	}
	panicOnInit(s.ComponentManager.Init(s))

	log.Print(termcolor.Green.Sprint("Init root filters:"))
	panicOnInit(s.RootFilters.Init(s))

	log.Print(termcolor.Green.Sprint("Init Handlers and Filters:"))
	panicOnInit(s.Router.Init(s))

	log.Print(termcolor.Green.Sprint("Execute registered init funcs:"))
	funcs := s.initFuncs()
	for _, f := range funcs {
		panicOnInit(f())
	}

	// destroy temporary data store
	tmpDestroy()
	log.Print(termcolor.Green.Sprint("Server Start: ", o.ListenAddr))

	runtime.GC()
}

// PanicLog will panic goroutine, be care to call this and note to relase resource
// with 'defer'
func (s *Server) PanicLog(err error) {
	if err != nil {
		s.Log.Panicln(err)
	}
}

func (s *Server) warnLog(err error) {
	if err != nil {
		s.Log.Warnln(err)
	}
}

// Start server as http server, if opt is nil, use default configurations
func (s *Server) Start(opt *ServerOption) error {
	if opt == nil {
		opt = &ServerOption{}
	}
	s.config(opt)

	l, err := s.listen(opt)
	if err == nil {
		s.listener = l
		srv := &http.Server{
			ReadTimeout:  opt.ReadTimeout,
			WriteTimeout: opt.WriteTimeout,
			Handler:      s,
			ConnState:    s.connStateHook,
		}
		err = srv.Serve(l)
	}

	return err
}

// from net/http/server/go
type tcpKeepAliveListener struct {
	*net.TCPListener
	AlivePeriod time.Duration
}

func (ln *tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}

	// if keep-alive fail, don't care
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(time.Duration(ln.AlivePeriod) * time.Minute)

	return tc, nil
}

func (s *Server) listen(opt *ServerOption) (net.Listener, error) {
	ln, err := net.Listen("tcp", opt.ListenAddr)
	if err == nil {
		ln = &tcpKeepAliveListener{
			TCPListener: ln.(*net.TCPListener),
			AlivePeriod: opt.KeepAlivePeriod,
		}

		if opt.TLSConfig != nil {
			ln = tls.NewListener(ln, opt.TLSConfig)
		} else if opt.CertFile != "" {
			// from net/http/server.go.ListenAndServeTLS
			tc := &tls.Config{
				NextProtos:   []string{"http/1.1"},
				Certificates: make([]tls.Certificate, 1),
			}

			tc.Certificates[0], err = tls.LoadX509KeyPair(opt.CertFile, opt.KeyFile)
			if err == nil {
				if opt.CAs != nil {
					tc.ClientCAs, err = tls2.CAPool(opt.CAs...)
					if err == nil {
						tc.ClientAuth = tls.RequireAndVerifyClientCert
					}
				}
				if err == nil {
					ln = tls.NewListener(ln, tc)
				}
			}
		}
	}

	if err != nil && ln != nil {
		s.warnLog(ln.Close())
		ln = nil
	}

	return ln, err
}

func (s *Server) connStateHook(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateActive:
		if atomic.LoadInt32(&s.state) == _NORMAL {
			s.activeConns.Add(1)
		} else {
			// previous idle connections before call server.Destroy() becomes active, directly close it
			s.warnLog(conn.Close())
		}
	case http.StateIdle:
		if atomic.LoadInt32(&s.state) == _DESTROYED {
			s.warnLog(conn.Close())
		}
		s.activeConns.Done()
	case http.StateHijacked:
		s.activeConns.Done()
	}
}

func panicOnInit(err error) {
	if err != nil {
		log.Panicln(err)
	}
}

// Destroy server, release all resources, if destroyed, server can't be reused
// It only wait for managed connections, hijacked/websocket connections will not waiting
// if timeout or server already destroyed, false was returned
func (s *Server) Destroy(timeout time.Duration) bool {
	if !atomic.CompareAndSwapInt32(&s.state, _NORMAL, _DESTROYED) { // signal close idle connections
		return false
	}

	var isTimeout = true
	s.warnLog(s.listener.Close()) // don't accept connections
	if timeout > 0 {
		c := make(chan struct{})
		go func(s *Server, c chan struct{}) {
			s.activeConns.Wait() // wait connections in service to be idle
			close(c)
		}(s, c)

		select {
		case <-time.NewTicker(timeout).C:
		case <-c:
			isTimeout = false
		}
	} else {
		s.activeConns.Wait() // wait connections in service to be idle
	}

	// release resources
	s.RootFilters.Destroy()
	s.Router.Destroy()
	s.ComponentManager.Destroy()

	return !isTimeout
}

func (s *Server) initFuncs() []func() error {
	funcs := TmpHGet(s, "initfuncs")
	if funcs == nil {
		return nil
	}
	return funcs.([]func() error)
}

// AddInitFuncs add functions to execute after all others done and before server start
// don't register component or add handler, filter in these functions unless you know
// what are you doing
func (s *Server) AddInitFuncs(fn ...func() error) {
	TmpHSet(s, "initfuncs", append(s.initFuncs(), fn...))
}
