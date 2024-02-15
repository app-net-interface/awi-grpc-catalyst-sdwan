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

package fake

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/docker/docker.v20/pkg/namesgenerator"

	connection_controller "awi-grpc-catalyst-sdwan/pkg/connection-controller"
	"awi-grpc-catalyst-sdwan/pkg/db"

	awi "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection"
)

// FakeController only adds and removes connections and app connections in database
type FakeController struct {
	logger   *logrus.Logger
	dbClient db.Client
}

func NewFakeController(logger *logrus.Logger, dbClient db.Client) *FakeController {
	return &FakeController{
		logger:   logger,
		dbClient: dbClient,
	}
}

func (c *FakeController) CreateConnection(ctx context.Context, requests []db.ConnectionRequest) error {
	for _, request := range requests {
		request.Status = connection.StateRunning
		err := c.dbClient.UpdateConnectionRequest(&request, request.ID)
		if err != nil {
			c.logger.Errorf("couldn't update connection request %s", request.ID)
			return err
		}
	}
	return nil
}

func (c *FakeController) DeleteConnection(ctx context.Context, connections []string) error {
	for _, connid := range connections {
		err := c.dbClient.DeleteConnectionRequest(connid)
		if err != nil {
			c.logger.Errorf("couldn't delete connection request %s", connid)
			return err
		}
	}
	return nil
}

func (c *FakeController) CreateAppConnection(ctx context.Context, appConnectionRequest *awi.AppConnectionRequest) error {
	aclDB := &db.AppConnection{
		ConnectionID:      appConnectionRequest.GetClusterConnectionReference(),
		ID:                namesgenerator.GetRandomName(0),
		Name:              appConnectionRequest.GetName(),
		FeatureTemplateID: "",
		Provider:          connection_controller.CiscoProvider,
		Status:            "SUCCESS",
	}
	return c.dbClient.UpdateAppConnection(aclDB, aclDB.ID)
}

func (c *FakeController) DeleteAppConnection(ctx context.Context, aclID string) error {
	return c.dbClient.DeleteAppConnection(aclID)
}

func (c *FakeController) Close() error {
	return nil
}

func (c *FakeController) Refresh(ctx context.Context) error {
	return nil
}

func (c *FakeController) Login() error {
	return nil
}

func (c *FakeController) RunMapping(deadline time.Duration) {
	return
}

func (m *FakeController) DiscoverNetworkDomains() error {
	return nil
}
