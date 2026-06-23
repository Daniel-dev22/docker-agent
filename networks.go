package main

import (
	"context"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/network"
)

// ---------------------------------------------------------------------------
// Network discovery — the agent-native replacement for the ansible
// docker_network_metrics collector (which we stop running in server_metrics).
// Published generically: ALL user-defined docker networks (platform=docker,
// domain=networks) PLUS the traefik container's network attachments
// (platform=traefik, domain=networks, for entrypoint management). Field shapes
// mirror plugins/module_utils/docker_network_metrics.py so the existing
// manage_docker_networks dropdown + traefik consumers resolve unchanged.
// ---------------------------------------------------------------------------

// NetworkStatus is one docker network row. Optional fields are omitted when
// empty, matching the python collector's conditional keys.
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
// container's IP + gateway on it. Replaces the legacy container_networks.<c>
// payload that fed platform=traefik/domain=networks.
type ContainerNetAttachment struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
}

// builtinNetworks are docker's defaults the legacy collector skipped
// (include_builtin=False) — the dropdowns only care about user-defined networks.
var builtinNetworks = map[string]struct{}{"bridge": {}, "none": {}, "host": {}}

// listNetworks returns all user-defined docker networks (builtins + none/host
// excluded), mirroring collect_docker_networks. A single NetworkList call — the
// Summary already carries IPAM/Options, so no per-network inspect.
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
// with the container's IP + gateway — replaces get_container_networks for the
// traefik container. A missing container yields nil (best-effort, like the
// python's empty-list-on-error); the caller treats nil as "no attachments".
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

// netShortID truncates a network ID to 12 chars (matching the python's [:12]).
func netShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
