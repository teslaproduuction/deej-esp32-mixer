// Package serial wraps go.bug.st/serial with the line-protocol used by
// the ESP32 firmware in mixer_proj/firmware: uplink lines
// "v1|v2|v3|v4|v5\n" at 115200 baud, plus arbitrary downlink commands.
package serial

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	gserial "go.bug.st/serial"
)

const NumSliders = 5

// Reader owns one open serial port. Start a single reader and call Stop to
// release the port. ValuesCh emits one frame per uplink line received.
type Reader struct {
	port    gserial.Port
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	values  chan [NumSliders]int
	errors  chan error
	running bool
}

func New() *Reader {
	return &Reader{
		values: make(chan [NumSliders]int, 8),
		errors: make(chan error, 4),
	}
}

func (r *Reader) ValuesCh() <-chan [NumSliders]int { return r.values }
func (r *Reader) ErrorsCh() <-chan error            { return r.errors }

// ListPorts returns the list of currently available COM ports (e.g.
// ["COM1","COM4"]). Display these in the GUI port picker.
func ListPorts() ([]string, error) {
	return gserial.GetPortsList()
}

func (r *Reader) Start(portName string, baud int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		return errors.New("serial reader already running")
	}

	mode := &gserial.Mode{BaudRate: baud}
	p, err := gserial.Open(portName, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", portName, err)
	}

	r.port = p
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	r.running = true

	go r.readLoop()
	return nil
}

func (r *Reader) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	close(r.stop)
	_ = r.port.Close()
	r.running = false
	r.mu.Unlock()

	<-r.done
}

func (r *Reader) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Send writes one downlink line. The firmware expects '\n' termination,
// so this helper appends it for us. Safe to call from any goroutine.
func (r *Reader) Send(cmd string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return errors.New("serial port is not open")
	}
	_, err := r.port.Write([]byte(cmd + "\n"))
	return err
}

func (r *Reader) readLoop() {
	defer close(r.done)

	scanner := bufio.NewScanner(r.port)
	// Default Scanner buffer is plenty for "v1|v2|v3|v4|v5"; bump it
	// just in case the firmware later starts sending longer lines.
	scanner.Buffer(make([]byte, 0, 256), 4096)

	for scanner.Scan() {
		select {
		case <-r.stop:
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		frame, ok := parseFrame(line)
		if !ok {
			// Bootloader noise on reset (\xff\xff…), "entry 0x…", "PONG"
			// replies and similar non-frame text — silently dropped so
			// the GUI status line doesn't fill with garbage.
			continue
		}

		select {
		case r.values <- frame:
		case <-r.stop:
			return
		default:
			// Drop if the consumer is slow — firmware sends 100 frames/s
			// and the GUI only needs the latest.
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case <-r.stop:
		default:
			select {
			case r.errors <- err:
			default:
			}
		}
	}
}

func parseFrame(line string) ([NumSliders]int, bool) {
	var out [NumSliders]int
	parts := strings.Split(line, "|")
	if len(parts) != NumSliders {
		return out, false
	}
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return out, false
		}
		out[i] = v
	}
	return out, true
}
