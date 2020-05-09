// Copyright 2020 Matt Layher
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

package crhttp

import (
	"fmt"

	"github.com/mdlayher/ndp"
)

// An interfacesBody is the top-level structure returned by the debug API's
// interfaces route.
type interfacesBody struct {
	Interfaces []interfaceBody `json:"interfaces"`
}

// An interfaceBody represents an individual advertising interface.
type interfaceBody struct {
	Interface   string `json:"interface"`
	Advertising bool   `json:"advertise"`

	// Nil if Advertising is false.
	Advertisement *routerAdvertisement `json:"advertisement"`
}

// A routerAdvertisement represents an unpacked NDP router advertisement.
type routerAdvertisement struct {
	CurrentHopLimit             int     `json:"current_hop_limit"`
	ManagedConfiguration        bool    `json:"managed_configuration"`
	OtherConfiguration          bool    `json:"other_configuration"`
	MobileIPv6HomeAgent         bool    `json:"mobile_ipv6_home_agent"`
	RouterSelectionPreference   string  `json:"router_selection_preference"`
	NeighborDiscoveryProxy      bool    `json:"neighbor_discovery_proxy"`
	RouterLifetimeSeconds       int     `json:"router_lifetime_seconds"`
	ReachableTimeMilliseconds   int     `json:"reachable_time_milliseconds"`
	RetransmitTimerMilliseconds int     `json:"retransmit_timer_milliseconds"`
	Options                     options `json:"options"`
}

// packRA packs the data from an RA into a routerAdvertisement structure.
func packRA(ra *ndp.RouterAdvertisement) *routerAdvertisement {
	return &routerAdvertisement{
		CurrentHopLimit:             int(ra.CurrentHopLimit),
		ManagedConfiguration:        ra.ManagedConfiguration,
		OtherConfiguration:          ra.OtherConfiguration,
		MobileIPv6HomeAgent:         ra.MobileIPv6HomeAgent,
		RouterSelectionPreference:   preference(ra.RouterSelectionPreference),
		NeighborDiscoveryProxy:      ra.NeighborDiscoveryProxy,
		RouterLifetimeSeconds:       int(ra.RouterLifetime.Seconds()),
		ReachableTimeMilliseconds:   int(ra.ReachableTime.Milliseconds()),
		RetransmitTimerMilliseconds: int(ra.RetransmitTimer.Milliseconds()),
		Options:                     packOptions(ra.Options),
	}
}

// preference returns a stringified preference value for p.
func preference(p ndp.Preference) string {
	switch p {
	case ndp.Low:
		return "low"
	case ndp.Medium:
		return "medium"
	case ndp.High:
		return "high"
	default:
		panic(fmt.Sprintf("crhttp: invalid ndp.Preference %q", p.String()))
	}
}

// options represents the options unpacked from an NDP router advertisement.
type options struct {
	MTU                    int
	SourceLinkLayerAddress string
}

// packOptions unpacks individual NDP options to produce an options structure.
func packOptions(opts []ndp.Option) options {
	var out options
	for _, o := range opts {
		switch o := o.(type) {
		case *ndp.LinkLayerAddress:
			out.SourceLinkLayerAddress = o.Addr.String()
		case *ndp.MTU:
			out.MTU = int(*o)
		}
	}

	return out
}