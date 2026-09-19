package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
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

	live  *protocol.LiveData
	peers map[protocol.MAC]protocol.PeerPing
	// echo prints every message as it is received (watch mode).
	echo bool
}

// open connects to the Totem and sends the Ready handshake.
func open(ctx context.Context, g *globals) (*session, error) {
	c, err := g.connect(ctx, client.Options{HalfDuplex: g.halfDuplex, AutoGrant: true, Logger: g.log})
	if err != nil {
		return nil, err
	}
	g.phase("connected and subscribed")
	if err := c.Start(); err != nil {
		_ = c.Close()
		return nil, err
	}
	g.phase("handshake sent")
	s := &session{ctx: ctx, c: c, g: g, out: g.out, peers: map[protocol.MAC]protocol.PeerPing{}}
	if c.HalfDuplex() {
		// Ready handed the device the TX window; it sends what it has queued
		// and hands it back. Without that, every command would wait out its
		// full timeout for a window that never comes.
		if _, err := s.await(g.wait(15*time.Second), isHandoff); err != nil {
			s.close()
			if errors.Is(err, errTimeout) {
				return nil, errors.New("no TX handoff from the Totem within 15 s: --half-duplex needs firmware 5.x " +
					"(see `totemctl info`) and a Bluetooth stack that confirms indications, which macOS does not; " +
					"run without --half-duplex")
			}
			return nil, err
		}
	}
	return s, nil
}

func isHandoff(m protocol.Message) bool {
	h, ok := m.(protocol.Handoff)
	return ok && h.ToApp
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

// send writes frames within one TX window, waiting for it up to 30 s.
func (s *session) send(frames ...protocol.Frame) error {
	return s.sendBy(time.Now().Add(s.g.wait(30*time.Second)), frames...)
}

// sendBy is send for a caller with its own deadline. Waiting for the TX
// window past it (in half-duplex mode) would stretch the caller's timeout.
func (s *session) sendBy(deadline time.Time, frames ...protocol.Frame) error {
	ctx, cancel := context.WithDeadline(s.ctx, deadline)
	defer cancel()
	err := s.c.Send(ctx, frames...)
	if errors.Is(err, context.DeadlineExceeded) && s.ctx.Err() == nil {
		return fmt.Errorf("%w: %w", errTimeout, err)
	}
	return err
}

var errTimeout = errors.New("timed out waiting for the Totem")

// await consumes events, keeping s's state current, until match returns
// true or the timeout expires. A timeout of zero or less has already
// expired: callers compute timeouts from deadlines, and one that ran out
// must not turn into no limit at all (awaitCtx(s.ctx, ...) waits for good).
func (s *session) await(timeout time.Duration, match func(protocol.Message) bool) (protocol.Message, error) {
	if timeout <= 0 {
		return nil, errTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	return s.awaitCtx(ctx, match)
}

// awaitCtx is await bounded by ctx, which must derive from s.ctx.
func (s *session) awaitCtx(ctx context.Context, match func(protocol.Message) bool) (protocol.Message, error) {
	for {
		select {
		case ev := <-s.c.Events():
			if ev.Err != nil {
				s.g.log.Warn("bad frame", "channel", ev.Channel, "data", hex.EncodeToString(ev.Raw), "err", ev.Err)
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
			return nil, errors.New("the Totem disconnected")
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
	case protocol.LiveData:
		s.live = &v
	case protocol.PeerPing:
		s.peers[v.MAC] = v
	}
}

func is[T protocol.Message](m protocol.Message) bool { _, ok := m.(T); return ok }

// fetchStatic requests Static Data and waits until check accepts a record.
// Records sent before a change may still be queued, so it keeps reading and
// only asks again after a quiet spell (a setting can take a moment to apply).
func (s *session) fetchStatic(timeout time.Duration, check func(protocol.StaticData) bool) (protocol.StaticData, error) {
	accept := func(m protocol.Message) bool {
		d, ok := m.(protocol.StaticData)
		return ok && (check == nil || check(d))
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := s.sendBy(deadline, protocol.RequestStaticData()); err != nil {
			return protocol.StaticData{}, err
		}
		m, err := s.await(min(time.Until(deadline), s.g.wait(3*time.Second)), accept)
		if err == nil {
			return m.(protocol.StaticData), nil
		}
		if !errors.Is(err, errTimeout) || !time.Now().Before(deadline) {
			return protocol.StaticData{}, err
		}
	}
}

// errNoPeer is returned by fetchPeer when the Totem sent no ping for the MAC.
var errNoPeer = errors.New("no such peer")

// fetchPeer requests Peer Pings for mac until check accepts one (nil
// accepts any). Like fetchStatic, it keeps reading, since pings queued
// before a change may still arrive, and asks again after a quiet spell. If
// pings came but none passed, it returns the last one with errTimeout.
func (s *session) fetchPeer(mac protocol.MAC, timeout time.Duration, check func(protocol.PeerPing) bool) (protocol.PeerPing, error) {
	req, err := protocol.RequestPeerDetails(mac)
	if err != nil {
		return protocol.PeerPing{}, err
	}
	var last *protocol.PeerPing
	accept := func(m protocol.Message) bool {
		p, ok := m.(protocol.PeerPing)
		if !ok || p.MAC != mac {
			return false
		}
		last = &p
		return check == nil || check(p)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := s.sendBy(deadline, req); err != nil {
			if errors.Is(err, errTimeout) && last != nil {
				return *last, errTimeout
			}
			return protocol.PeerPing{}, err
		}
		m, err := s.await(min(time.Until(deadline), s.g.wait(3*time.Second)), accept)
		if err == nil {
			return m.(protocol.PeerPing), nil
		}
		if !errors.Is(err, errTimeout) {
			return protocol.PeerPing{}, err
		}
		if !time.Now().Before(deadline) {
			if last == nil {
				return protocol.PeerPing{}, fmt.Errorf("%w: %s is not on this Totem (see `totemctl peers`)", errNoPeer, mac)
			}
			return *last, errTimeout
		}
	}
}

// peerList returns the bonded peers (and POIs) the Totem lists.
func (s *session) peerList() ([]protocol.MAC, error) {
	// The device sends Peer Sync only once Static Data was delivered.
	if _, err := s.fetchStatic(s.g.wait(30*time.Second), nil); err != nil {
		return nil, err
	}
	if err := s.send(protocol.RequestPeerSync()); err != nil {
		return nil, err
	}
	m, err := s.await(s.g.wait(30*time.Second), is[protocol.PeerSync])
	if err != nil {
		return nil, fmt.Errorf("no peer list from the Totem: %w", err)
	}
	return m.(protocol.PeerSync).Peers, nil
}

// refuseBond fails if mac is a bonded Totem on this device (rather than a
// point of interest or nothing): a POI written under its id replaces the
// bond, which only re-bonding the Totems side by side restores.
func (s *session) refuseBond(mac protocol.MAC) error {
	macs, err := s.peerList()
	if err != nil {
		return err
	}
	if !slices.Contains(macs, mac) {
		return nil
	}
	p, err := s.fetchPeer(mac, s.g.wait(30*time.Second), nil)
	if err != nil {
		return err
	}
	if !p.POI {
		return fmt.Errorf("%s is %q, a bonded Totem: a POI with this id would replace the bond", mac, p.Name)
	}
	return nil
}

// settle waits until the device has had a chance to act on what was just
// sent: its next TX handoff in half-duplex mode, or its next Live Data
// record (one pass of the send loop) in legacy mode.
func (s *session) settle() error {
	if !s.c.HalfDuplex() {
		_, err := s.await(s.g.wait(10*time.Second), is[protocol.LiveData])
		if errors.Is(err, errTimeout) {
			return nil // commands are processed independently of the send loop
		}
		return err
	}
	gen := s.c.Handoffs()
	_, err := s.await(s.g.wait(20*time.Second), func(m protocol.Message) bool {
		return is[protocol.Handoff](m) && s.c.Handoffs() > gen
	})
	return err
}
