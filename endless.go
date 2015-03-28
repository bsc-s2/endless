package endless

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	// "github.com/fvbock/uds-go/introspect"
)

const (
	PRE_SIGNAL  = 0
	POST_SIGNAL = 1
)

var (
	runningServerReg     sync.Mutex
	runningServers       map[string]*endlessServer
	runningServersOrder  map[int]string
	runningServersForked bool

	DefaultReadTimeOut  time.Duration
	DefaultWriteTimeOut time.Duration

	isChild bool
)

func init() {
	flag.BoolVar(&isChild, "continue", false, "listen on open fd (after forking)")
	flag.Parse()

	runningServerReg = sync.Mutex{}
	runningServers = make(map[string]*endlessServer)
	runningServersOrder = make(map[int]string)
}

type endlessServer struct {
	http.Server
	EndlessListener  net.Listener
	tlsInnerListener *endlessListener
	wg               sync.WaitGroup
	sigChan          chan os.Signal
	isChild          bool
	SignalHooks      map[int]map[os.Signal][]func()
}

func NewServer(addr string, handler http.Handler) (srv *endlessServer) {
	srv = &endlessServer{
		wg:      sync.WaitGroup{},
		sigChan: make(chan os.Signal),
		isChild: isChild,
		SignalHooks: map[int]map[os.Signal][]func(){
			PRE_SIGNAL: map[os.Signal][]func(){
				syscall.SIGHUP:  []func(){},
				syscall.SIGUSR1: []func(){},
				syscall.SIGUSR2: []func(){},
				syscall.SIGINT:  []func(){},
				syscall.SIGTERM: []func(){},
				syscall.SIGTSTP: []func(){},
			},
			POST_SIGNAL: map[os.Signal][]func(){
				syscall.SIGHUP:  []func(){},
				syscall.SIGUSR1: []func(){},
				syscall.SIGUSR2: []func(){},
				syscall.SIGINT:  []func(){},
				syscall.SIGTERM: []func(){},
				syscall.SIGTSTP: []func(){},
			},
		},
	}

	srv.Server.Addr = addr
	srv.Server.ReadTimeout = DefaultReadTimeOut
	srv.Server.WriteTimeout = DefaultWriteTimeOut
	// srv.Server.MaxHeaderBytes = 1 << 16
	srv.Server.Handler = handler

	runningServerReg.Lock()
	runningServersOrder[len(runningServers)] = addr
	runningServers[addr] = srv
	runningServerReg.Unlock()

	return
}

func ListenAndServe(addr string, handler http.Handler) error {
	server := NewServer(addr, handler)
	return server.ListenAndServe()
}

func (srv *endlessServer) ListenAndServe() (err error) {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}

	go srv.handleSignals()

	l, err := srv.getListener(addr)
	if err != nil {
		log.Println(err)
		return
	}

	srv.EndlessListener = newEndlessListener(l, srv)

	if srv.isChild {
		syscall.Kill(syscall.Getppid(), syscall.SIGTERM)
	}

	log.Println(syscall.Getpid(), srv.Addr)
	return srv.Serve()
}

func (srv *endlessServer) Serve() (err error) {
	err = srv.Server.Serve(srv.EndlessListener)
	log.Println(syscall.Getpid(), "Waiting for connections to finish...")
	srv.wg.Wait()
	return
}

func ListenAndServeTLS(addr string, certFile string, keyFile string, handler http.Handler) error {
	server := NewServer(addr, handler)
	return server.ListenAndServeTLS(certFile, keyFile)
}

func (srv *endlessServer) ListenAndServeTLS(certFile, keyFile string) (err error) {
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	config := &tls.Config{}
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"http/1.1"}
	}

	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return
	}

	go srv.handleSignals()

	l, err := srv.getListener(addr)
	if err != nil {
		log.Println(err)
		return
	}

	srv.tlsInnerListener = newEndlessListener(l, srv)
	srv.EndlessListener = tls.NewListener(srv.tlsInnerListener, config)

	if srv.isChild {
		syscall.Kill(syscall.Getppid(), syscall.SIGTERM)
	}

	log.Println(syscall.Getpid(), srv.Addr)
	return srv.Serve()
}

func (srv *endlessServer) getListener(laddr string) (l net.Listener, err error) {
	if srv.isChild {
		var ptrOffset uint = 0
		// wonder whether starting servers in goroutines could create a
		// race which ends up assigning the wrong fd... maybe add Addr
		// to the registry of runningServers
		// UPDATE: yes. it *can* happen ;)
		for i, addr := range runningServersOrder {
			if addr == laddr {
				ptrOffset = uint(i)
				break
			}
		}
		log.Println("addr", laddr, ">>> ptr 3 +", ptrOffset)
		f := os.NewFile(uintptr(3+ptrOffset), "")
		l, err = net.FileListener(f)
		if err != nil {
			err = fmt.Errorf("net.FileListener error:", err)
			return
		}
	} else {
		// l, err = net.Listen("tcp", srv.Server.Addr)
		l, err = net.Listen("tcp", laddr)
		if err != nil {
			err = fmt.Errorf("net.Listen error:", err)
			return
		}
	}
	return
}

func (srv *endlessServer) handleSignals() {
	var sig os.Signal

	signal.Notify(
		srv.sigChan,
		syscall.SIGHUP,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGTSTP,
	)

	pid := syscall.Getpid()
	for {
		sig = <-srv.sigChan
		srv.signalHooks(PRE_SIGNAL, sig)
		switch sig {
		case syscall.SIGHUP:
			log.Println(pid, "Received SIGHUP. forking.")
			err := srv.fork()
			if err != nil {
				log.Println("Fork err:", err)
			}
		case syscall.SIGUSR1:
			log.Println(pid, "Received SIGUSR1.")
		case syscall.SIGUSR2:
			log.Println(pid, "Received SIGUSR2.")
		case syscall.SIGINT:
			log.Println(pid, "Received SIGINT.")
			srv.shutdown()
		case syscall.SIGTERM:
			log.Println(pid, "Received SIGTERM.")
			srv.shutdown()
		case syscall.SIGTSTP:
			log.Println(pid, "Received SIGTSTP.")
		default:
			log.Printf("Received %v: nothing i care about....\n", sig)
		}
		srv.signalHooks(POST_SIGNAL, sig)
	}
}

func (srv *endlessServer) signalHooks(ppFlag int, sig os.Signal) {
	if _, notSet := srv.SignalHooks[ppFlag][sig]; !notSet {
		return
	}
	for _, f := range srv.SignalHooks[ppFlag][sig] {
		f()
	}
	return
}

func (srv *endlessServer) shutdown() {
	err := srv.EndlessListener.Close()
	if err != nil {
		log.Println(syscall.Getpid(), "srv.EndlessListener.Close() error:", err)
	} else {
		log.Println(syscall.Getpid(), srv.EndlessListener.Addr(), "srv.EndlessListener closed.")
	}
}

// /* TODO: add this
// hammerTime forces the server to shutdown in a given timeout - whether it
// finished outstanding requests or not. if Read/WriteTimeout are not set or the
// max header size is 0 a connection could hang...
// */
// func (srv *endlessServer) hammerTime(d time.Duration) (err error) {
// 	log.Println("[STOP - HAMMER TIME] Forcefully shutting down parent.")
// 	return
// }

func (srv *endlessServer) fork() (err error) {
	// only one server isntance should fork!
	runningServerReg.Lock()
	defer runningServerReg.Unlock()
	if runningServersForked {
		return
	}
	runningServersForked = true

	var files []*os.File
	// get the accessor socket fds for _all_ server instances
	for _, srvPtr := range runningServers {
		// introspect.PrintTypeDump(srvPtr.EndlessListener)
		switch srvPtr.EndlessListener.(type) {
		case *endlessListener:
			// log.Println("normal listener")
			files = append(files, srvPtr.EndlessListener.(*endlessListener).File()) // returns a dup(2) - FD_CLOEXEC flag *not* set
		default:
			// log.Println("tls listener")
			files = append(files, srvPtr.tlsInnerListener.File()) // returns a dup(2) - FD_CLOEXEC flag *not* set
		}
	}

	path := os.Args[0]
	args := []string{"-continue"}

	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = files

	err = cmd.Start()
	if err != nil {
		log.Fatalf("Restart: Failed to launch, error: %v", err)
	}

	return
}

type endlessListener struct {
	net.Listener
	stop    chan error
	stopped bool
	server  *endlessServer
}

func (el *endlessListener) Accept() (c net.Conn, err error) {
	// c, err = el.Listener.Accept()
	tc, err := el.Listener.(*net.TCPListener).AcceptTCP()
	if err != nil {
		return
	}

	tc.SetKeepAlive(true)                  // see http.tcpKeepAliveListener
	tc.SetKeepAlivePeriod(3 * time.Minute) // see http.tcpKeepAliveListener

	c = endlessConn{
		Conn:   tc,
		server: el.server,
	}

	el.server.wg.Add(1)
	return
}

func newEndlessListener(l net.Listener, srv *endlessServer) (el *endlessListener) {
	el = &endlessListener{
		Listener: l,
		stop:     make(chan error),
		server:   srv,
	}

	go func() {
		_ = <-el.stop
		el.stopped = true
		el.stop <- el.Listener.Close()
	}()
	return
}

func (el *endlessListener) Close() error {
	if el.stopped {
		return syscall.EINVAL
	}
	el.stop <- nil
	return <-el.stop
}

func (el *endlessListener) File() *os.File {
	tl := el.Listener.(*net.TCPListener)
	fl, _ := tl.File()
	return fl
}

type endlessConn struct {
	net.Conn
	server *endlessServer
}

func (w endlessConn) Close() error {
	w.server.wg.Done()
	return w.Conn.Close()
}