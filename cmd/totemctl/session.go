package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ljagiello/totem-compass/client"
	"github.com/ljagiello/totem-compass/protocol"
)

// session is a connected, started Totem plus the latest state it reported.
type session struct {
	ctx context.Context
	c   *client.Client
	g   *globals
	out *printer

	static *protocol.StaticData
	live   *protocol.LiveData
	peers  map[protocol.MAC]protocol.PeerPing
	// echo prints every message as it is received (watch mode).
	echo bool
}

func open(ctx context.Context, g *globals) (*session, error) {
	scanCtx, cancel := context.WithTimeout(ctx, g.scanTimeout)
	defer cancel()
	g.out.status("scanning for %s…", describeMatch(g.device))
	dev, err := client.Find(scanCtx, g.device)
	if err != nil {
		return nil, err
	}
	g.phase("found")
	g.out.status("connecting to %s (%s, %d dBm)…", orUnnamed(dev.Name), dev.Address, dev.RSSI)
	opts := client.Options{HalfDuplex: g.halfDuplex, AutoGrant: true}
	if g.trace {
		opts.Trace = func(dir string, ch protocol.Channel, b []byte) {
			fmt.Fprintf(os.Stderr, "%s %s %-11s % x\n", time.Now().Format("15:04:05.000"), dir, ch, b)
		}
	}
	c, err := client.Connect(ctx, dev, opts)
	if err != nil {
		return nil, err
	}
	g.phase("connected and subscribed")
	if err := c.Start(); err != nil {
		_ = c.Close()
		return nil, err
	}
	g.phase("handshake sent")
	return &session{ctx: ctx, c: c, g: g, out: g.out, peers: map[protocol.MAC]protocol.PeerPing{}}, nil
}

func describeMatch(m string) string {
	if m == "" {
		return "a Totem"
	}
	return fmt.Sprintf("a Totem matching %q", m)
}

func orUnnamed(s string) string {
	if s == "" {
		return "(unnamed)"
	}
	return s
}

func (s *session) close() {
	s.g.phase("done")
	_ = s.c.Close()
	s.g.phase("disconnected")
}

func (s *session) send(frames ...protocol.Frame) error {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return s.c.Send(ctx, frames...)
}

var errTimeout = errors.New("timed out waiting for the Totem")

// await consumes events, keeping s's state current, until match returns
// true or the timeout expires.
func (s *session) await(timeout time.Duration, match func(protocol.Message) bool) (protocol.Message, error) {
	ctx := s.ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		select {
		case ev := <-s.c.Events():
			if ev.Err != nil {
				s.out.warn("bad frame on %s (% x): %v", ev.Channel, ev.Raw, ev.Err)
				continue
			}
			s.track(ev.Msg)
			if s.echo {
				s.out.message(ev.Msg)
			}
			if d, ok := ev.Msg.(protocol.DisconnectIntent); ok && !s.echo {
				s.out.status("device is disconnecting (%v)", d)
			}
			if match != nil && match(ev.Msg) {
				return ev.Msg, nil
			}
		case <-s.c.Done():
			return nil, errors.New("Totem disconnected")
		case <-ctx.Done():
			if s.ctx.Err() != nil {
				return nil, s.ctx.Err()
			}
			return nil, errTimeout
		}
	}
}

func (s *session) track(m protocol.Message) {
	switch v := m.(type) {
	case protocol.StaticData:
		s.static = &v
	case protocol.LiveData:
		s.live = &v
	case protocol.PeerPing:
		s.peers[v.MAC] = v
	}
}

func is[T protocol.Message](m protocol.Message) bool { _, ok := m.(T); return ok }

// fetchStatic requests Static Data and waits until check accepts it,
// re-requesting in each TX window (settings take a few seconds to apply).
func (s *session) fetchStatic(timeout time.Duration, check func(protocol.StaticData) bool) (protocol.StaticData, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := s.send(protocol.RequestStaticData()); err != nil {
			return protocol.StaticData{}, err
		}
		m, err := s.await(time.Until(deadline), is[protocol.StaticData])
		if err != nil {
			return protocol.StaticData{}, err
		}
		if d := m.(protocol.StaticData); check == nil || check(d) {
			return d, nil
		}
	}
	return protocol.StaticData{}, errTimeout
}

// fetchPeer requests a Peer Ping for mac and waits for it.
func (s *session) fetchPeer(mac protocol.MAC, timeout time.Duration) (protocol.PeerPing, error) {
	req, err := protocol.RequestPeerDetails(mac)
	if err != nil {
		return protocol.PeerPing{}, err
	}
	if err := s.send(req); err != nil {
		return protocol.PeerPing{}, err
	}
	m, err := s.await(timeout, func(m protocol.Message) bool {
		p, ok := m.(protocol.PeerPing)
		return ok && p.MAC == mac
	})
	if errors.Is(err, errTimeout) {
		return protocol.PeerPing{}, fmt.Errorf("no peer %s on this Totem (see `totemctl peers`)", mac)
	}
	if err != nil {
		return protocol.PeerPing{}, err
	}
	return m.(protocol.PeerPing), nil
}

// settle waits until the device has had a chance to act on what was just
// sent: its next TX handoff in half-duplex mode, or its next Live Data
// record (one pass of the send loop) in legacy mode.
func (s *session) settle() error {
	if !s.c.HalfDuplex() {
		_, err := s.await(10*time.Second, is[protocol.LiveData])
		if errors.Is(err, errTimeout) {
			return nil // commands are processed independently of the send loop
		}
		return err
	}
	gen := s.c.Handoffs()
	_, err := s.await(20*time.Second, func(m protocol.Message) bool {
		return is[protocol.Handoff](m) && s.c.Handoffs() > gen
	})
	return err
}
