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

package db

import awi "github.com/app-net-interface/awi-grpc/pb"

type AppConnection struct {
	ID                      string
	ConnectionID            string
	Name                    string
	Provider                string
	SecurityGroupID         string
	Source                  *AppConnectionSource
	Destination             *AppConnectionDestination
	ProtocolsAndPorts       map[string][]string
	Status                  string
	RequestID               string
	FeatureTemplateID       string
	SiteID                  string
	URLFilteringID          string
	AllNetworkDomainsStatus AppConnForAllNetworkDomainStatus
	Config                  *awi.AppConnection
}

type AppConnForAllNetworkDomainStatus struct {
	Counter int
}

type AppConnectionSource struct {
	Compute   *Compute
	VPCID     string
	Region    string
	CIDRs     []string
	Labels    map[string]string
	Condition string
	Matched   *awi.MatchedResources
}

type AppConnectionDestination struct {
	VPCID            string
	Region           string
	Compute          *Compute
	InstanceIDs      []string
	LoadBalancersIDs []string
	Service          *Service
	Labels           map[string]string
	CIDRs            []string
	Condition        string
	Matched          *awi.MatchedResources
}

type Compute struct {
	Type string
	Id   string
}

type Service struct {
	Cluster   string
	Namespace string
	Name      string
}

type NetworkSLADB struct {
	NetworkSLA *awi.NetworkSLA
	Status     string
}

type AppConnectionPolicy struct {
	ID     string
	Config *awi.AppConnection
}

type NetworkDomainDB struct {
	NetworkDomain *awi.NetworkDomainObject
}

type AccessPolicyDB struct {
	AccessPolicy *awi.Security_AccessPolicy
}
