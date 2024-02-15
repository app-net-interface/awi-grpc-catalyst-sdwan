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
	"fmt"
	"testing"
	"time"

	"awi-grpc-catalyst-sdwan/pkg/mocks"
	"awi-grpc-catalyst-sdwan/pkg/mocks/mock_vmanage"

	"github.com/golang/mock/gomock"
	"github.com/sirupsen/logrus/hooks/test"

	"awi-grpc-catalyst-sdwan/pkg/db/mock_db"

	"github.com/app-net-interface/catalyst-sdwan-app-client/common"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection"
	"github.com/app-net-interface/catalyst-sdwan-app-client/device"
	"github.com/app-net-interface/catalyst-sdwan-app-client/feature"
	"github.com/app-net-interface/catalyst-sdwan-app-client/policy"
	"github.com/app-net-interface/catalyst-sdwan-app-client/types"
)

func TestDisconnectConnectionForNonExistingRequest(t *testing.T) {
	connectionID := "connection-id"
	ctrl := gomock.NewController(t)
	m := newMockManager(ctrl)
	m.dbClient.EXPECT().GetConnectionRequest(connectionID).Return(nil, nil)

	if err := m.disconnectConnections(context.TODO(), connectionID, false); err != nil {
		t.Errorf("disconnectConnections() error = %v", err)
	}
}

func TestDisconnectACL(t *testing.T) {
	ctrl := gomock.NewController(t)

	featureID := "feature-id"
	siteID := "site-id"

	m := newMockManager(ctrl)
	mockACLDeletion(m, featureID, siteID)

	if err := m.deleteACLPolicy(context.TODO(), siteID, featureID); err != nil {
		t.Errorf("deleteACLPolicy() error = %v", err)
	}
}

func TestDisconnectConnection(t *testing.T) {
	connectionID := "connection-id"
	cloudType := "aws"
	ctrl := gomock.NewController(t)
	m := newMockManager(ctrl)
	request := makeConnectionRequestWithDestinations(1, cloudType)
	m.dbClient.EXPECT().GetConnectionRequest(connectionID).Return(request, nil)
	m.dbClient.EXPECT().DeleteConnectionRequest(connectionID).Return(nil)

	expectedMatrices := []connection.Matrix{
		{
			Connection:      "disabled",
			SourceID:        "source-id",
			SourceType:      "tag",
			DestinationID:   "destination-0-id",
			DestinationType: "tag",
		},
		{
			Connection:      "disabled",
			DestinationID:   "source-id",
			SourceType:      "tag",
			SourceID:        "destination-0-id",
			DestinationType: "tag",
		},
	}

	mockACLDeletion(m, "feature-template-id", "site-id")
	mockConnectionRequest(m.client, expectedMatrices, cloudType)

	if err := m.disconnectConnections(context.TODO(), connectionID, false); err != nil {
		t.Errorf("disconnectConnections() error = %v", err)
	}
}

func TestDisconnectConnectionForOneDestination(t *testing.T) {
	connectionID := "test-connection"
	cloudType := "aws"
	ctrl := gomock.NewController(t)
	m := newMockManager(ctrl)
	request := makeConnectionRequestWithDestinations(3, cloudType)
	savedRequest := makeEmptyConnectionRequest(cloudType)
	savedRequest.Destination = []types.ConnectionRequestDestination{request.Destination[0], request.Destination[2]}
	m.dbClient.EXPECT().GetConnectionRequest(connectionID).Return(request, nil)
	m.dbClient.EXPECT().UpdateConnectionRequest(savedRequest, connectionID).Return(nil)

	expectedMatrices := []connection.Matrix{
		{
			Connection:      "disabled",
			SourceID:        "source-id",
			SourceType:      "tag",
			DestinationID:   "destination-1-id",
			DestinationType: "tag",
		},
		{
			Connection:      "disabled",
			SourceID:        "destination-1-id",
			SourceType:      "tag",
			DestinationID:   "source-id",
			DestinationType: "tag",
		},
	}
	mockConnectionRequest(m.client, expectedMatrices, cloudType)

	destinationID := fmt.Sprintf("%s:%s", connectionID, "destination-db-1-id")
	if err := m.disconnectConnections(context.TODO(), destinationID, false); err != nil {
		t.Errorf("disconnectConnections() error = %v", err)
	}
}

func mockACLDeletion(m *mockManager, featureID, siteID string) {
	ft := &feature.Template{}
	m.client.FeatureClient.EXPECT().Get(gomock.Any(), featureID).Return(ft, nil)

	addAccessListToFeatureTemplate(ft)
	templateID := "master-template-id"
	updateResponse := &common.UpdateResponse{
		MasterTemplatesAffected: []string{templateID},
	}
	m.client.FeatureClient.EXPECT().Update(gomock.Any(), featureID, ft).Return(updateResponse, nil)

	attachedDevices := []*device.AttachedDevice{
		{
			SiteID: "irrelevant",
		},
	}
	m.client.DeviceClient.EXPECT().GetAttachedDevices(gomock.Any(), templateID).Return(attachedDevices, nil)

	statusID := "status-id"
	m.client.DeviceClient.EXPECT().PushConfiguration(gomock.Any(), attachedDevices, templateID).Return(statusID, nil)

	m.client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), statusID).Return(nil)

	policyID := "policy-id"
	deviceTemplateID := "device-template-id"
	dt := &device.Template{
		TemplateID: deviceTemplateID,
		PolicyID:   policyID,
	}
	m.client.DeviceClient.EXPECT().GetFromSite(gomock.Any(), siteID).Return(dt, nil)

	dtInput := &device.Template{
		TemplateID: deviceTemplateID,
	}
	deviceResponse := &device.UpdateResponse{
		AttachedDevices: attachedDevices,
	}
	m.clock.EXPECT().Sleep(10 * time.Second)
	m.client.DeviceClient.EXPECT().Update(gomock.Any(), dtInput).Return(deviceResponse, nil)
	m.client.DeviceClient.EXPECT().PushConfiguration(gomock.Any(), attachedDevices, deviceTemplateID).Return(statusID, nil)
	m.client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), statusID).Return(nil)

	aclID := "acl-id"
	input := &policy.Input{
		PolicyDefinition: policy.Definition{
			Assemblies: []policy.Assembly{
				{
					DefinitionID: aclID,
				},
			},
		},
	}
	m.client.PolicyClient.EXPECT().Get(gomock.Any(), policyID).Return(input, nil)

	m.clock.EXPECT().Sleep(5 * time.Second)
	m.client.PolicyClient.EXPECT().Delete(gomock.Any(), policyID).Return(nil)

	m.client.ACLClient.EXPECT().Delete(gomock.Any(), aclID).Return(nil)
}

type mockManager struct {
	*AWIConnectionController
	client   *mock_vmanage.StabClient
	dbClient *mock_db.MockClient
	hook     *test.Hook
	clock    *mocks.MockClock
	strategy *mocks.MockStrategy
}

func newMockManager(ctrl *gomock.Controller) *mockManager {
	client := mock_vmanage.NewStab(ctrl)
	dbClient := mock_db.NewMockClient(ctrl)
	logger, hook := test.NewNullLogger()
	clock := mocks.NewMockClock(ctrl)
	strategy := mocks.NewMockStrategy(ctrl)

	return &mockManager{
		AWIConnectionController: newManager(client, logger, dbClient, clock, strategy),
		client:                  client,
		dbClient:                dbClient,
		hook:                    hook,
		clock:                   clock,
		strategy:                strategy,
	}
}
