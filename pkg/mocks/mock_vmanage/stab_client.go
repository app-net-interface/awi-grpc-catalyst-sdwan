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

package mock_vmanage

import "github.com/golang/mock/gomock"

type StabClient struct {
	*MockClient
	ACLClient        *MockACL
	ConnectionClient *MockConnection
	DeviceClient     *MockDevice
	FeatureClient    *MockFeature
	VPNClient        *MockVPN
	VPCClient        *MockVPC
	StatusClient     *MockStatus
	SiteClient       *MockSite
	PolicyClient     *MockPolicy
}

func NewStab(ctrl *gomock.Controller) *StabClient {
	client := NewMockClient(ctrl)

	aclClient := NewMockACL(ctrl)
	client.EXPECT().ACL().Return(aclClient).AnyTimes()

	connectionClient := NewMockConnection(ctrl)
	client.EXPECT().Connection().Return(connectionClient).AnyTimes()

	deviceClient := NewMockDevice(ctrl)
	client.EXPECT().Device().Return(deviceClient).AnyTimes()

	featureClient := NewMockFeature(ctrl)
	client.EXPECT().Feature().Return(featureClient).AnyTimes()

	vpnClient := NewMockVPN(ctrl)
	client.EXPECT().VPN().Return(vpnClient).AnyTimes()

	vpcClient := NewMockVPC(ctrl)
	client.EXPECT().VPC().Return(vpcClient).AnyTimes()

	statusClient := NewMockStatus(ctrl)
	client.EXPECT().Status().Return(statusClient).AnyTimes()

	siteClient := NewMockSite(ctrl)
	client.EXPECT().Site().Return(siteClient).AnyTimes()

	policyClient := NewMockPolicy(ctrl)
	client.EXPECT().Policy().Return(policyClient).AnyTimes()

	return &StabClient{
		MockClient:       client,
		ACLClient:        aclClient,
		ConnectionClient: connectionClient,
		DeviceClient:     deviceClient,
		FeatureClient:    featureClient,
		VPNClient:        vpnClient,
		VPCClient:        vpcClient,
		StatusClient:     statusClient,
		SiteClient:       siteClient,
		PolicyClient:     policyClient,
	}
}
