// Copyright 2019 Matt Layher
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package corerad

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"time"

	"github.com/mdlayher/corerad/internal/config"
	"github.com/mdlayher/corerad/internal/system"
	"github.com/mdlayher/ndp"
	"github.com/mdlayher/schedgroup"
	"golang.org/x/net/ipv6"
	"golang.org/x/sync/errgroup"
	"inet.af/netaddr"
)

// An Advertiser sends NDP router advertisements.
type Advertiser struct {
	// Static configuration.
	iface string
	cfg   config.Interface
	ll    *log.Logger
	mm    *Metrics

	// Socket creation and system state manipulation.
	dialer *system.Dialer
	state  system.State

	// Notifications of state change for tests.
	eventC chan Event
}

// An Event indicates events of interest produced by an Advertiser.
type Event int

// Possible Event types.
const (
	ReceiveRA Event = iota
	InconsistentRA
)

// NewAdvertiser creates an Advertiser for the specified interface. If ll is
// nil, logs are discarded. If mm is nil, metrics are discarded.
func NewAdvertiser(iface string, cfg config.Interface, ll *log.Logger, mm *Metrics) *Advertiser {
	if ll == nil {
		ll = log.New(ioutil.Discard, "", 0)
	}
	if mm == nil {
		mm = NewMetrics(nil)
	}

	return &Advertiser{
		iface: iface,
		cfg:   cfg,
		ll:    ll,
		mm:    mm,

		// By default, directly manipulate the system.
		dialer: system.NewDialer(ll, iface),
		state:  system.NewState(),

		eventC: make(chan Event),
	}
}

// Events returns a channel of Events from the Advertiser. Events must be called
// before calling Advertise. The channel will be closed when Advertise returns.
func (a *Advertiser) Events() <-chan Event { return a.eventC }

// Advertise initializes the configured interface and begins router solicitation
// and advertisement handling. Advertise will block until ctx is canceled or an
// error occurs.
//
// Before calling Advertise, call Events and ensure that the returned channel is
// being drained, or Advertiser will stop processing.
func (a *Advertiser) Advertise(ctx context.Context) error {
	// No more events when Advertise returns.
	defer close(a.eventC)

	return a.dialer.Dial(ctx, func(ctx context.Context, dctx *system.DialContext) error {
		// We can now initialize any plugins that rely on dynamic information
		// about the network interface.
		for _, p := range a.cfg.Plugins {
			if err := p.Prepare(dctx.Interface); err != nil {
				return fmt.Errorf("failed to prepare plugin %q: %v", p.Name(), err)
			}

			a.logf("%q: %s", p.Name(), p)
		}

		// Before starting any other goroutines, verify that the interface can
		// actually be used to send an initial router advertisement, avoiding a
		// needless start/error/restart loop.
		//
		// TODO: don't do this for unicast-only mode.
		if err := a.send(dctx.Conn, netaddr.IPv6LinkLocalAllNodes(), a.cfg); err != nil {
			return fmt.Errorf("failed to send initial multicast router advertisement: %v", err)
		}

		// Note unicast-only mode in logs.
		var method string
		if a.cfg.UnicastOnly {
			method = "unicast-only "
		}

		a.logf("initialized, advertising %sfrom %s", method, dctx.IP)

		// Advertise until an error occurs, reinitializing under certain
		// circumstances.
		err := a.advertise(ctx, dctx.Conn)
		switch {
		case errors.Is(err, context.Canceled):
			// Intentional shutdown.
			return a.shutdown(dctx.Conn)
		case err == nil:
			panic("corerad: advertise must never return nil error")
		default:
			return err
		}
	})
}

// advertise is the internal loop for Advertise which coordinates the various
// Advertiser goroutines.
func (a *Advertiser) advertise(ctx context.Context, conn system.Conn) error {
	// Attach the context to the errgroup so that goroutines are canceled when
	// one of them returns an error.
	eg, ctx := errgroup.WithContext(ctx)

	ipC := make(chan netaddr.IP, 16)

	// RA scheduler which consumes requests to send RAs and dispatches them
	// at the appropriate times.
	eg.Go(func() error {
		if err := a.schedule(ctx, conn, ipC); err != nil {
			return fmt.Errorf("failed to schedule router advertisements: %w", err)
		}

		return nil
	})

	// Multicast RA generator, unless running in unicast-only mode.
	if !a.cfg.UnicastOnly {
		eg.Go(func() error {
			a.multicast(ctx, ipC)
			return nil
		})
	}

	// Listener which issues RAs in response to RS messages.
	eg.Go(func() error {
		if err := a.listen(ctx, conn, ipC); err != nil {
			return fmt.Errorf("failed to listen: %w", err)
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("failed to run advertiser: %w", err)
	}

	// Should only reach this state when context is canceled on shutdown.
	return ctx.Err()
}

// Constants taken from https://tools.ietf.org/html/rfc4861#section-10.
const (
	maxInitialAdvInterval = 16 * time.Second
	maxInitialAdv         = 3
	minDelayBetweenRAs    = 3 * time.Second
	maxRADelay            = 500 * time.Millisecond
)

// multicast runs a multicast advertising loop until ctx is canceled.
func (a *Advertiser) multicast(ctx context.Context, ipC chan<- netaddr.IP) {
	// Initialize PRNG so we can add jitter to our unsolicited multicast RA
	// delay times.
	var (
		prng = rand.New(rand.NewSource(time.Now().UnixNano()))
		min  = a.cfg.MinInterval.Nanoseconds()
		max  = a.cfg.MaxInterval.Nanoseconds()
	)

	for i := 0; ; i++ {
		// Enable cancelation before sending any messages, if necessary.
		select {
		case <-ctx.Done():
			return
		default:
		}

		ipC <- netaddr.IPv6LinkLocalAllNodes()

		select {
		case <-ctx.Done():
			return
		case <-time.After(multicastDelay(prng, i, min, max)):
		}
	}
}

// deadlineNow causes connection deadlines to trigger immediately.
var deadlineNow = time.Unix(1, 0)

// listen issues unicast router advertisements in response to router
// solicitations, until ctx is canceled.
func (a *Advertiser) listen(ctx context.Context, conn system.Conn, ipC chan<- netaddr.IP) error {
	// Wait for cancelation and then force any pending reads to time out.
	var eg errgroup.Group
	eg.Go(func() error {
		<-ctx.Done()

		if err := conn.SetReadDeadline(deadlineNow); err != nil {
			return fmt.Errorf("failed to interrupt listener: %w", err)
		}

		return nil
	})

	for {
		// Enable cancelation before sending any messages, if necessary.
		if ctx.Err() != nil {
			return eg.Wait()
		}

		m, cm, host, err := conn.ReadFrom()
		if err != nil {
			if ctx.Err() != nil {
				// Context canceled.
				return eg.Wait()
			}

			a.mm.ErrorsTotal.WithLabelValues(a.cfg.Name, "receive").Inc()

			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				// Temporary error or timeout, just continue.
				// TODO: smarter backoff/retry.
				time.Sleep(50 * time.Millisecond)
				continue
			}

			return fmt.Errorf("failed to read NDP messages: %w", err)
		}

		// Handle the incoming message and send a response if one is needed.
		hostAddr, err := netaddr.ParseIP(host.String())
		if err != nil {
			return fmt.Errorf("failed to parse IP address: %w", err)
		}

		ip, err := a.handle(m, cm, hostAddr)
		if err != nil {
			return fmt.Errorf("failed to handle NDP message: %w", err)
		}
		if ip != nil {
			ipC <- *ip
		}
	}
}

// handle handles an incoming NDP message from a remote host.
func (a *Advertiser) handle(m ndp.Message, cm *ipv6.ControlMessage, host netaddr.IP) (*netaddr.IP, error) {
	a.mm.MessagesReceivedTotal.WithLabelValues(a.cfg.Name, m.Type().String()).Add(1)

	// Ensure this message has a valid hop limit.
	if cm.HopLimit != ndp.HopLimit {
		a.logf("received NDP message with IPv6 hop limit %d from %s, ignoring", cm.HopLimit, host)
		a.mm.MessagesReceivedInvalidTotal.WithLabelValues(a.cfg.Name, m.Type().String()).Add(1)
		return nil, nil
	}

	switch m := m.(type) {
	case *ndp.RouterSolicitation:
		// Issue a unicast RA for clients with valid addresses, or a multicast
		// RA for any client contacting us via the IPv6 unspecified address,
		// per https://tools.ietf.org/html/rfc4861#section-6.2.6.
		if host == netaddr.IPv6Unspecified() {
			host = netaddr.IPv6LinkLocalAllNodes()
		}

		// TODO: consider checking for numerous RS in succession and issuing
		// a multicast RA in response.
		return &host, nil
	case *ndp.RouterAdvertisement:
		// Received a router advertisement from a different router on this
		// LAN, verify its consistency with our own.
		a.eventC <- ReceiveRA
		want, err := a.buildRA(a.cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build router advertisement: %w", err)
		}

		// Ensure the RAs are consistent.
		if !verifyRAs(want, m) {
			// RAs are not consistent, report this per the RFC.
			a.eventC <- InconsistentRA
			a.logf("inconsistencies detected in router advertisement from router with IP %q, source link-layer address %q",
				host, sourceLLA(m.Options))
			a.mm.RouterAdvertisementInconsistenciesTotal.WithLabelValues(a.cfg.Name).Add(1)
		}
	default:
		a.logf("received NDP message of type %T from %s, ignoring", m, host)
		a.mm.MessagesReceivedInvalidTotal.WithLabelValues(a.cfg.Name, m.Type().String()).Add(1)
	}

	// No response necessary.
	return nil, nil
}

// schedule consumes RA requests and schedules them with workers so they may
// occur at the appropriate times.
func (a *Advertiser) schedule(ctx context.Context, conn system.Conn, ipC <-chan netaddr.IP) error {
	// Enable canceling schedule's context on send RA error.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		// Schedule router advertisements and handles any errors from those
		// advertisements. Note that sg.Wait cannot be used for that purpose
		// because it is invalid to call Wait and schedule other work after,
		// meaning that we can only Wait once we're exiting this function.
		sg   = schedgroup.New(ctx)
		errC = make(chan error)

		// Jitter for RA delays.
		prng = rand.New(rand.NewSource(time.Now().UnixNano()))

		// Assume that a.init sent the initial RA recently and space out others
		// accordingly.
		lastMulticast = time.Now()
	)

	for {
		// New IP for each loop iteration to prevent races.
		var ip netaddr.IP

		select {
		case err := <-errC:
			// We received an error and will need to determine if we can
			// reinitialize the listener. Don't schedule any more tasks and
			// return the error immediately.
			cancel()
			_ = sg.Wait()
			return err
		case <-ctx.Done():
			// Context cancelation is expected.
			if err := sg.Wait(); err != nil && err != context.Canceled {
				return err
			}

			return nil
		case ip = <-ipC:
		}

		if !ip.IsMulticast() {
			// This is a unicast RA. Delay it for a short period of time per
			// the RFC and then send it.
			delay := time.Duration(prng.Int63n(maxRADelay.Nanoseconds())) * time.Nanosecond
			sg.Delay(delay, func() error {
				if err := a.sendWorker(conn, ip); err != nil {
					errC <- err
				}
				return nil
			})
			continue
		}

		// Ensure that we space out multicast RAs as required by the RFC.
		var delay time.Duration
		if time.Since(lastMulticast) < minDelayBetweenRAs {
			delay = minDelayBetweenRAs
		}

		// Ready to send this multicast RA.
		lastMulticast = time.Now()
		sg.Delay(delay, func() error {
			if err := a.sendWorker(conn, ip); err != nil {
				errC <- err
			}
			return nil
		})
	}
}

// sendWorker is a goroutine worker which sends a router advertisement to ip.
func (a *Advertiser) sendWorker(conn system.Conn, ip netaddr.IP) error {
	if err := a.send(conn, ip, a.cfg); err != nil {
		a.logf("failed to send scheduled router advertisement to %s: %v", ip, err)
		a.mm.ErrorsTotal.WithLabelValues(a.cfg.Name, "transmit").Inc()
		return err
	}

	typ := "unicast"
	if ip.IsMulticast() {
		typ = "multicast"
		a.mm.LastMulticastTime.WithLabelValues(a.cfg.Name).SetToCurrentTime()
	}

	a.mm.RouterAdvertisementsTotal.WithLabelValues(a.cfg.Name, typ).Add(1)
	return nil
}

// send sends a single router advertisement built from cfg to the destination IP
// address, which may be a unicast or multicast address.
func (a *Advertiser) send(conn system.Conn, dst netaddr.IP, cfg config.Interface) error {
	if cfg.UnicastOnly && dst.IsMulticast() {
		// Nothing to do.
		return nil
	}

	// Build a router advertisement from configuration and always append
	// the source address option.
	ra, err := a.buildRA(cfg)
	if err != nil {
		return fmt.Errorf("failed to build router advertisement: %w", err)
	}

	if err := conn.WriteTo(ra, nil, dst.IPAddr().IP); err != nil {
		return fmt.Errorf("failed to send router advertisement to %s: %w", dst, err)
	}

	return nil
}

// buildRA builds a router advertisement from configuration and updates any
// necessary metrics.
func (a *Advertiser) buildRA(ifi config.Interface) (*ndp.RouterAdvertisement, error) {
	// Check for any system state changes which could impact the router
	// advertisement, and then build it using an interface configuration.
	forwarding, err := a.state.IPv6Forwarding(ifi.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPv6 forwarding state: %w", err)
	}

	ra, err := ifi.RouterAdvertisement(forwarding)
	if err != nil {
		return nil, fmt.Errorf("failed to generate router advertisement: %v", err)
	}

	// Finally, update Prometheus metrics to provide a consistent view of the
	// router advertisement state.

	for _, o := range ra.Options {
		// TODO(mdlayher): consider extracting this logic if it grows unwieldy.
		switch o := o.(type) {
		case *ndp.PrefixInformation:
			// Combine the prefix and prefix length fields into a proper CIDR
			// subnet for the label.
			pfx := &net.IPNet{
				IP:   o.Prefix,
				Mask: net.CIDRMask(int(o.PrefixLength), 128),
			}

			a.mm.updateGauge(
				a.mm.RouterAdvertisementPrefixAutonomous,
				[]string{a.cfg.Name, pfx.String()},
				boolFloat(o.AutonomousAddressConfiguration),
			)
		}
	}

	return ra, nil
}

// shutdown indicates to hosts that this host is no longer a router.
func (a *Advertiser) shutdown(conn system.Conn) error {
	// In general, many of these actions are best-effort and should not halt
	// shutdown on failure.

	// Send a final router advertisement (TODO: more than one) with a router
	// lifetime of 0 to indicate that hosts should not use this router as a
	// default router.
	//
	// a.cfg is copied in case any delayed send workers are outstanding and
	// the server's context is canceled.
	cfg := a.cfg
	cfg.DefaultLifetime = 0

	if err := a.send(conn, netaddr.IPv6LinkLocalAllNodes(), cfg); err != nil {
		a.logf("failed to send final multicast router advertisement: %v", err)
	}

	return nil
}

// logf prints a formatted log with the Advertiser's interface name.
func (a *Advertiser) logf(format string, v ...interface{}) {
	a.ll.Println(a.iface + ": " + fmt.Sprintf(format, v...))
}

// multicastDelay selects an appropriate delay duration for unsolicited
// multicast RA sending.
func multicastDelay(r *rand.Rand, i int, min, max int64) time.Duration {
	// Implements the algorithm described in:
	// https://tools.ietf.org/html/rfc4861#section-6.2.4.

	var d time.Duration
	if min == max {
		// Identical min/max, use a static interval.
		d = (time.Duration(max) * time.Nanosecond).Round(time.Second)
	} else {
		// min <= wait <= max, rounded to 1 second granularity.
		d = (time.Duration(min+rand.Int63n(max-min)) * time.Nanosecond).Round(time.Second)
	}

	// For first few advertisements, select a shorter wait time so routers
	// can be discovered quickly, per the RFC.
	if i < maxInitialAdv && d > maxInitialAdvInterval {
		d = maxInitialAdvInterval
	}

	return d
}
