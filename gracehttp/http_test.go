package gracehttp_test

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/facebookgo/freeport"
)

// Debug logging.
var debugLog = flag.Bool("debug", false, "enable debug logging")

func debug(format string, a ...interface{}) {
	if *debugLog {
		println(fmt.Sprintf(format, a...))
	}
}

// State for the test run.
type harness struct {
	T                 *testing.T     // The test instance.
	httpAddr          string         // The address for the http server.
	httpsAddr         string         // The address for the https server.
	Process           *os.Process    // The server launcher.
	RequestWaitGroup  sync.WaitGroup // The wait group for the HTTP requests.
	newProcess        chan bool      // A bool is sent on start/restart.
	requestCount      int
	requestCountMutex sync.Mutex
}

// Find 3 free ports and setup addresses.
func (h *harness) setupAddr() {
	port, err := freeport.Get()
	if err != nil {
		h.T.Fatalf("Failed to find a free port: %s", err)
	}
	h.httpAddr = fmt.Sprintf("127.0.0.1:%d", port)

	port, err = freeport.Get()
	if err != nil {
		h.T.Fatalf("Failed to find a free port: %s", err)
	}
	h.httpsAddr = fmt.Sprintf("127.0.0.1:%d", port)
	debug("Addresses %s & %s", h.httpAddr, h.httpsAddr)
}

// Start a fresh server and wait for pid updates on restart.
func (h *harness) Start() {
	h.setupAddr()
	cmd := exec.Command(os.Args[0], "-http", h.httpAddr, "-https", h.httpsAddr)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		h.T.Fatal(err)
	}
	go func() {
		reader := bufio.NewReader(stderr)
		for {
			line, isPrefix, err := reader.ReadLine()
			if err == io.EOF {
				return
			}
			if err != nil {
				println(fmt.Sprintf("Failed to read line from server process: %s", err))
			}
			if isPrefix {
				println(fmt.Sprintf("Deal with isPrefix for line: %s", line))
			}
			res := &response{}
			err = json.Unmarshal([]byte(line), res)
			if err != nil {
				println(fmt.Sprintf("Could not parse json from stderr %s: %s", line, err))
			}
			if res.Error != "" {
				println(fmt.Sprintf("Got error from process: %v", res))
			}
			h.newProcess <- true
		}
	}()
	err = cmd.Start()
	if err != nil {
		h.T.Fatalf("Failed to start command: %s", err)
	}
	h.Process = cmd.Process
	<-h.newProcess
}

// Restart the most recent server.
func (h *harness) Restart() {
	err := h.Process.Signal(syscall.SIGUSR2)
	if err != nil {
		h.T.Fatalf("Failed to send SIGUSR2 and restart process: %s", err)
	}
	<-h.newProcess
}

// Graceful termination of the most recent server.
func (h *harness) Stop() {
	err := h.Process.Signal(syscall.SIGTERM)
	if err != nil {
		h.T.Fatalf("Failed to send SIGTERM and stop process: %s", err)
	}
}

// Get the global request count and increment it.
func (h *harness) RequestCount() int {
	h.requestCountMutex.Lock()
	defer h.requestCountMutex.Unlock()
	c := h.requestCount
	h.requestCount++
	return c
}

// Helper for sending a single request.
func (h *harness) SendOne(dialgroup *sync.WaitGroup, url string, pid int) {
	defer h.RequestWaitGroup.Done()
	count := h.RequestCount()
	debug("Send %02d pid=%d url=%s", count, pid, url)
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				defer func() {
					time.Sleep(50 * time.Millisecond)
					dialgroup.Done()
				}()
				return net.Dial(network, addr)
			},
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	r, err := client.Get(url)
	if err != nil {
		h.T.Fatalf("Failed request %02d to %s pid=%d: %s", count, url, pid, err)
	}
	debug("Body %02d pid=%d url=%s", count, pid, url)
	defer r.Body.Close()
	res := &response{}
	err = json.NewDecoder(r.Body).Decode(res)
	if err != nil {
		h.T.Fatalf("Failed to ready decode json response body pid=%d: %s", pid, err)
	}
	debug("Done %02d pid=%d url=%s", count, pid, url)
}

// Send test HTTP request.
func (h *harness) SendRequest() {
	pid := h.Process.Pid
	httpFastURL := fmt.Sprintf("http://%s/sleep/?duration=0", h.httpAddr)
	httpSlowURL := fmt.Sprintf("http://%s/sleep/?duration=2s", h.httpAddr)
	httpsFastURL := fmt.Sprintf("https://%s/sleep/?duration=0", h.httpsAddr)
	httpsSlowURL := fmt.Sprintf("https://%s/sleep/?duration=2s", h.httpsAddr)

	var dialgroup sync.WaitGroup
	h.RequestWaitGroup.Add(4)
	dialgroup.Add(4)
	go h.SendOne(&dialgroup, httpFastURL, pid)
	go h.SendOne(&dialgroup, httpSlowURL, pid)
	go h.SendOne(&dialgroup, httpsFastURL, pid)
	go h.SendOne(&dialgroup, httpsSlowURL, pid)
	debug("Added Requests pid=%d", pid)
	dialgroup.Wait()
	debug("Dialed Requests pid=%d", pid)
}

// Wait for everything.
func (h *harness) Wait() {
	h.RequestWaitGroup.Wait()
}

func newHarness(t *testing.T) *harness {
	return &harness{
		T:          t,
		newProcess: make(chan bool),
	}
}

// The main test case.
func TestComplex(t *testing.T) {
	t.Parallel()
	debug("Started TestComplex")
	h := newHarness(t)
	debug("Initial Start")
	h.Start()
	debug("Send Request 1")
	h.SendRequest()
	debug("Restart 1")
	h.Restart()
	debug("Send Request 2")
	h.SendRequest()
	debug("Restart 2")
	h.Restart()
	debug("Send Request 3")
	h.SendRequest()
	debug("Stopping")
	h.Stop()
	debug("Waiting")
	h.Wait()
}

func TestComplexAgain(t *testing.T) {
	t.Parallel()
	debug("Started TestComplex")
	h := newHarness(t)
	debug("Initial Start")
	h.Start()
	debug("Send Request 1")
	h.SendRequest()
	debug("Restart 1")
	h.Restart()
	debug("Send Request 2")
	h.SendRequest()
	debug("Restart 2")
	h.Restart()
	debug("Send Request 3")
	h.SendRequest()
	debug("Stopping")
	h.Stop()
	debug("Waiting")
	h.Wait()
}
