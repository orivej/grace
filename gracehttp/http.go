package gracehttp

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/coreos/go-systemd/activation"
	"github.com/coreos/go-systemd/daemon"
	"github.com/facebookgo/httpdown"
)

const (
	listenPID = "LISTEN_PID"
	listenFDs = "LISTEN_FDS"
	sdReady   = "READY=1"
)

var verbose = flag.Bool("gracehttp.log", false, "Enable logging.")

// Serve either serves on the previously opened sockets if started with
// systemd-compatible protocol, or:
// - opens sockets and starts /proc/self/exe in a subprocess with
//   systemd-compatible protocol
// - gracefully terminates subprocess on SIGTERM
// - gracefully restarts subprocess on SIGUSR2
func Serve(servers ...*http.Server) error {
	// Simplify LISTEN_PID protocol because it is harder to set PID between
	// fork and exec in Golang.
	pid := os.Getenv(listenPID)
	initActivated := len(pid) != 0 && pid != "0"
	if pid == "0" {
		if err := os.Setenv(listenPID, strconv.Itoa(os.Getpid())); err != nil {
			return err
		}
	}

	listeners, _ := activation.Listeners(true) // ignore error: always nil
	didInherit := len(listeners) != 0

	if *verbose {
		if didInherit {
			if initActivated {
				log.Printf("Listening on init activated %s", pprintAddr(servers))
			} else {
				const msg = "Graceful handoff of %s with new pid %d"
				log.Printf(msg, pprintAddr(servers), os.Getpid())
			}
		} else {
			const msg = "Serving %s with pid %d"
			log.Printf(msg, pprintAddr(servers), os.Getpid())
		}
	}

	if didInherit {
		// Serve.
		return serve(listeners, servers)
	}

	// Listen and fork&exec to serve.
	fds := make([]*os.File, len(servers))
	for i, srv := range servers {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return err
		}

		fds[i], err = l.(*net.TCPListener).File()
		if err != nil {
			return err
		}
		defer fds[i].Close() // ignore error
	}

	return runServer(fds)
}

func serve(listeners []net.Listener, servers []*http.Server) error {
	hdServers := make([]httpdown.Server, len(listeners))
	for i, l := range listeners {
		if servers[i].TLSConfig != nil {
			l = tls.NewListener(l, servers[i].TLSConfig)
		}
		hdServers[i] = httpdown.HTTP{}.Serve(servers[i], l)
	}
	_ = daemon.SdNotify(sdReady) // ignore error

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGTERM)
	<-ch
	signal.Stop(ch)

	errs := make([]error, len(listeners))
	wg := sync.WaitGroup{}
	for i := range hdServers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = hdServers[i].Stop()
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func runServer(fds []*os.File) error {
	if err := os.Setenv(listenPID, "0"); err != nil {
		return err
	}
	if err := os.Setenv(listenFDs, strconv.Itoa(len(fds))); err != nil {
		return err
	}

	cmd, err := startProcess(fds)
	if err != nil {
		return err
	}

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGUSR2)
	for {
		sig := <-ch
		switch sig {
		case syscall.SIGTERM:
			return endProcess(cmd.Process)
		case syscall.SIGUSR2:
			go endProcess(cmd.Process) // ignore error
			if cmd, err = startProcess(fds); err != nil {
				return err
			}
		}
	}
}

func startProcess(fds []*os.File) (exec.Cmd, error) {
	cmd := exec.Cmd{
		Path:       "/proc/self/exe",
		Args:       os.Args,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		ExtraFiles: fds,
	}
	return cmd, cmd.Start()
}

func endProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	_, err := p.Wait()
	if *verbose {
		log.Printf("Exiting pid %d.", p.Pid)
	}
	return err
}

// Used for pretty printing addresses.
func pprintAddr(servers []*http.Server) []byte {
	var out bytes.Buffer
	for i, srv := range servers {
		if i != 0 {
			fmt.Fprint(&out, ", ")
		}
		fmt.Fprint(&out, srv.Addr)
	}
	return out.Bytes()
}
