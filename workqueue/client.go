/*
Copyright 2024 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package workqueue

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	delegate "chainguard.dev/go-grpc-kit/pkg/options"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
)

type Client interface {
	WorkqueueServiceClient

	Close() error
}

func NewWorkqueueClient(ctx context.Context, endpoint string, addlOpts ...grpc.DialOption) (Client, error) {
	apiURI, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse service address, must be a url: %w", err)
	}

	target, opts := delegate.GRPCOptions(*apiURI)
	opts = append(opts, keepaliveOptions(apiURI.Scheme)...)

	// If the endpoint is TLS terminated (not on K8s), then we are running on
	// Cloud Run and we should authenticate with an ID token.
	if strings.HasPrefix(endpoint, "https://") {
		ts, err := idtoken.NewTokenSource(ctx, endpoint)
		if err != nil {
			return nil, fmt.Errorf("google identity token source: %w", err)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(oauth.TokenSource{
			TokenSource: oauth2.ReuseTokenSource(nil, ts),
		}))
	}

	opts = append(opts, addlOpts...)

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("NewWorkqueueClient: failed to connect to the server: %w", err)
	}

	return &clients{
		WorkqueueServiceClient: NewWorkqueueServiceClient(conn),
		conn:                   conn,
	}, nil
}

// keepaliveOptions returns the keepalive dial options for a target with the
// given URL scheme. An https target is a Cloud Run service reached through
// Google's frontend, where an outbound VPC connection that sits idle for ten
// minutes is severed without notice; pinging it keeps a long unary RPC's
// connection alive and surfaces a dead one within the ping timeout instead of
// on the next write. Any other target talks to a gRPC server directly, and a
// server on gRPC's default enforcement policy answers these pings with a
// too_many_pings GOAWAY, so those targets get none. Callers can still supply
// their own keepalive parameters through addlOpts, which apply last.
func keepaliveOptions(scheme string) []grpc.DialOption {
	if scheme != "https" {
		return nil
	}
	return []grpc.DialOption{delegate.KeepaliveDialOption()}
}

type clients struct {
	WorkqueueServiceClient

	conn *grpc.ClientConn
}

// Close implements Client
func (c *clients) Close() error {
	return c.conn.Close()
}
