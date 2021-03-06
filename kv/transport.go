// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package kv

import (
	"fmt"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/util/envutil"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/opentracing/opentracing-go"
	"google.golang.org/grpc"
)

// Allow local calls to be dispatched directly to the local server without
// sending an RPC.
var enableLocalCalls = envutil.EnvOrDefaultBool("enable_local_calls", true)

// TransportFactory encapsulates all interaction with the RPC
// subsystem, allowing it to be mocked out for testing. The factory
// function returns a Transport object which is used to send the given
// arguments to one or more replicas in the slice.
//
// In addition to actually sending RPCs, the transport is responsible
// for ordering replicas in accordance with SendOptions.Ordering and
// transport-specific knowledge such as connection health or latency.
//
// TODO(bdarnell): clean up this crufty interface; it was extracted
// verbatim from the non-abstracted code.
type TransportFactory func(
	SendOptions, *rpc.Context, ReplicaSlice, roachpb.BatchRequest,
) (Transport, error)

// Transport objects can send RPCs to one or more replicas of a range.
// All calls to Transport methods are made from a single thread, so
// Transports are not required to be thread-safe.
type Transport interface {
	// IsExhausted returns true if there are no more replicas to try.
	IsExhausted() bool

	// SendNext sends the rpc (captured at creation time) to the next
	// replica. May panic if the transport is exhausted. Should not
	// block; the transport is responsible for starting other goroutines
	// as needed.
	SendNext(chan BatchCall)

	// Close is called when the transport is no longer needed. It may
	// cancel any pending RPCs without writing any response to the channel.
	Close()
}

// grpcTransportFactory is the default TransportFactory, using GRPC.
func grpcTransportFactory(
	opts SendOptions,
	rpcContext *rpc.Context,
	replicas ReplicaSlice,
	args roachpb.BatchRequest,
) (Transport, error) {
	clients := make([]batchClient, 0, len(replicas))
	for _, replica := range replicas {
		conn, err := rpcContext.GRPCDial(replica.NodeDesc.Address.String())
		if err != nil {
			return nil, err
		}
		argsCopy := args
		argsCopy.Replica = replica.ReplicaDescriptor
		clients = append(clients, batchClient{
			remoteAddr: replica.NodeDesc.Address.String(),
			conn:       conn,
			client:     roachpb.NewInternalClient(conn),
			args:       argsCopy,
		})
	}

	// Put known-unhealthy clients last.
	nHealthy, err := splitHealthy(clients)
	if err != nil {
		return nil, err
	}

	var orderedClients []batchClient
	switch opts.Ordering {
	case orderStable:
		orderedClients = clients
	case orderRandom:
		// Randomly permute order, but keep known-unhealthy clients last.
		shuffleClients(clients[:nHealthy])
		shuffleClients(clients[nHealthy:])

		orderedClients = clients
	}
	// TODO(spencer): going to need to also sort by affinity; closest
	// ping time should win. Makes sense to have the rpc client/server
	// heartbeat measure ping times. With a bit of seasoning, each
	// node will be able to order the healthy replicas based on latency.

	return &grpcTransport{
		opts:           opts,
		rpcContext:     rpcContext,
		orderedClients: orderedClients,
	}, nil
}

type grpcTransport struct {
	opts           SendOptions
	rpcContext     *rpc.Context
	orderedClients []batchClient
}

func (gt *grpcTransport) IsExhausted() bool {
	return len(gt.orderedClients) == 0
}

// SendNext invokes the specified RPC on the supplied client when the
// client is ready. On success, the reply is sent on the channel;
// otherwise an error is sent.
func (gt *grpcTransport) SendNext(done chan BatchCall) {
	client := gt.orderedClients[0]
	gt.orderedClients = gt.orderedClients[1:]

	addr := client.remoteAddr
	if log.V(2) {
		log.Infof("sending request to %s: %+v", addr, client.args)
	}

	if localServer := gt.rpcContext.GetLocalInternalServerForAddr(addr); enableLocalCalls && localServer != nil {
		ctx, cancel := gt.opts.contextWithTimeout()
		defer cancel()

		reply, err := localServer.Batch(ctx, &client.args)
		done <- BatchCall{Reply: reply, Err: err}
		return
	}

	go func() {
		ctx, cancel := gt.opts.contextWithTimeout()
		defer cancel()

		c := client.conn
		for state, err := c.State(); state != grpc.Ready; state, err = c.WaitForStateChange(ctx, state) {
			if err != nil {
				done <- BatchCall{Err: err}
				return
			}
			if state == grpc.Shutdown {
				done <- BatchCall{Err: fmt.Errorf("rpc to %s failed as client connection was closed", addr)}
				return
			}
		}

		reply, err := client.client.Batch(ctx, &client.args)
		done <- BatchCall{Reply: reply, Err: err}
	}()
}

func (*grpcTransport) Close() {
	// TODO(bdarnell): Save the cancel functions of all pending RPCs and
	// call them here. (it's fine to ignore them for now since they'll
	// time out anyway)
}

// SenderTransportFactory wraps a client.Sender for use as a KV
// Transport. This is useful for tests that want to use DistSender
// without a full RPC stack.
func SenderTransportFactory(tracer opentracing.Tracer, sender client.Sender) TransportFactory {
	return func(
		_ SendOptions, _ *rpc.Context, _ ReplicaSlice, args roachpb.BatchRequest,
	) (Transport, error) {
		return &senderTransport{tracer, sender, args, false}, nil
	}
}

type senderTransport struct {
	tracer opentracing.Tracer
	sender client.Sender
	args   roachpb.BatchRequest

	called bool
}

func (s *senderTransport) IsExhausted() bool {
	return s.called
}

func (s *senderTransport) SendNext(done chan BatchCall) {
	sp := s.tracer.StartSpan("node")
	defer sp.Finish()
	ctx := opentracing.ContextWithSpan(context.Background(), sp)
	log.Trace(ctx, s.args.String())
	br, pErr := s.sender.Send(ctx, s.args)
	if br == nil {
		br = &roachpb.BatchResponse{}
	}
	if br.Error != nil {
		panic(roachpb.ErrorUnexpectedlySet(s.sender, br))
	}
	br.Error = pErr
	if pErr != nil {
		log.Trace(ctx, "error: "+pErr.String())
	}
	done <- BatchCall{Reply: br}
}

func (s *senderTransport) Close() {
}
