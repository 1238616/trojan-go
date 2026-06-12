package singmux

import (
	"context"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

const Name = "SINGMUX"

type Tunnel struct{}

func (*Tunnel) Name() string {
	return Name
}

func (*Tunnel) NewClient(ctx context.Context, client tunnel.Client) (tunnel.Client, error) {
	return nil, common.NewError("singmux client not implemented (server-only)")
}

func (*Tunnel) NewServer(ctx context.Context, server tunnel.Server) (tunnel.Server, error) {
	return NewServer(ctx, server)
}

func init() {
	tunnel.RegisterTunnel(Name, &Tunnel{})
}
