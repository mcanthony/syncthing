// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package beacon

import (
	"fmt"
	"net"
	"time"

	"github.com/thejerf/suture"
)

type Broadcast struct {
	*suture.Supervisor
	port   int
	inbox  chan []byte
	outbox chan recv
	br     *broadcastReader
	bw     *broadcastWriter
}

func NewBroadcast(port int) *Broadcast {
	b := &Broadcast{
		Supervisor: suture.New("broadcastBeacon", suture.Spec{
			// Don't retry too frenetically: an error to open a socket or
			// whatever is usually something that is either permanent or takes
			// a while to get solved...
			FailureThreshold: 2,
			FailureBackoff:   60 * time.Second,
			// Only log restarts in debug mode.
			Log: func(line string) {
				if debug {
					l.Debugln(line)
				}
			},
		}),
		port:   port,
		inbox:  make(chan []byte),
		outbox: make(chan recv, 16),
	}

	b.br = &broadcastReader{
		port:   port,
		outbox: b.outbox,
	}
	b.Add(b.br)
	b.bw = &broadcastWriter{
		port:  port,
		inbox: b.inbox,
	}
	b.Add(b.bw)

	return b
}

func (b *Broadcast) Send(data []byte) {
	b.inbox <- data
}

func (b *Broadcast) Recv() ([]byte, net.Addr) {
	recv := <-b.outbox
	return recv.data, recv.src
}

func (b *Broadcast) Error() error {
	if err := b.br.Error(); err != nil {
		return err
	}
	return b.bw.Error()
}

type broadcastWriter struct {
	port  int
	inbox chan []byte
	conn  *net.UDPConn
	errorHolder
}

func (w *broadcastWriter) Serve() {
	if debug {
		l.Debugln(w, "starting")
		defer l.Debugln(w, "stopping")
	}

	var err error
	w.conn, err = net.ListenUDP("udp4", nil)
	if err != nil {
		if debug {
			l.Debugln(err)
		}
		w.setError(err)
		return
	}
	defer w.conn.Close()

	for bs := range w.inbox {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			if debug {
				l.Debugln(err)
			}
			w.setError(err)
			continue
		}

		var dsts []net.IP
		for _, addr := range addrs {
			if iaddr, ok := addr.(*net.IPNet); ok && len(iaddr.IP) >= 4 && iaddr.IP.IsGlobalUnicast() && iaddr.IP.To4() != nil {
				baddr := bcast(iaddr)
				dsts = append(dsts, baddr.IP)
			}
		}

		if len(dsts) == 0 {
			// Fall back to the general IPv4 broadcast address
			dsts = append(dsts, net.IP{0xff, 0xff, 0xff, 0xff})
		}

		if debug {
			l.Debugln("addresses:", dsts)
		}

		success := 0
		for _, ip := range dsts {
			dst := &net.UDPAddr{IP: ip, Port: w.port}

			w.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, err := w.conn.WriteTo(bs, dst)
			w.conn.SetWriteDeadline(time.Time{})

			if err, ok := err.(net.Error); ok && err.Timeout() {
				// Write timeouts should not happen. We treat it as a fatal
				// error on the socket.
				if debug {
					l.Debugln(err)
				}
				w.setError(err)
				return
			}

			if err, ok := err.(net.Error); ok && err.Temporary() {
				// A transient error. Lets hope for better luck in the future.
				if debug {
					l.Debugln(err)
				}
				continue
			}

			if err != nil {
				// Some other error that we don't expect. Bail and retry.
				if debug {
					l.Debugln(err)
				}
				w.setError(err)
				return
			}

			if debug {
				l.Debugf("sent %d bytes to %s", len(bs), dst)
			}

			success++
		}

		if success > 0 {
			w.setError(nil)
		}
	}
}

func (w *broadcastWriter) Stop() {
	w.conn.Close()
}

func (w *broadcastWriter) String() string {
	return fmt.Sprintf("broadcastWriter@%p", w)
}

type broadcastReader struct {
	port   int
	outbox chan recv
	conn   *net.UDPConn
	errorHolder
}

func (r *broadcastReader) Serve() {
	if debug {
		l.Debugln(r, "starting")
		defer l.Debugln(r, "stopping")
	}

	var err error
	r.conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: r.port})
	if err != nil {
		if debug {
			l.Debugln(err)
		}
		r.setError(err)
		return
	}
	defer r.conn.Close()

	bs := make([]byte, 65536)
	for {
		n, addr, err := r.conn.ReadFrom(bs)
		if err != nil {
			if debug {
				l.Debugln(err)
			}
			r.setError(err)
			return
		}

		r.setError(nil)

		if debug {
			l.Debugf("recv %d bytes from %s", n, addr)
		}

		c := make([]byte, n)
		copy(c, bs)
		select {
		case r.outbox <- recv{c, addr}:
		default:
			if debug {
				l.Debugln("dropping message")
			}
		}
	}

}

func (r *broadcastReader) Stop() {
	r.conn.Close()
}

func (r *broadcastReader) String() string {
	return fmt.Sprintf("broadcastReader@%p", r)
}

func bcast(ip *net.IPNet) *net.IPNet {
	var bc = &net.IPNet{}
	bc.IP = make([]byte, len(ip.IP))
	copy(bc.IP, ip.IP)
	bc.Mask = ip.Mask

	offset := len(bc.IP) - len(bc.Mask)
	for i := range bc.IP {
		if i-offset >= 0 {
			bc.IP[i] = ip.IP[i] | ^ip.Mask[i-offset]
		}
	}
	return bc
}
