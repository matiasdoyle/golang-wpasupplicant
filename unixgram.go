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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/apex/log"
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
	c                                       *net.UnixConn
	fd                                      uintptr
	file                                    *os.File
	solicited, unsolicited                  chan message
	wpaEvents                               chan WPAEvent
	unsolicitedCloseChan, readLoopCloseChan chan bool
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
		return nil, err
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

	// This specifies whether we are listening to WPA events or not.
	// If we don't, this would block if they were sent.
	uc.file = file
	uc.fd = file.Fd()

	uc.solicited = make(chan message)
	uc.unsolicited = make(chan message)
	uc.wpaEvents = make(chan WPAEvent)
	uc.readLoopCloseChan = make(chan bool)
	uc.unsolicitedCloseChan = make(chan bool)

	go uc.readLoop()
	go uc.readUnsolicited()
	// Issue an ATTACH command to start receiving unsolicited events.
	err = uc.runCommand("ATTACH")
	if err != nil {
		return nil, err
	}

	return uc, nil
}

// readLoop is spawned after we connect.  It receives messages from the
// socket, and routes them to the appropriate channel based on whether they
// are solicited (in response to a request) or unsolicited.
func (uc *unixgramConn) readLoop() error {
	for {
		// The syscall below uses a zero-length buffer to look at the datagram
		// without discarding it (MSG_PEEK), returning the actual
		// datagram size (MSG_TRUNC). It will not wait to receieve a datagram to avoid
		// hanging goroutines.
		// See the recvfrom(2) man page.
		//
		// The actual read occurs using UnixConn.Read(), once we've
		// allocated an appropriately-sized buffer.
		n, _, err := syscall.Recvfrom(int(uc.fd), []byte{}, syscall.MSG_PEEK|syscall.MSG_TRUNC|syscall.MSG_DONTWAIT)
		if err != nil {
			// Treat read errors as a response to whatever command
			// was last issued. If it returned to avoid block check if we should exit
			// or continue.
			if err == syscall.EWOULDBLOCK {
				time.Sleep(1 * time.Second)
				select {
				case <-uc.readLoopCloseChan:
					return nil
				default:
					continue
				}
			}
			select {
			case uc.solicited <- message{
				err: err,
			}:
				continue
			case <-uc.readLoopCloseChan:
				return nil
			}
		}

		buf := make([]byte, n)
		_, err = uc.c.Read(buf[:])
		if err != nil {
			select {
			case uc.solicited <- message{
				err: err,
			}:
				continue
			case <-uc.readLoopCloseChan:
				return nil
			}
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

		select {
		case c <- message{
			priority: p,
			data:     buf,
		}:
		case <-uc.readLoopCloseChan:
			return nil
		}
	}
}

// readUnsolicited handles messages sent to the unsolicited channel and parse them
// into a WPAEvent. At the moment we only handle `CTRL-EVENT-*` events and only events
// where the 'payload' is formatted with key=val.
func (uc *unixgramConn) readUnsolicited() {
	for {
		select {
		case mgs := <-uc.unsolicited:
			data := bytes.NewBuffer(mgs.data).String()

			parts := strings.Split(data, " ")
			if len(parts) == 0 {
				continue
			}

			if strings.Index(parts[0], "CTRL-") != 0 {
				select {
				case uc.wpaEvents <- WPAEvent{
					Event: "MESSAGE",
					Line:  data,
				}:
					continue
				case <-uc.unsolicitedCloseChan:
					return
				}
			}

			event := WPAEvent{
				Arguments: make(map[string]string),
				Line:      data,
			}
			if len(parts) >= 6 && parts[5] == "reason=WRONG_KEY" {
				event.Event = "BAD-PASSPHRASE"
			} else {
				event.Event = strings.TrimPrefix(parts[0], "CTRL-EVENT-")
			}

			for _, args := range parts[1:] {
				if strings.Contains(args, "=") {
					keyval := strings.Split(args, "=")
					if len(keyval) != 2 {
						continue
					}
					event.Arguments[keyval[0]] = keyval[1]
				}
			}

			select {
			case uc.wpaEvents <- event:
			case <-uc.unsolicitedCloseChan:
				return
			}
		case <-uc.unsolicitedCloseChan:
			close(uc.wpaEvents)
			return
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

	select {
	case msg := <-uc.solicited:
		return msg.data, msg.err
	case <-time.After(30 * time.Second):
		return nil, errors.New("Timed out waiting for response")
	}
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

func (uc *unixgramConn) EventQueue() chan WPAEvent {
	return uc.wpaEvents
}

func (uc *unixgramConn) Close() error {
	if err := uc.runCommand("DETACH"); err != nil {
		log.WithError(err).Error("Error closing uc uc.runCommand DETACH")
	}
	go uc.stopGoroutines()

	if err := uc.file.Close(); err != nil {
		log.WithError(err).Error("Error closing uc uc.file.Close()")
	}

	if err := uc.c.Close(); err != nil {
		log.WithError(err).Error("Error closing uc uc.c.Close()")
	}
	return nil
}

func (uc *unixgramConn) stopGoroutines() {
	select {
	case uc.unsolicitedCloseChan <- true:
	case <-time.After(20 * time.Second):
		log.Error("Could not send close to unsolicited")
	}
	select {
	case uc.readLoopCloseChan <- true:
	case <-time.After(20 * time.Second):
		log.Error("Could not send close to read loop")
	}
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

func (uc *unixgramConn) AddNetwork() (int, error) {
	resp, err := uc.cmd("ADD_NETWORK")
	if err != nil {
		return -1, err
	}

	b := bytes.NewBuffer(resp)
	return strconv.Atoi(strings.Trim(b.String(), "\n"))
}

func (uc *unixgramConn) EnableNetwork(networkID int) error {
	return uc.runCommand(fmt.Sprintf("ENABLE_NETWORK %d", networkID))
}

func (uc *unixgramConn) EnableAllNetworks() error {
	return uc.runCommand("ENABLE_NETWORK all")
}

func (uc *unixgramConn) SelectNetwork(networkID int) error {
	return uc.runCommand(fmt.Sprintf("SELECT_NETWORK %d", networkID))
}

func (uc *unixgramConn) DisableNetwork(networkID int) error {
	return uc.runCommand(fmt.Sprintf("DISABLE_NETWORK %d", networkID))
}

func (uc *unixgramConn) RemoveNetwork(networkID int) error {
	return uc.runCommand(fmt.Sprintf("REMOVE_NETWORK %d", networkID))
}

func (uc *unixgramConn) RemoveAllNetworks() error {
	return uc.runCommand("REMOVE_NETWORK all")
}

func (uc *unixgramConn) SetNetwork(networkID int, variable string, value string) error {
	var cmd string

	// Since key_mgmt and priority expects the value to not be wrapped in "" we do a little check here.
	if variable == "key_mgmt" || variable == "priority" || variable == "eap" || variable == "scan_ssid" {
		cmd = fmt.Sprintf("SET_NETWORK %d %s %s", networkID, variable, value)
	} else {
		cmd = fmt.Sprintf("SET_NETWORK %d %s \"%s\"", networkID, variable, value)
	}

	return uc.runCommand(cmd)
}

func (uc *unixgramConn) GetNetwork(networkID int, variable string) (string, error) {
	resp, err := uc.cmd(fmt.Sprintf("GET_NETWORK %d %s", networkID, variable))
	if err != nil {
		return "ERROR", err
	}
	if string(resp) == "FAIL\n" {
		return "FAIL", errors.New("Failed")
	}
	if variable == "ssid" {
		return decodeByteLiteralString(string(resp)), nil
	} else {
		n := len(resp)
		s := string(resp[:n])
		return s, nil
	}
}

func (uc *unixgramConn) SaveConfig() error {
	return uc.runCommand("SAVE_CONFIG")
}

func (uc *unixgramConn) Reconfigure() error {
	return uc.runCommand("RECONFIGURE")
}

func (uc *unixgramConn) Reassociate() error {
	return uc.runCommand("REASSOCIATE")
}

func (uc *unixgramConn) Reconnect() error {
	return uc.runCommand("RECONNECT")
}

func (uc *unixgramConn) Scan() error {
	return uc.runCommand("SCAN")
}

func (uc *unixgramConn) ScanResults() ([]ScanResult, []error) {
	resp, err := uc.cmd("SCAN_RESULTS")
	if err != nil {
		return nil, []error{err}
	}

	return parseScanResults(bytes.NewBuffer(resp))
}

func (uc *unixgramConn) Status() (StatusResult, error) {
	resp, err := uc.cmd("STATUS")
	if err != nil {
		return nil, err
	}

	return parseStatusResults(bytes.NewBuffer(resp))
}

func (uc *unixgramConn) ListNetworks() ([]ConfiguredNetwork, error) {
	resp, err := uc.cmd("LIST_NETWORKS")
	if err != nil {
		return nil, err
	}

	return parseListNetworksResult(bytes.NewBuffer(resp))
}

// runCommand is a wrapper around the uc.cmd command which makes sure the
// command returned a successful (OK) response.
func (uc *unixgramConn) runCommand(cmd string) error {
	resp, err := uc.cmd(cmd)
	if err != nil {
		return err
	}

	if bytes.Compare(resp, []byte("OK\n")) == 0 {
		return nil
	}

	return &ParseError{Line: string(resp)}
}

func parseListNetworksResult(resp io.Reader) (res []ConfiguredNetwork, err error) {
	s := bufio.NewScanner(resp)
	if !s.Scan() {
		return nil, &ParseError{}
	}

	networkIDCol, ssidCol, bssidCol, flagsCol, maxCol := -1, -1, -1, -1, -1
	for n, col := range strings.Split(s.Text(), " / ") {
		switch col {
		case "network id":
			networkIDCol = n
		case "ssid":
			ssidCol = n
		case "bssid":
			bssidCol = n
		case "flags":
			flagsCol = n
		}

		maxCol = n
	}

	for s.Scan() {
		ln := s.Text()
		fields := strings.Split(ln, "\t")
		if len(fields) < maxCol {
			return nil, &ParseError{Line: ln}
		}

		var networkID string
		if networkIDCol != -1 {
			networkID = fields[networkIDCol]
		}

		var ssid string
		if ssidCol != -1 {
			ssid = fields[ssidCol]
		}

		var bssid string
		if bssidCol != -1 {
			bssid = fields[bssidCol]
		}

		var flags []string
		if flagsCol != -1 {
			if len(fields[flagsCol]) >= 2 && fields[flagsCol][0] == '[' && fields[flagsCol][len(fields[flagsCol])-1] == ']' {
				flags = strings.Split(fields[flagsCol][1:len(fields[flagsCol])-1], "][")
			}
		}

		res = append(res, &configuredNetwork{
			networkID: networkID,
			ssid:      ssid,
			bssid:     bssid,
			flags:     flags,
		})
	}

	return res, nil
}

func parseStatusResults(resp io.Reader) (StatusResult, error) {
	s := bufio.NewScanner(resp)

	res := &statusResult{}

	for s.Scan() {
		ln := s.Text()
		fields := strings.Split(ln, "=")
		if len(fields) != 2 {
			continue
		}

		switch fields[0] {
		case "wpa_state":
			res.wpaState = fields[1]
		case "key_mgmt":
			res.keyMgmt = fields[1]
		case "ip_address":
			res.ipAddr = fields[1]
		case "ssid":
			res.ssid = fields[1]
		case "address":
			res.address = fields[1]
		case "bssid":
			res.bssid = fields[1]
		case "freq":
			res.frequency = fields[1]
		case "id_str":
			res.idStr = fields[1]
		}
	}

	return res, nil
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
			ssid = decodeByteLiteralString(fields[ssidCol])
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

func decodeByteLiteralString(input string) string {
	result := []byte{}
	length := len(input)
	for idx := 0; idx < length; {
		if idx < length-2 && string(input[idx:idx+2]) == string("\\\\") {
			result = append(result, input[idx:idx+1]...)
			idx += 2
		} else if idx < length+3 && string(input[idx]) == string("\\") && string(input[idx+1]) == "x" {
			byts, _ := hex.DecodeString(input[idx+2 : idx+4])
			result = append(result, byts...)
			idx += 4
		} else {
			result = append(result, input[idx])
			idx += 1
		}
	}
	return string(result)
}
