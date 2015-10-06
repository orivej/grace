package gracehttp

import (
	"crypto/tls"
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

// Serve either serves on the socket in FD 3 if started with systemd-compatible
// protocol, or:
// - starts /proc/self/exe in a subprocess with systemd-compatible protocol
// - gracefully terminates subprocess on SIGTERM
// - gracefully restarts subprocess on SIGUSR2
func Serve(servers ...*http.Server) error {
	// Simplify LISTEN_PID protocol because it is harder to set PID between
	// fork and exec in Golang.
	if os.Getenv(listenPID) == "0" {
		if err := os.Setenv(listenPID, strconv.Itoa(os.Getpid())); err != nil {
			return err
		}
	}

	listeners, _ := activation.Listeners(true) // ignore error: always nil
	if len(listeners) != 0 {
		// Serve.
		return serve(listeners, servers)
	}

	// Listen and fork&exec to serve.
	fds := make([]*os.File, len(servers))
	for i, s := range servers {
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}

		if s.TLSConfig != nil {
			l = tls.NewListener(l, s.TLSConfig)
		}

		fds[i], err = l.(*net.TCPListener).File()
		if err != nil {
			return err
		}
	}

	return runServer(fds)
}

func serve(listeners []net.Listener, servers []*http.Server) error {
	hdServers := make([]httpdown.Server, len(listeners))
	for i, l := range listeners {
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
	if _, err := p.Wait(); err != nil {
		return err
	}
	return nil
}
