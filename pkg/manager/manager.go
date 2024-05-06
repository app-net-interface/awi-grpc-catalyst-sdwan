// Copyright (c) 2023 Cisco Systems, Inc. and its affiliates
// All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http:www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/app-net-interface/awi-infra-guard/provider"

	connection_controller "awi-grpc-catalyst-sdwan/pkg/connection-controller"
	"awi-grpc-catalyst-sdwan/pkg/db"

	awiGrpc "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/catalyst-sdwan-app-client/client"
	"github.com/app-net-interface/catalyst-sdwan-app-client/site"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vmanage"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vpc"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vpn"
	"github.com/sirupsen/logrus"
)

const (
	channelSize = 100
)

type requestType string

const (
	connectionRequest       = "connection"
	disconnectionRequest    = "disconnection"
	appConnectionRequest    = "appConnection"
	appDisconnectionRequest = "appDisconnection"
)

type request struct {
	ctx         context.Context
	retires     int
	requestType requestType
	data        interface{}
}

type processFunc func(ctx context.Context, man connection_controller.ConnectionController, deadline time.Duration, data interface{}) error

type Manager struct {
	client      vmanage.Client
	logger      *logrus.Logger
	queue       chan *request
	strategy    map[requestType]processFunc
	db          db.Client
	connManager connection_controller.ConnectionController
	deadline    time.Duration
	wait        time.Duration
	maxRetries  int
}

func NewManager(client vmanage.Client,
	dbClient db.Client,
	logger *logrus.Logger,
	providerStrategy provider.Strategy,
	ndConnector connection_controller.NetworkDomainConnector) *Manager {
	queue := make(chan *request, channelSize)
	mgr := connection_controller.NewConnectionControllerWithDb(client, logger, dbClient, providerStrategy, ndConnector)

	strategy := map[requestType]processFunc{
		connectionRequest:       processConnectionRequest,
		disconnectionRequest:    processDisconnectRequest,
		appConnectionRequest:    processAppConnectionRequest,
		appDisconnectionRequest: processAppDisconnectRequest,
	}
	return &Manager{
		client:      client,
		logger:      logger,
		queue:       queue,
		db:          dbClient,
		connManager: mgr,
		strategy:    strategy,
		deadline:    120 * time.Second,
		wait:        30 * time.Second,
		maxRetries:  3,
	}
}

func (m *Manager) RunMapping(ctx context.Context) {
	m.connManager.RunMapping(m.deadline)
	ticker := time.NewTicker(m.wait)
	for {
		select {
		case <-ticker.C:
			m.connManager.RunMapping(m.deadline)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) RunNetworkDomainDiscovery(ctx context.Context) {
	m.connManager.DiscoverNetworkDomains(ctx)
	ticker := time.NewTicker(m.wait)
	for {
		select {
		case <-ticker.C:
			m.connManager.DiscoverNetworkDomains(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) ProcessQueue(ctx context.Context) {
	for {
		select {
		case request := <-m.queue:
			m.logger.Infof("Processing %s request", request.requestType)
			m.processRequest(request)

		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) Refresh(ctx context.Context) {
	m.refresh()
	ticker := time.NewTicker(m.wait)
	for {
		select {
		case <-ticker.C:
			m.refresh()
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), m.deadline)
	defer cancel()
	err := m.connManager.RefreshAppConnections(ctx)
	if err != nil {
		m.logger.Errorf("could not refresh app connection: %v", err)
	}
	err = m.connManager.RefreshNetworkDomainConnections(ctx)
	if err != nil {
		m.logger.Errorf("could not refresh networ domain connections: %v", err)
	}
}

func (m *Manager) processRequest(request *request) {
	process, ok := m.strategy[request.requestType]
	if !ok {
		m.logger.Errorf("Unsupported request type %s", request.requestType)
		return
	}
	ctx := context.WithValue(context.Background(), "id", request.ctx.Value("id"))
	if err := process(ctx, m.connManager, m.deadline, request.data); err != nil {
		m.logger.Errorf("failed process request %s, %d retry: %s", request.requestType, request.retires, err)
		if request.retires < m.maxRetries {
			if errors.Is(err, &client.LoginError{}) {
				if err := m.connManager.Login(); err != nil {
					m.logger.Errorf("could not login: %v", err)
				}
			}
			m.logger.Infof("rescheduling request %s", request.requestType)
			time.Sleep(time.Second * 10)
			request.retires++
			m.queue <- request
		}
	}
}

func processConnectionRequest(ctx context.Context, man connection_controller.ConnectionController, deadline time.Duration, request interface{}) error {
	csr := request.(*awiGrpc.ConnectionRequest)
	if csr == nil {
		return fmt.Errorf("empty connection request message")
	}
	withTimeout, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	return man.CreateConnection(withTimeout, csr)
}

func processDisconnectRequest(ctx context.Context, man connection_controller.ConnectionController, deadline time.Duration, request interface{}) error {
	dsr := request.(*awiGrpc.DisconnectRequest)
	withTimeout, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	return man.DeleteConnection(withTimeout, []string{dsr.ConnectionId})
}

func processAppConnectionRequest(ctx context.Context, man connection_controller.ConnectionController, deadline time.Duration, request interface{}) error {
	acr := request.(*awiGrpc.AppConnection)

	withTimeout, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	return man.CreateAppConnection(withTimeout, acr)
}

func processAppDisconnectRequest(ctx context.Context, man connection_controller.ConnectionController, deadline time.Duration, data interface{}) error {
	adr := data.(*awiGrpc.AppDisconnectionRequest)

	withTimeout, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	return man.DeleteAppConnection(withTimeout, adr.GetConnectionId())
}

func (m *Manager) AddConnectionRequest(ctx context.Context, data *awiGrpc.ConnectionRequest) {
	m.addRequest(ctx, connectionRequest, data)
}

func (m *Manager) AddDisconnectRequest(ctx context.Context, data *awiGrpc.DisconnectRequest) {
	m.addRequest(ctx, disconnectionRequest, data)
}

func (m *Manager) AddAppConnectionRequest(ctx context.Context, data *awiGrpc.AppConnection) {
	m.addRequest(ctx, appConnectionRequest, data)
}

func (m *Manager) AddAppDisconnectRequest(ctx context.Context, data *awiGrpc.AppDisconnectionRequest) {
	m.addRequest(ctx, appDisconnectionRequest, data)
}

func (m *Manager) addRequest(ctx context.Context, reqType requestType, data interface{}) {
	m.queue <- &request{
		ctx:         ctx,
		retires:     0,
		requestType: reqType,
		data:        data,
	}
}

func (m *Manager) ListSites(ctx context.Context) (*awiGrpc.ListSiteResponse, error) {
	if !m.IsConnectionControllerVManage() {
		return &awiGrpc.ListSiteResponse{}, nil
	}
	sites, err := m.client.Site().List(ctx)
	if err != nil {
		return nil, err
	}
	return &awiGrpc.ListSiteResponse{
		Sites: convertSites(sites),
	}, nil
}

func (m *Manager) ListVPCs(ctx context.Context, request *awiGrpc.ListVPCRequest) (*awiGrpc.ListVPCResponse, error) {
	if !m.IsConnectionControllerVManage() {
		return &awiGrpc.ListVPCResponse{}, nil
	}
	var vpcs = make([]*vpc.VPC, 0)
	var err error
	if request.Provider == "" && request.Region == "" && request.AccountIDs == "" {
		vpcs, err = m.client.VPC().ListAll(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		params := &vpc.ListVPCParameters{
			Region:     request.Region,
			CloudType:  request.Provider,
			AccountIDs: request.AccountIDs,
		}
		vpcs, err = m.client.VPC().ListWithParameters(ctx, params)
		if err != nil {
			return nil, err
		}
	}

	return &awiGrpc.ListVPCResponse{
		VPCs: convertVPCs(vpcs),
	}, nil
}

func (m *Manager) GetMatchedResources(ctx context.Context, appConnection *awiGrpc.AppConnection) (*awiGrpc.MatchedResources, *awiGrpc.MatchedResources, error) {
	return m.connManager.GetMatchedResources(ctx, appConnection)
}

func (m *Manager) ListVPCTags(ctx context.Context, request *awiGrpc.ListVPCTagRequest) (*awiGrpc.ListVPCResponse, error) {
	if !m.IsConnectionControllerVManage() {
		return &awiGrpc.ListVPCResponse{}, nil
	}
	params := &vpc.ListVPCTagParameters{
		Region:    request.Region,
		CloudType: request.Provider,
		TagName:   request.Tag,
	}
	vpcs, err := m.client.VPC().ListWithTagWithParameters(ctx, params)
	if err != nil {
		return nil, err
	}
	return &awiGrpc.ListVPCResponse{
		VPCs: convertVPCs(vpcs),
	}, nil
}

func (m *Manager) ListVPNs(ctx context.Context, request *awiGrpc.ListVPNRequest) (*awiGrpc.ListVPNResponse, error) {
	if !m.IsConnectionControllerVManage() {
		return &awiGrpc.ListVPNResponse{}, nil
	}
	vpns, err := m.client.VPN().List(ctx, request.Provider)
	if err != nil {
		return nil, err
	}
	return &awiGrpc.ListVPNResponse{
		VPNs: convertVPNs(vpns),
	}, nil
}

func (m *Manager) IsConnectionControllerVManage() bool {
	return m.connManager.GetConnectorType() == connection_controller.VManageConnector
}

func convertSites(sites []*site.Site) []*awiGrpc.SiteDetail {
	result := make([]*awiGrpc.SiteDetail, 0, len(sites))
	for _, site := range sites {
		result = append(result, &awiGrpc.SiteDetail{
			ID:     site.ChasisNumber,
			Name:   site.NcsDeviceName,
			SiteID: site.SiteID,
			IP:     site.DeviceIP,
		})
	}
	return result
}

func convertVPCs(vpcs []*vpc.VPC) []*awiGrpc.VPC {
	result := make([]*awiGrpc.VPC, 0, len(vpcs))
	for _, vpc := range vpcs {
		result = append(result, &awiGrpc.VPC{
			ID:          vpc.HostVPCID,
			Name:        vpc.HostVPCName,
			Tag:         vpc.Tag,
			Region:      vpc.Region,
			AccountName: vpc.AccountName,
			Provider:    vpc.CloudType,
		})
	}
	return result
}

func convertVPNs(vpns []*vpn.Data) []*awiGrpc.VPN {
	result := make([]*awiGrpc.VPN, 0, len(vpns))
	for _, vpn := range vpns {
		result = append(result, &awiGrpc.VPN{
			ID:          vpn.ID,
			SegmentID:   vpn.SegmentID,
			SegmentName: vpn.SegmentName,
		})
	}
	return result
}
