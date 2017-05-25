// Copyright (c) 2017 Dave Pifke.
//
// Redistribution and use in source and binary forms, with or without
// modification, is permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice,
//    this list of conditions and the following disclaimer.
//
// 2. Redistributions in binary form must reproduce the above copyright notice,
//    this list of conditions and the following disclaimer in the documentation
//    and/or other materials provided with the distribution.
//
// 3. Neither the name of the copyright holder nor the names of its
//    contributors may be used to endorse or promote products derived from
//    this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

package wpasupplicant

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
)

// message is a queued response (or read error) from the wpa_supplicant
// daemon.  Messages may be either solicited or unsolicited.
type message struct {
	priority int
	data     []byte
	err      error
}

// unixgramConn is the implementation of Conn for the AF_UNIX SOCK_DGRAM
// control interface.
//
// See https://w1.fi/wpa_supplicant/devel/ctrl_iface_page.html.
type unixgramConn struct {
	c                      *net.UnixConn
	fd                     uintptr
	solicited, unsolicited chan message
}

// socketPath is where to find the the AF_UNIX sockets for each interface.  It
// can be overridden for testing.
var socketPath = "/run/wpa_supplicant"

// Unixgram returns a connection to wpa_supplicant for the specified
// interface, using the socket-based control interface.
func Unixgram(ifName string) (Conn, error) {
	var err error
	uc := &unixgramConn{}

	local, err := ioutil.TempFile("/tmp", "wpa_supplicant")
	if err != nil {
		panic(err)
	}
	os.Remove(local.Name())

	uc.c, err = net.DialUnix("unixgram",
		&net.UnixAddr{Name: local.Name(), Net: "unixgram"},
		&net.UnixAddr{Name: path.Join(socketPath, ifName), Net: "unixgram"})
	if err != nil {
		return nil, err
	}

	file, err := uc.c.File()
	if err != nil {
		return nil, err
	}
	uc.fd = file.Fd()

	uc.solicited = make(chan message)
	uc.unsolicited = make(chan message)

	go uc.readLoop()

	// TODO: issue an ACCEPT command so as to receive unsolicited
	// messages.  (We don't do this yet, since we don't yet have any way
	// to consume them.)

	return uc, nil
}

// readLoop is spawned after we connect.  It receives messages from the
// socket, and routes them to the appropriate channel based on whether they
// are solicited (in response to a request) or unsolicited.
func (uc *unixgramConn) readLoop() {
	for {
		// The syscall below will block until a datagram is received.
		// It uses a zero-length buffer to look at the datagram
		// without discarding it (MSG_PEEK), returning the actual
		// datagram size (MSG_TRUNC).  See the recvfrom(2) man page.
		//
		// The actual read occurs using UnixConn.Read(), once we've
		// allocated an appropriately-sized buffer.
		n, _, err := syscall.Recvfrom(int(uc.fd), []byte{}, syscall.MSG_PEEK|syscall.MSG_TRUNC)
		if err != nil {
			// Treat read errors as a response to whatever command
			// was last issued.
			uc.solicited <- message{
				err: err,
			}
			continue
		}

		buf := make([]byte, n)
		_, err = uc.c.Read(buf[:])
		if err != nil {
			uc.solicited <- message{
				err: err,
			}
			continue
		}

		// Unsolicited messages are preceded by a priority
		// specification, e.g. "<1>message".  If there's no priority,
		// default to 2 (info) and assume it's the response to
		// whatever command was last issued.
		var p int
		var c chan message
		if len(buf) >= 3 && buf[0] == '<' && buf[2] == '>' {
			switch buf[1] {
			case '0', '1', '2', '3', '4':
				c = uc.unsolicited
				p, _ = strconv.Atoi(string(buf[1]))
				buf = buf[3:]
			default:
				c = uc.solicited
				p = 2
			}
		} else {
			c = uc.solicited
			p = 2
		}

		c <- message{
			priority: p,
			data:     buf,
		}
	}
}

// cmd executes a command and waits for a reply.
func (uc *unixgramConn) cmd(cmd string) ([]byte, error) {
	// TODO: block if any other commands are running

	_, err := uc.c.Write([]byte(cmd))
	if err != nil {
		return nil, err
	}

	msg := <-uc.solicited
	return msg.data, msg.err
}

// ParseError is returned when we can't parse the wpa_supplicant response.
// Some functions may return multiple ParseErrors.
type ParseError struct {
	// Line is the line of output from wpa_supplicant which we couldn't
	// parse.
	Line string

	// Err is any nested error.
	Err error
}

func (err *ParseError) Error() string {
	b := &bytes.Buffer{}
	b.WriteString("failed to parse wpa_supplicant response")

	if err.Line != "" {
		fmt.Fprintf(b, ": %q", err.Line)
	}

	if err.Err != nil {
		fmt.Fprintf(b, ": %s", err.Err.Error())
	}

	return b.String()
}

func (uc *unixgramConn) Ping() error {
	resp, err := uc.cmd("PING")
	if err != nil {
		return err
	}

	if bytes.Compare(resp, []byte("PONG\n")) == 0 {
		return nil
	}
	return &ParseError{Line: string(resp)}
}

func (uc *unixgramConn) ScanResults() ([]ScanResult, []error) {
	resp, err := uc.cmd("SCAN_RESULTS")
	if err != nil {
		return nil, []error{err}
	}

	return parseScanResults(bytes.NewBuffer(resp))
}

// parseScanResults parses the SCAN_RESULTS output from wpa_supplicant.  This
// is split out from ScanResults() to make testing easier.
func parseScanResults(resp io.Reader) (res []ScanResult, errs []error) {
	// In an attempt to make our parser more resilient, we start by
	// parsing the header line and using that to determine the column
	// order.
	s := bufio.NewScanner(resp)
	if !s.Scan() {
		errs = append(errs, &ParseError{})
		return
	}
	bssidCol, freqCol, rssiCol, flagsCol, ssidCol, maxCol := -1, -1, -1, -1, -1, -1
	for n, col := range strings.Split(s.Text(), " / ") {
		switch col {
		case "bssid":
			bssidCol = n
		case "frequency":
			freqCol = n
		case "signal level":
			rssiCol = n
		case "flags":
			flagsCol = n
		case "ssid":
			ssidCol = n
		}
		maxCol = n
	}

	var err error
	for s.Scan() {
		ln := s.Text()
		fields := strings.Split(ln, "\t")
		if len(fields) < maxCol {
			errs = append(errs, &ParseError{Line: ln})
			continue
		}

		var bssid net.HardwareAddr
		if bssidCol != -1 {
			if bssid, err = net.ParseMAC(fields[bssidCol]); err != nil {
				errs = append(errs, &ParseError{Line: ln, Err: err})
				continue
			}
		}

		var freq int
		if freqCol != -1 {
			if freq, err = strconv.Atoi(fields[freqCol]); err != nil {
				errs = append(errs, &ParseError{Line: ln, Err: err})
				continue
			}
		}

		var rssi int
		if rssiCol != -1 {
			if rssi, err = strconv.Atoi(fields[rssiCol]); err != nil {
				errs = append(errs, &ParseError{Line: ln, Err: err})
				continue
			}
		}

		var flags []string
		if flagsCol != -1 {
			if len(fields[flagsCol]) >= 2 && fields[flagsCol][0] == '[' && fields[flagsCol][len(fields[flagsCol])-1] == ']' {
				flags = strings.Split(fields[flagsCol][1:len(fields[flagsCol])-1], "][")
			}
		}

		var ssid string
		if ssidCol != -1 {
			ssid = fields[ssidCol]
		}

		res = append(res, &scanResult{
			bssid:     bssid,
			frequency: freq,
			rssi:      rssi,
			flags:     flags,
			ssid:      ssid,
		})
	}

	return
}