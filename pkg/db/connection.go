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

import awiGrpc "github.com/app-net-interface/awi-grpc/pb"

const (
	ConditionLabel = "condition"
	OrCondition    = "OR"
	AndCondition   = "AND"
)

const (
	ConnectionTypeVPC    = "vpc"
	ConnectionTypeVPN    = "vpn"
	ConnectionTypeTag    = "tag"
	ConnectionTypeSubnet = "subnet"
)

type ConnectionRequest struct {
	ID                    string                                 `mapstructure:"id"`
	Name                  string                                 `mapstructure:"name"`
	Type                  string                                 `mapstructure:"type"`
	Status                string                                 `mapstructure:"status"`
	Source                ConnectionRequestSource                `mapstructure:"source"`
	Destination           ConnectionRequestDestination           `mapstructure:"destinations"`
	Config                *awiGrpc.NetworkDomainConnectionConfig `mapstructure:"config"`
	Metadata              *awiGrpc.ConnectionMetadata            `mapstructure:"metadata"`
	CreationTimestamp     string                                 `mapstructure:"creation_timestamp"`
	ModificationTimestamp string                                 `mapstructure:"modification_timestamp"`
}

type Metadata struct {
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
}

type ConnectionRequestSource struct {
	ID                string   `mapstructure:"id"`
	Metadata          Metadata `mapstructure:"metadata"`
	SiteID            string   `mapstructure:"site_id"`
	FeatureTemplateID string   `mapstructure:"feature_template_id"`
	Type              string   `mapstructure:"type"`
	Provider          string   `mapstructure:"provider"`
	Networks          []string `mapstructure:"networks"`
	Tag               string   `mapstructure:"tag"`
	Region            string   `mapstructure:"region"`
}

type ConnectionRequestDestination struct {
	DbID                   string                 `mapstructure:"db_id"`
	Metadata               Metadata               `mapstructure:"metadata"`
	SiteID                 string                 `mapstructure:"site_id"`
	ID                     string                 `mapstructure:"id"`
	Type                   string                 `mapstructure:"type"`
	Provider               string                 `mapstructure:"provider"`
	RequestedConnectionSLA RequestedConnectionSLA `mapstructure:"requested_connection_sla"`
	DefaultAccess          string                 `mapstructure:"default_access"`
	Tag                    string                 `mapstructure:"tag"`
	Region                 string                 `mapstructure:"region"`
}

type RequestedConnectionSLA struct {
	Type      string `mapstructure:"type"`
	Bandwidth int    `mapstructure:"bandwidth"`
	Jitter    int    `mapstructure:"jitter"`
	Loss      int    `mapstructure:"loss"`
	Latency   int    `mapstructure:"latency"`
}
