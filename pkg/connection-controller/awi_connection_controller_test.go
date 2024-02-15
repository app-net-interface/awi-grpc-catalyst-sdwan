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
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/app-net-interface/catalyst-sdwan-app-client/acl"
	"github.com/app-net-interface/catalyst-sdwan-app-client/common"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection"
	"github.com/app-net-interface/catalyst-sdwan-app-client/device"
	"github.com/app-net-interface/catalyst-sdwan-app-client/feature"
	"github.com/app-net-interface/catalyst-sdwan-app-client/types"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vpc"

	"awi-grpc-catalyst-sdwan/pkg/mocks"
	"awi-grpc-catalyst-sdwan/pkg/mocks/mock_vmanage"
)

func TestEmptyConnectionRequests(t *testing.T) {
	var requests []types.ConnectionRequest
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	err := manager.CreateConnection(context.TODO(), requests)
	require.NoError(t, err)
}

func TestConnectionRequests(t *testing.T) {
	connectionID := "test-connection"
	request := makeConnectionRequestWithSubnets("aws")
	requests := []types.ConnectionRequest{*request}
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)

	manager.dbClient.EXPECT().GetConnectionRequest(connectionID).Return(nil, nil)
	tag := "new-test-tag"
	expectedRequest := makeConnectionRequestWithDestinations(1, "aws")
	expectedRequest.ID = connectionID
	expectedRequest.Source.ID = tag
	manager.dbClient.EXPECT().UpdateConnectionRequest(expectedRequest, connectionID).Return(nil)

	mockACLPolicy(manager, "site-id", "feature-template-id", []acl.Sequence{})

	expectedMatrices := manager.createConnectionMatrices(expectedRequest, nil, true)
	mockConnectionRequest(manager.client, expectedMatrices, "aws")
	vpcID := "test-vpc-id"
	vpcData := &vpc.VPC{
		Tag: tag,
	}
	manager.client.VPCClient.EXPECT().Get(gomock.Any(), "AWS", vpcID).Return(vpcData, nil)

	provider := mocks.NewMockCloudProvider(ctrl)
	provider.EXPECT().GetVPCIDForCIDR(gomock.Any(), "10.10.10.0/24").Return(vpcID, nil)
	manager.strategy.EXPECT().GetProvider(gomock.Any(), "aws").Return(provider, nil)

	err := manager.CreateConnection(context.TODO(), requests)
	require.NoError(t, err)
}

func TestProcessSubnetSubstituteSubnet(t *testing.T) {
	cloud := "aws"
	subnet := "10.10.10.0/24"
	vpcID := "test-vpc-id"
	subnet2 := "10.10.20.0/24"
	vpc2ID := "test-vpc-2-id"
	request := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: cloud,
			Type:     "subnet",
			Networks: []string{subnet},
		},
		Destination: []types.ConnectionRequestDestination{{
			Provider: cloud,
			Type:     "subnet",
			ID:       subnet2,
		}},
	}
	expectedRequest := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: cloud,
			Type:     "vpc",
			ID:       vpcID,
			Networks: []string{subnet},
		},
		Destination: []types.ConnectionRequestDestination{{
			Provider: cloud,
			Type:     "vpc",
			ID:       vpc2ID,
		}},
	}
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)

	provider := mocks.NewMockCloudProvider(ctrl)
	provider.EXPECT().GetVPCIDForCIDR(gomock.Any(), subnet).Return(vpcID, nil)
	provider.EXPECT().GetVPCIDForCIDR(gomock.Any(), subnet2).Return(vpc2ID, nil)
	manager.strategy.EXPECT().GetProvider(gomock.Any(), cloud).Return(provider, nil).Times(2)

	err := manager.processSubnets(context.TODO(), request)
	require.NoError(t, err)
	require.Equal(t, expectedRequest, request)
}

func TestProcessSubnetNotSubstitute(t *testing.T) {
	subnet := "10.10.10.0/24"
	vpcID := "test-vpc-id"
	request := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "vpc",
			ID:       "test-vpc-id",
			Networks: []string{subnet},
		},
	}
	expectedRequest := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "vpc",
			ID:       vpcID,
			Networks: []string{subnet},
		},
	}
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)

	err := manager.processSubnets(context.TODO(), request)
	require.NoError(t, err)
	require.Equal(t, expectedRequest, request)
}

func TestTagConnectionUsingExistingTag(t *testing.T) {
	vpcID := "test-vpc-id"
	request := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "vpc",
			ID:       vpcID,
		},
	}
	tag := "new-test-tag"
	expectedRequest := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "tag",
			ID:       tag,
		},
	}
	vpcData := &vpc.VPC{
		Tag: tag,
	}
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	manager.client.VPCClient.EXPECT().Get(gomock.Any(), "AWS", vpcID).Return(vpcData, nil)

	err := manager.tagConnection(context.TODO(), request)
	require.NoError(t, err)
	require.Equal(t, expectedRequest, request)
}

func TestCreateNewTagAndTagConnection(t *testing.T) {
	vpcID := "test-vpc-id"
	request := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "vpc",
			ID:       vpcID,
		},
	}
	expectedRequest := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "tag",
			ID:       vpcID,
		},
	}
	vpcData := &vpc.VPC{}
	longPollID := "test-poll-id"
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	manager.client.VPCClient.EXPECT().Get(gomock.Any(), "AWS", vpcID).Return(vpcData, nil)
	manager.client.VPCClient.EXPECT().CreateVPCTag(gomock.Any(), vpcID, vpcData).Return(longPollID, nil)
	manager.client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), longPollID).Return(nil)

	err := manager.tagConnection(context.TODO(), request)
	require.NoError(t, err)
	require.Equal(t, expectedRequest, request)
}

func TestTagDestinationConnectionUsingExistingTag(t *testing.T) {
	vpcID := "test-vpc-id"
	request := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "vpc",
			ID:       vpcID,
		},
		Destination: []types.ConnectionRequestDestination{
			{
				Provider: "aws",
				Type:     "vpc",
				ID:       vpcID,
			},
		},
	}
	tag := "new-test-tag"
	expectedRequest := &types.ConnectionRequest{
		Source: types.ConnectionRequestSource{
			Provider: "aws",
			Type:     "tag",
			ID:       tag,
		},
		Destination: []types.ConnectionRequestDestination{
			{
				Provider: "aws",
				Type:     "tag",
				ID:       tag,
			},
		},
	}
	vpcData := &vpc.VPC{
		Tag: tag,
	}
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	manager.client.VPCClient.EXPECT().Get(gomock.Any(), "AWS", vpcID).Return(vpcData, nil).Times(2)

	err := manager.tagConnection(context.TODO(), request)
	require.NoError(t, err)
	require.Equal(t, expectedRequest, request)
}

func TestCreateDBConnectionRequest(t *testing.T) {
	cloudType := "aws"
	request := makeConnectionRequestWithDestinations(2, cloudType)
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	manager.dbClient.EXPECT().UpdateConnectionRequest(request, request.ID).Return(nil)

	err := manager.createDBConnectionRequest(request)
	require.NoError(t, err)
}

func TestCreateEmptyConnectionRequest(t *testing.T) {
	cloudType := "aws"
	request := makeEmptyConnectionRequest(cloudType)
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)

	_, err := manager.updateConnectionRequest(context.TODO(), request, nil, true)
	require.NoError(t, err)
}

func TestCreateConnectionRequest(t *testing.T) {
	expectedMatrices := []connection.Matrix{
		{
			Connection:      "enabled",
			SourceID:        "source-id",
			SourceType:      "tag",
			DestinationID:   "destination-0-id",
			DestinationType: "tag",
		},
		{
			Connection:      "enabled",
			DestinationID:   "source-id",
			SourceType:      "tag",
			SourceID:        "destination-0-id",
			DestinationType: "tag",
		},
	}
	cloudType := "aws"
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	mockConnectionRequest(manager.client, expectedMatrices, cloudType)

	request := makeConnectionRequestWithDestinations(1, cloudType)

	_, err := manager.updateConnectionRequest(context.TODO(), request, nil, true)
	require.NoError(t, err)
}

func TestUpdateSameConnectionRequest(t *testing.T) {
	cloudType := "aws"
	request := makeConnectionRequestWithDestinations(2, cloudType)

	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)

	_, err := manager.updateConnectionRequest(context.TODO(), request, request, true)
	require.NoError(t, err)
}

func TestUpdateConnectionRequest(t *testing.T) {
	expectedMatrices := []connection.Matrix{
		{
			Connection:      "enabled",
			SourceID:        "source-id",
			SourceType:      "tag",
			DestinationID:   "destination-0-id",
			DestinationType: "tag",
		},
		{
			Connection:      "enabled",
			DestinationID:   "source-id",
			SourceType:      "tag",
			SourceID:        "destination-0-id",
			DestinationType: "tag",
		},
		{
			Connection:      "disabled",
			SourceID:        "source-id",
			SourceType:      "tag",
			DestinationID:   "destination-2-id",
			DestinationType: "tag",
		},
		{
			Connection:      "disabled",
			DestinationID:   "source-id",
			SourceType:      "tag",
			SourceID:        "destination-2-id",
			DestinationType: "tag",
		},
	}

	cloudType := "aws"
	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	mockConnectionRequest(manager.client, expectedMatrices, cloudType)

	destinations := makeDestinations(3, cloudType)
	request := makeEmptyConnectionRequest(cloudType)
	request.Destination = []types.ConnectionRequestDestination{destinations[0], destinations[1]}
	oldRequest := makeEmptyConnectionRequest(cloudType)
	oldRequest.Destination = []types.ConnectionRequestDestination{destinations[1], destinations[2]}

	_, err := manager.updateConnectionRequest(context.TODO(), request, oldRequest, true)
	require.NoError(t, err)
}

func TestCreateACLPolicy(t *testing.T) {
	siteID := "site-id"
	featureTemplateID := "feature-template-id"
	sequences := []acl.Sequence{{SequenceIPType: "seq-1"}, {SequenceIPType: "seq-2"}}
	request := makeConnectionRequestWithSubnets("aws")

	ctrl := gomock.NewController(t)
	manager := newMockManager(ctrl)
	mockACLPolicy(manager, siteID, featureTemplateID, sequences)

	err := manager.createACLPolicy(context.TODO(), request, sequences)
	require.NoError(t, err)
}

func makeConnectionRequestWithSubnets(cloudType string) *types.ConnectionRequest {
	request := makeConnectionRequestWithDestinations(1, cloudType)
	request.Source.Type = "subnet"
	return request
}

func makeConnectionRequestWithDestinations(destinationsCount int, cloudType string) *types.ConnectionRequest {
	request := makeEmptyConnectionRequest(cloudType)
	request.Destination = makeDestinations(destinationsCount, cloudType)
	return request
}

func makeEmptyConnectionRequest(cloudType string) *types.ConnectionRequest {
	return &types.ConnectionRequest{
		ID: "test-connection",
		Source: types.ConnectionRequestSource{
			FeatureTemplateID: "feature-template-id",
			SiteID:            "site-id",
			Provider:          cloudType,
			Type:              "tag",
			ID:                "source-id",
			Networks:          []string{"10.10.10.0/24"},
		},
	}
}

func makeDestinations(count int, cloudType string) []types.ConnectionRequestDestination {
	var destinations []types.ConnectionRequestDestination
	for i := 0; i < count; i++ {
		destination := types.ConnectionRequestDestination{
			Provider: cloudType,
			Type:     "tag",
			ID:       fmt.Sprintf("destination-%d-id", i),
			DbID:     fmt.Sprintf("destination-db-%d-id", i),
			Metadata: types.Metadata{
				Name: fmt.Sprintf("destination-%d", i),
			},
		}
		destinations = append(destinations, destination)
	}
	return destinations
}

func mockACLPolicy(m *mockManager, siteID, featureTemplateID string, sequences []acl.Sequence) {
	aclName := "site-id-acl"
	aclData := createACLData(aclName, "", sequences)
	aclID := "test-acl-id"
	m.client.ACLClient.EXPECT().GetByName(gomock.Any(), aclName).Return(nil, nil)
	m.client.ACLClient.EXPECT().Create(gomock.Any(), aclData).Return(aclID, nil)
	m.dbClient.EXPECT().UpdateACL(&aclData, aclName).Return(nil)

	policyName := "site-id-policy"
	policyData := createPolicyData(policyName, aclID)
	policyID := "test-policy-id"
	m.client.PolicyClient.EXPECT().Create(gomock.Any(), policyData).Return(policyID, nil)

	deviceTemplateID := "test-device-template-id"
	deviceTemplate := &device.Template{TemplateID: deviceTemplateID}
	m.client.DeviceClient.EXPECT().GetFromSite(gomock.Any(), siteID).Return(deviceTemplate, nil)

	m.clock.EXPECT().Sleep(10 * time.Second)

	deviceTemplate.PolicyID = policyID
	attachedDevices := []*device.AttachedDevice{{HostName: "dev-1"}, {HostName: "dev-2"}}
	deviceResponse := &device.UpdateResponse{AttachedDevices: attachedDevices}
	m.client.DeviceClient.EXPECT().Update(gomock.Any(), deviceTemplate).Return(deviceResponse, nil)

	updateStatusID := "update-status-id"
	m.client.DeviceClient.EXPECT().PushConfiguration(gomock.Any(), attachedDevices, deviceTemplateID).Return(
		updateStatusID,
		nil,
	)

	m.client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), updateStatusID).Return(nil)

	featureTemplate := &feature.Template{
		TemplateType: "test",
	}
	m.client.FeatureClient.EXPECT().Get(gomock.Any(), featureTemplateID).Return(featureTemplate, nil)

	featureTemplate.TemplateDefinition.AccessList = createAccessList(aclName)
	masterTemplates := []string{"master-template-id-1", "master-template-id-2"}
	featureResponse := &common.UpdateResponse{MasterTemplatesAffected: masterTemplates}
	m.client.FeatureClient.EXPECT().Update(gomock.Any(), featureTemplateID, featureTemplate).Return(featureResponse, nil)

	for _, masterTemplateID := range masterTemplates {
		ad := []*device.AttachedDevice{{
			UUID: "abc",
		}}
		m.client.DeviceClient.EXPECT().GetAttachedDevices(gomock.Any(), masterTemplateID).Return(ad, nil)

		url := "push-config-id-url"
		m.client.DeviceClient.EXPECT().PushConfiguration(gomock.Any(), ad, masterTemplateID).Return(url, nil)

		m.client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), url).Return(nil)
	}
}

func mockConnectionRequest(client *mock_vmanage.StabClient, matrices []connection.Matrix, cloudType string) {
	connectionID := "connection-id"
	params := &connection.Parameters{
		CloudType: strings.ToUpper(cloudType),
		Matrices:  matrices,
	}
	client.ConnectionClient.EXPECT().Create(gomock.Any(), params).Return(connectionID, nil)
	client.StatusClient.EXPECT().ActionStatusLongPoll(gomock.Any(), connectionID).Return(nil)
}

func makeAccessRequest() types.AccessControlRequest {
	return types.AccessControlRequest{
		Source: types.AccessControlSource{
			Metadata: types.Metadata{
				Name: "test-source",
			},
			Subnet: types.Subnet{
				Prefixes: []string{"10.10.10.0/24"},
			},
		},
		Destination: types.AccessControlDestination{
			AccessControlSource: types.AccessControlSource{
				Subnet: types.Subnet{
					Prefixes: []string{"10.10.20.0/24", "10.10.30.0/24"},
					Endpoint: types.Endpoint{
						Common: types.Common{
							Metadata: types.Metadata{
								Name: "test-destination-1",
							},
							ProtocolPorts: types.ProtocolPorts{"icmp": []string{"22", "80"}},
						},
					},
				},
			},
		},
	}
}
