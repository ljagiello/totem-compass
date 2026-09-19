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
	// sentAt is when the last send completed, eventAt when the event being
	// matched arrived: settle tells fresh records from queued ones by them.
	sentAt, eventAt time.Time
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
	if err == nil {
		s.sentAt = time.Now()
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
			s.eventAt = ev.At
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

// fetch sends req and reads records until accept takes one, asking again
// after a quiet spell, until the timeout: records sent before a change may
// still be queued, and a change can take a moment to apply. It returns the
// accepted record, or an error wrapping errTimeout.
func fetch[T protocol.Message](s *session, req protocol.Frame, timeout time.Duration, accept func(T) bool) (T, error) {
	var zero T
	match := func(m protocol.Message) bool {
		v, ok := m.(T)
		return ok && accept(v)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := s.sendBy(deadline, req); err != nil {
			return zero, err
		}
		m, err := s.await(min(time.Until(deadline), s.g.wait(3*time.Second)), match)
		if err == nil {
			return m.(T), nil
		}
		if !errors.Is(err, errTimeout) || !time.Now().Before(deadline) {
			return zero, err
		}
	}
}

// fetchStatic requests Static Data until check accepts a record (nil
// accepts any).
func (s *session) fetchStatic(timeout time.Duration, check func(protocol.StaticData) bool) (protocol.StaticData, error) {
	return fetch(s, protocol.RequestStaticData(), timeout, func(d protocol.StaticData) bool {
		return check == nil || check(d)
	})
}

var (
	// errNoPeer: the Totem does not list the MAC.
	errNoPeer = errors.New("no such peer")
	// errUnconfirmed: the Totem kept reporting a peer, but not as asked.
	errUnconfirmed = errors.New("change not confirmed")
)

// fetchPeer requests Peer Pings for mac until check accepts one (nil
// accepts any). If pings came but none passed check, it returns the last
// one with errUnconfirmed; if none came, an error wrapping errTimeout.
func (s *session) fetchPeer(mac protocol.MAC, timeout time.Duration, check func(protocol.PeerPing) bool) (protocol.PeerPing, error) {
	if p, ok := s.peers[mac]; ok && check == nil {
		// Already sent this session, e.g. after the peer list (whose ack
		// asks for every peer's ping): no need to ask again.
		return p, nil
	}
	req, err := protocol.RequestPeerDetails(mac)
	if err != nil {
		return protocol.PeerPing{}, err
	}
	var last *protocol.PeerPing
	p, err := fetch(s, req, timeout, func(p protocol.PeerPing) bool {
		if p.MAC != mac {
			return false
		}
		last = &p
		return check == nil || check(p)
	})
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, errTimeout) && last != nil:
		return *last, errUnconfirmed
	case errors.Is(err, errTimeout):
		return protocol.PeerPing{}, fmt.Errorf("no Peer Ping from the Totem for %s: %w", mac, err)
	}
	return protocol.PeerPing{}, err
}

// requirePeer fails fast if the Totem does not list mac, instead of
// waiting out a fetchPeer timeout for a mistyped MAC.
func (s *session) requirePeer(mac protocol.MAC) error {
	macs, err := s.peerList()
	if err != nil {
		return err
	}
	if !slices.Contains(macs, mac) {
		return fmt.Errorf("%w: %s is not on this Totem (see `totemctl peers`)", errNoPeer, mac)
	}
	return nil
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
// record (one pass of the send loop) in legacy mode. A record received
// before the send, still queued, does not count.
//
// The frame was written with response, so the device has it either way; a
// timeout here is not a failure (in either mode), and changes that can be
// checked are confirmed by their caller.
func (s *session) settle() error {
	var err error
	if s.c.HalfDuplex() {
		gen := s.c.Handoffs()
		_, err = s.await(s.g.wait(20*time.Second), func(m protocol.Message) bool {
			return is[protocol.Handoff](m) && s.c.Handoffs() > gen
		})
	} else {
		_, err = s.await(s.g.wait(10*time.Second), func(m protocol.Message) bool {
			return is[protocol.LiveData](m) && s.eventAt.After(s.sentAt)
		})
	}
	if errors.Is(err, errTimeout) {
		return nil
	}
	return err
}

// confirmGone waits until the Totem no longer lists mac.
func (s *session) confirmGone(mac protocol.MAC) error {
	_, err := fetch(s, protocol.RequestPeerSync(), s.g.wait(30*time.Second), func(ps protocol.PeerSync) bool {
		return !slices.Contains(ps.Peers, mac)
	})
	if errors.Is(err, errTimeout) {
		return fmt.Errorf("sent, but the Totem still lists %s: %w", mac, err)
	}
	return err
}
