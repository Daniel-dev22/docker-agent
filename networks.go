package main

import (
	"context"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/network"
)

// ---------------------------------------------------------------------------
// Network discovery. Published as part of the discovery snapshot: ALL
// user-defined docker networks, PLUS the network attachments (with IP + gateway)
// of the host's reverse-proxy container, which a controller needs to manage
// proxy entrypoints. Both are cheap, fixed-cost calls — see discovery.go.
// ---------------------------------------------------------------------------

// NetworkStatus is one docker network row. Optional fields are omitted when empty.
type NetworkStatus struct {
	Name        string `json:"name"`
	Driver      string `json:"driver"`
	Scope       string `json:"scope"`
	ID          string `json:"id"`
	Subnet      string `json:"subnet,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Parent      string `json:"parent,omitempty"`
	VlanTag     string `json:"vlan_tag,omitempty"`
	IpvlanMode  string `json:"ipvlan_mode,omitempty"`
	MacvlanMode string `json:"macvlan_mode,omitempty"`
	Internal    bool   `json:"internal,omitempty"`
	Attachable  bool   `json:"attachable,omitempty"`
}

// ContainerNetAttachment is one network a container is attached to, with the
// container's IP + gateway on it.
type ContainerNetAttachment struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
}

// builtinNetworks are docker's own defaults, skipped by the listing: consumers
// only care about user-defined networks.
var builtinNetworks = map[string]struct{}{"bridge": {}, "none": {}, "host": {}}

// listNetworks returns all user-defined docker networks (builtins + none/host
// excluded). A single NetworkList call — the Summary already carries
// IPAM/Options, so there is no per-network inspect.
func (d *dockerClient) listNetworks(ctx context.Context) ([]NetworkStatus, error) {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NetworkStatus, 0, len(nets))
	for _, n := range nets {
		if _, builtin := builtinNetworks[n.Name]; builtin {
			continue
		}
		ns := NetworkStatus{
			Name:       n.Name,
			Driver:     n.Driver,
			Scope:      n.Scope,
			ID:         netShortID(n.ID),
			Internal:   n.Internal,
			Attachable: n.Attachable,
		}
		if len(n.IPAM.Config) > 0 {
			ns.Subnet = n.IPAM.Config[0].Subnet
			ns.Gateway = n.IPAM.Config[0].Gateway
		}
		// ipvlan/macvlan parent + VLAN tag (e.g. parent "eth0.50" → vlan_tag "50").
		if parent := n.Options["parent"]; parent != "" {
			ns.Parent = parent
			if i := strings.LastIndex(parent, "."); i >= 0 && i < len(parent)-1 {
				ns.VlanTag = parent[i+1:]
			}
		}
		if m := n.Options["ipvlan_mode"]; m != "" {
			ns.IpvlanMode = m
		}
		if m := n.Options["macvlan_mode"]; m != "" {
			ns.MacvlanMode = m
		}
		out = append(out, ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// containerNetAttachments returns the networks a container is attached to, each
// with the container's IP + gateway. A missing container yields nil
// (best-effort); the caller treats nil as "no attachments".
func (d *dockerClient) containerNetAttachments(ctx context.Context, nameOrID string) []ContainerNetAttachment {
	resp, err := d.cli.ContainerInspect(ctx, nameOrID)
	if err != nil || resp.NetworkSettings == nil {
		return nil
	}
	out := make([]ContainerNetAttachment, 0, len(resp.NetworkSettings.Networks))
	for name, ep := range resp.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		out = append(out, ContainerNetAttachment{Name: name, IP: ep.IPAddress, Gateway: ep.Gateway})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// netShortID truncates a network ID to 12 chars, the form docker itself displays.
func netShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
