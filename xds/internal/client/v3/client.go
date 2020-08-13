/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package v3 provides xDS v3 transport protocol specific functionality.
package v3

import (
	"context"
	"fmt"
	"sync"

	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/internal/grpclog"
	xdsclient "google.golang.org/grpc/xds/internal/client"
	"google.golang.org/grpc/xds/internal/version"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3adsgrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	v3discoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

func init() {
	xdsclient.RegisterAPIClientBuilder(clientBuilder{})
}

type clientBuilder struct{}

func (clientBuilder) Build(cc *grpc.ClientConn, opts xdsclient.BuildOptions) (xdsclient.APIClient, error) {
	return newClient(cc, opts)
}

func (clientBuilder) Version() version.TransportAPI {
	return version.TransportV3
}

func newClient(cc *grpc.ClientConn, opts xdsclient.BuildOptions) (xdsclient.APIClient, error) {
	nodeProto, ok := opts.NodeProto.(*v3corepb.Node)
	if !ok {
		return nil, fmt.Errorf("xds: unsupported Node proto type: %T, want %T", opts.NodeProto, v3corepb.Node{})
	}
	v3c := &client{
		cc:        cc,
		parent:    opts.Parent,
		nodeProto: nodeProto,
		logger:    opts.Logger,
	}
	v3c.ctx, v3c.cancelCtx = context.WithCancel(context.Background())
	v3c.TransportHelper = xdsclient.NewTransportHelper(v3c, opts.Logger, opts.Backoff)
	return v3c, nil
}

type adsStream v3adsgrpc.AggregatedDiscoveryService_StreamAggregatedResourcesClient

// client performs the actual xDS RPCs using the xDS v3 API. It creates a
// single ADS stream on which the different types of xDS requests and responses
// are multiplexed.
type client struct {
	*xdsclient.TransportHelper

	ctx       context.Context
	cancelCtx context.CancelFunc
	parent    xdsclient.UpdateHandler
	logger    *grpclog.PrefixLogger

	// ClientConn to the xDS gRPC server. Owned by the parent xdsClient.
	cc        *grpc.ClientConn
	nodeProto *v3corepb.Node

	mu sync.Mutex
	// ldsResourceName is the LDS resource_name to watch. It is set to the first
	// LDS resource_name to watch, and removed when the LDS watch is canceled.
	//
	// It's from the dial target of the parent ClientConn. RDS resource
	// processing needs this to do the host matching.
	ldsResourceName string
	ldsWatchCount   int
}

// AddWatch overrides the transport helper's AddWatch to save the LDS
// resource_name. This is required when handling an RDS response to perform hot
// matching.
func (v3c *client) AddWatch(resourceType, resourceName string) {
	v3c.mu.Lock()
	// Special handling for LDS, because RDS needs the LDS resource_name for
	// response host matching.
	if resourceType == version.V2ListenerURL || resourceType == version.V3ListenerURL {
		// Set hostname to the first LDS resource_name, and reset it when the
		// last LDS watch is removed. The upper level Client isn't expected to
		// watchLDS more than once.
		v3c.ldsWatchCount++
		if v3c.ldsWatchCount == 1 {
			v3c.ldsResourceName = resourceName
		}
	}
	v3c.mu.Unlock()
	v3c.TransportHelper.AddWatch(resourceType, resourceName)
}

func (v3c *client) RemoveWatch(resourceType, resourceName string) {
	v3c.mu.Lock()
	// Special handling for LDS, because RDS needs the LDS resource_name for
	// response host matching.
	if resourceType == version.V2ListenerURL || resourceType == version.V3ListenerURL {
		// Set hostname to the first LDS resource_name, and reset it when the
		// last LDS watch is removed. The upper level Client isn't expected to
		// watchLDS more than once.
		v3c.ldsWatchCount--
		if v3c.ldsWatchCount == 0 {
			v3c.ldsResourceName = ""
		}
	}
	v3c.mu.Unlock()
	v3c.TransportHelper.RemoveWatch(resourceType, resourceName)
}

func (v3c *client) NewStream(ctx context.Context) (grpc.ClientStream, error) {
	return v3adsgrpc.NewAggregatedDiscoveryServiceClient(v3c.cc).StreamAggregatedResources(v3c.ctx, grpc.WaitForReady(true))
}

// sendRequest sends a request for provided typeURL and resource on the provided
// stream.
//
// version is the ack version to be sent with the request
// - If this is the new request (not an ack/nack), version will be an empty
// string
// - If this is an ack, version will be the version from the response
// - If this is a nack, version will be the previous acked version (from
// versionMap). If there was no ack before, it will be an empty string
func (v3c *client) SendRequest(s grpc.ClientStream, resourceNames []string, typeURL, version, nonce string) error {
	stream, ok := s.(adsStream)
	if !ok {
		return fmt.Errorf("xds: Attempt to send request on unsupported stream type: %T", s)
	}
	req := &v3discoverypb.DiscoveryRequest{
		Node:          v3c.nodeProto,
		TypeUrl:       typeURL,
		ResourceNames: resourceNames,
		VersionInfo:   version,
		ResponseNonce: nonce,
		// TODO: populate ErrorDetails for nack.
	}
	if err := stream.Send(req); err != nil {
		return fmt.Errorf("xds: stream.Send(%+v) failed: %v", req, err)
	}
	v3c.logger.Debugf("ADS request sent: %v", req)
	return nil
}

// RecvResponse blocks on the receipt of one response message on the provided
// stream.
func (v3c *client) RecvResponse(s grpc.ClientStream) (proto.Message, error) {
	stream, ok := s.(adsStream)
	if !ok {
		return nil, fmt.Errorf("xds: Attempt to receive response on unsupported stream type: %T", s)
	}

	resp, err := stream.Recv()
	if err != nil {
		// TODO: call watch callbacks with error when stream is broken.
		return nil, fmt.Errorf("xds: stream.Recv() failed: %v", err)
	}
	v3c.logger.Infof("ADS response received, type: %v", resp.GetTypeUrl())
	v3c.logger.Debugf("ADS response received: %v", resp)
	return resp, nil
}

func (v3c *client) HandleResponse(r proto.Message) (string, string, string, error) {
	resp, ok := r.(*v3discoverypb.DiscoveryResponse)
	if !ok {
		return "", "", "", fmt.Errorf("xds: unsupported message type: %T", resp)
	}

	// Note that the xDS transport protocol is versioned independently of
	// the resource types, and it is supported to transfer older versions
	// of resource types using new versions of the transport protocol, or
	// vice-versa. Hence we need to handle v3 type_urls as well here.
	var err error
	switch resp.GetTypeUrl() {
	case version.V2ListenerURL, version.V3ListenerURL:
		err = v3c.handleLDSResponse(resp)
	case version.V2RouteConfigURL, version.V3RouteConfigURL:
		err = v3c.handleRDSResponse(resp)
	case version.V2ClusterURL, version.V3ClusterURL:
		err = v3c.handleCDSResponse(resp)
	case version.V2EndpointsURL, version.V3EndpointsURL:
		err = v3c.handleEDSResponse(resp)
	default:
		return "", "", "", xdsclient.ErrResourceTypeUnsupported{
			ErrStr: fmt.Sprintf("Resource type %v unknown in response from server", resp.GetTypeUrl()),
		}
	}
	return resp.GetTypeUrl(), resp.GetVersionInfo(), resp.GetNonce(), err
}

// handleLDSResponse processes an LDS response received from the xDS server. On
// receipt of a good response, it also invokes the registered watcher callback.
func (v3c *client) handleLDSResponse(resp *v3discoverypb.DiscoveryResponse) error {
	update, err := xdsclient.UnmarshalListener(resp.GetResources(), v3c.logger)
	if err != nil {
		return err
	}
	v3c.parent.NewListeners(update)
	return nil
}

// handleRDSResponse processes an RDS response received from the xDS server. On
// receipt of a good response, it caches validated resources and also invokes
// the registered watcher callback.
func (v3c *client) handleRDSResponse(resp *v3discoverypb.DiscoveryResponse) error {
	v3c.mu.Lock()
	hostname := v3c.ldsResourceName
	v3c.mu.Unlock()

	update, err := xdsclient.UnmarshalRouteConfig(resp.GetResources(), hostname, v3c.logger)
	if err != nil {
		return err
	}
	v3c.parent.NewRouteConfigs(update)
	return nil
}

// handleCDSResponse processes an CDS response received from the xDS server. On
// receipt of a good response, it also invokes the registered watcher callback.
func (v3c *client) handleCDSResponse(resp *v3discoverypb.DiscoveryResponse) error {
	update, err := xdsclient.UnmarshalCluster(resp.GetResources(), v3c.logger)
	if err != nil {
		return err
	}
	v3c.parent.NewClusters(update)
	return nil
}

func (v3c *client) handleEDSResponse(resp *v3discoverypb.DiscoveryResponse) error {
	update, err := xdsclient.UnmarshalEndpoints(resp.GetResources(), v3c.logger)
	if err != nil {
		return err
	}
	v3c.parent.NewEndpoints(update)
	return nil
}