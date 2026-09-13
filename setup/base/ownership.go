package base

import (
	"context"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal/ownership"
)

// GuardFederationClient binds outbound federation dispatch to one authorized
// activation. Remote servers cannot reject a retired owner, so the dispatch
// acceptance point is here: a transaction that has not yet started its HTTP
// round-trip is refused.
//
// Transactions already dispatched may still complete, which is the documented
// at-least-once behavior. Recovering them is the successor's job, using the
// durable queue and stable transaction identifiers.
func GuardFederationClient(client fclient.FederationClient, owner ownership.Owner) fclient.FederationClient {
	return &guardedFederationClient{FederationClient: client, owner: owner}
}

type guardedFederationClient struct {
	fclient.FederationClient
	owner ownership.Owner
}

func (g *guardedFederationClient) owned() error {
	if g.owner == nil {
		return fmt.Errorf("federation dispatch refused: ownership has not been established")
	}
	if err := g.owner.Valid(); err != nil {
		return fmt.Errorf("federation dispatch refused: %w", err)
	}
	return nil
}

func (g *guardedFederationClient) SendTransaction(ctx context.Context, t gomatrixserverlib.Transaction) (fclient.RespSend, error) {
	if err := g.owned(); err != nil {
		return fclient.RespSend{}, err
	}
	return g.FederationClient.SendTransaction(ctx, t)
}

func (g *guardedFederationClient) P2PSendTransactionToRelay(ctx context.Context, u spec.UserID, t gomatrixserverlib.Transaction, forwardingServer spec.ServerName) (fclient.EmptyResp, error) {
	if err := g.owned(); err != nil {
		return fclient.EmptyResp{}, err
	}
	return g.FederationClient.P2PSendTransactionToRelay(ctx, u, t, forwardingServer)
}
