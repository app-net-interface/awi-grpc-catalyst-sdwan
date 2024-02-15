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

package connection_controller

import (
	"context"
	"time"

	awi "github.com/app-net-interface/awi-grpc/pb"
)

type NetworkDomainConnector string

const (
	VManageConnector NetworkDomainConnector = "vManage"
	AwiConnector     NetworkDomainConnector = "AWI"
)

type ConnectionController interface {
	CreateConnection(ctx context.Context, request *awi.ConnectionRequest) error
	DeleteConnection(ctx context.Context, connections []string) error
	CreateAppConnection(ctx context.Context, appConnectionRequest *awi.AppConnection) error
	DeleteAppConnection(ctx context.Context, appConnectionID string) error
	GetMatchedResources(ctx context.Context, appConnectionRequest *awi.AppConnection) (source *awi.MatchedResources, dest *awi.MatchedResources, err error)
	RefreshAppConnections(ctx context.Context) error
	RefreshNetworkDomainConnections(ctx context.Context) error
	Login() error
	Close() error
	RunMapping(deadline time.Duration)
	DiscoverNetworkDomains(ctx context.Context)
	GetConnectorType() NetworkDomainConnector
}
