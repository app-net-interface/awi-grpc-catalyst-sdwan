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

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/boltdb/bolt"

	"github.com/app-net-interface/catalyst-sdwan-app-client/connection"
)

const (
	StateRunning  = "Running"
	StatePending  = "Pending"
	StateFailed   = "Failed"
	StateWatching = "Watching"
)

const (
	crTableName                  = "connection_requests" // -> network domain connections
	aclTableName                 = "acls"                // -> application connections
	appConnectionPolicyTableName = "app_connection_policies"
	networkSLATableName          = "network_slas"
	networkDomainTable           = "network_domains"
	accessPolicyTable            = "access_policies"
)

var (
	tableNames = [...]string{crTableName, aclTableName, networkSLATableName, appConnectionPolicyTableName,
		networkDomainTable, accessPolicyTable}
)

type Client interface {
	Open(filename string) error
	Close() error
	DropDB() error
	UpdateConnectionRequest(cr *ConnectionRequest, id string) error
	GetConnectionRequest(id string) (*ConnectionRequest, error)
	ListConnectionRequests() ([]ConnectionRequest, error)
	DeleteConnectionRequest(id string) error
	UpdateAppConnection(appConnection *AppConnection, aclID string) error
	GetAppConnection(id string) (*AppConnection, error)
	ListAppConnections() ([]AppConnection, error)
	DeleteAppConnection(id string) error
	UpdateConnectionStatus(status []*connection.Status) error
	UpdateNetworkSLA(sla *NetworkSLADB, id string) error
	GetNetworkSLA(id string) (*NetworkSLADB, error)
	ListNetworkSLAs() ([]NetworkSLADB, error)
	DeleteNetworkSLA(id string) error
	UpdateNetworkDomain(sla *NetworkDomainDB, id string) error
	GetNetworkDomain(id string) (*NetworkDomainDB, error)
	ListNetworkDomains() ([]NetworkDomainDB, error)
	DeleteNetworkDomain(id string) error
	UpdateAppConnectionPolicy(appConnection *AppConnectionPolicy, aclID string) error
	GetAppConnectionPolicy(id string) (*AppConnectionPolicy, error)
	ListAppConnectionsPolicy() ([]AppConnectionPolicy, error)
	DeleteAppConnectionPolicy(id string) error
	UpdateAccessPolicy(AccessPolicy *AccessPolicyDB, id string) error
	GetAccessPolicy(id string) (*AccessPolicyDB, error)
	ListAccessPolicies() ([]AccessPolicyDB, error)
	DeleteAccessPolicy(id string) error
}

type client struct {
	db *bolt.DB
}

func NewClient() Client {
	return &client{}
}

func (client *client) Open(filename string) error {
	options := &bolt.Options{Timeout: time.Second}
	var err error
	client.db, err = bolt.Open(filename, 0600, options)
	if err != nil {
		return err
	}

	return client.db.Update(func(tx *bolt.Tx) error {
		for _, tableName := range tableNames {
			_, err := tx.CreateBucketIfNotExists([]byte(tableName))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (client *client) Close() error {
	return client.db.Close()
}

func (client *client) DropDB() error {
	return client.db.Update(func(tx *bolt.Tx) error {
		for _, tableName := range tableNames {
			if err := tx.DeleteBucket([]byte(tableName)); err != nil {
				return err
			}
			_, err := tx.CreateBucket([]byte(tableName))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (client *client) UpdateConnectionRequest(cr *ConnectionRequest, id string) error {
	return update(client, cr, id, crTableName)
}

func (client *client) GetConnectionRequest(id string) (*ConnectionRequest, error) {
	return get[ConnectionRequest](client, id, crTableName)
}

func (client *client) ListConnectionRequests() ([]ConnectionRequest, error) {
	return list[ConnectionRequest](client, crTableName)
}

func (client *client) DeleteConnectionRequest(id string) error {
	return client.delete(id, crTableName)
}

func (client *client) UpdateAppConnection(acl *AppConnection, aclID string) error {
	t := time.Now().UTC().Format(time.RFC3339)
	if acl.Config.GetMetadata().GetCreationTimestamp() == "" {
		acl.Config.Metadata.CreationTimestamp = t
	} else {
		acl.Config.Metadata.ModificationTimestamp = t
	}

	return update(client, acl, aclID, aclTableName)
}

func (client *client) GetAppConnection(aclID string) (*AppConnection, error) {
	return get[AppConnection](client, aclID, aclTableName)
}

func (client *client) ListAppConnections() ([]AppConnection, error) {
	return list[AppConnection](client, aclTableName)
}

func (client *client) DeleteAppConnection(aclID string) error {
	return client.delete(aclID, aclTableName)
}

func (client *client) UpdateConnectionStatus(status []*connection.Status) error {
	connections, err := client.ListConnectionRequests()
	if err != nil {
		return err
	}
	connectionMap := make(map[string]*ConnectionRequest, len(connections))
	for _, s := range status {
		for _, mapped := range s.Mapped {
			connection := client.transformConnectionMapping(mapped, StateRunning)
			connectionMap[connection.ID] = connection
		}
		for _, mapped := range s.Unmapped {
			connection := client.transformConnectionMapping(mapped, StateFailed)
			connectionMap[connection.ID] = connection
		}
		for _, mapped := range s.OutstandingMapping {
			connection := client.transformConnectionMapping(mapped, StatePending)
			connectionMap[connection.ID] = connection
		}
	}

	return client.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(crTableName))
		for _, connection := range connections {
			connection := connection
			updatedConn, ok := connectionMap[connection.ID]
			if !ok {
				if err := bucket.Delete([]byte(connection.ID)); err != nil {
					return err
				}
			} else {
				connection.Status = updatedConn.Status
				connectionMap[connection.ID] = &connection
			}
		}
		for connID, connection := range connectionMap {
			t := time.Now().UTC().Format(time.RFC3339)
			if connection.CreationTimestamp == "" {
				connection.CreationTimestamp = t
			} else {
				connection.ModificationTimestamp = t
			}
			data, err := json.Marshal(connection)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(connID), data); err != nil {
				return err
			}
		}

		return nil
	})
}

func (client *client) transformConnectionMapping(status connection.StatusMapped, s string) *ConnectionRequest {
	srcID := status.SrcId
	destID := status.DestId
	id := fmt.Sprintf("%s:%s", srcID, destID)
	srcName := status.SourceTag
	if srcName == "" {
		srcName = srcID
	}
	destName := status.DestTag
	if destName == "" {
		destName = destID
	}
	srcProvider := status.CloudType
	if status.SrcType == "vpn" {
		srcProvider = "vManage"
	}
	destProvider := status.CloudType
	if status.DestType == "vpn" {
		destProvider = "vManage"
	}
	return &ConnectionRequest{
		Source: ConnectionRequestSource{
			Metadata: Metadata{
				Name: srcName,
			},
			Provider: srcProvider,
			Type:     status.SrcType,
			ID:       status.SrcId,
			Tag:      status.SourceTag,
			Region:   status.SourceRegion,
		},
		Destination: ConnectionRequestDestination{
			Metadata: Metadata{
				Name: destName,
			},
			Provider: destProvider,
			Type:     status.DestType,
			ID:       status.DestId,
			Tag:      status.DestTag,
			Region:   status.DestRegion,
		},
		ID:     id,
		Status: s,
		Name:   fmt.Sprintf("%s to %s", srcName, destName),
	}
}

func (client *client) GetNetworkSLA(id string) (*NetworkSLADB, error) {
	return get[NetworkSLADB](client, id, networkSLATableName)
}

func (client *client) UpdateNetworkSLA(sla *NetworkSLADB, id string) error {
	t := time.Now().UTC().Format(time.RFC3339)
	if sla.NetworkSLA.GetMetadata().GetCreationTimestamp() == "" {
		sla.NetworkSLA.Metadata.CreationTimestamp = t
	} else {
		sla.NetworkSLA.Metadata.ModificationTimestamp = t
	}
	return update(client, sla, id, networkSLATableName)
}

func (client *client) ListNetworkSLAs() ([]NetworkSLADB, error) {
	return list[NetworkSLADB](client, networkSLATableName)
}

func (client *client) DeleteNetworkSLA(id string) error {
	return client.delete(id, networkSLATableName)
}

func (client *client) UpdateAppConnectionPolicy(acl *AppConnectionPolicy, aclID string) error {
	t := time.Now().UTC().Format(time.RFC3339)
	if acl.Config.GetMetadata().GetCreationTimestamp() == "" {
		acl.Config.Metadata.CreationTimestamp = t
	} else {
		acl.Config.Metadata.ModificationTimestamp = t
	}
	return update(client, acl, aclID, appConnectionPolicyTableName)
}

func (client *client) GetAppConnectionPolicy(aclID string) (*AppConnectionPolicy, error) {
	return get[AppConnectionPolicy](client, aclID, appConnectionPolicyTableName)
}

func (client *client) ListAppConnectionsPolicy() ([]AppConnectionPolicy, error) {
	return list[AppConnectionPolicy](client, appConnectionPolicyTableName)
}

func (client *client) DeleteAppConnectionPolicy(aclID string) error {
	return client.delete(aclID, appConnectionPolicyTableName)
}

func (client *client) GetNetworkDomain(id string) (*NetworkDomainDB, error) {
	return get[NetworkDomainDB](client, id, networkDomainTable)
}

func (client *client) UpdateNetworkDomain(nd *NetworkDomainDB, id string) error {
	return update(client, nd, id, networkDomainTable)
}

func (client *client) ListNetworkDomains() ([]NetworkDomainDB, error) {
	return list[NetworkDomainDB](client, networkDomainTable)
}

func (client *client) DeleteNetworkDomain(id string) error {
	return client.delete(id, networkDomainTable)
}

func (client *client) GetAccessPolicy(id string) (*AccessPolicyDB, error) {
	return get[AccessPolicyDB](client, id, accessPolicyTable)
}

func (client *client) UpdateAccessPolicy(nd *AccessPolicyDB, id string) error {
	return update(client, nd, id, accessPolicyTable)
}

func (client *client) ListAccessPolicies() ([]AccessPolicyDB, error) {
	return list[AccessPolicyDB](client, accessPolicyTable)
}

func (client *client) DeleteAccessPolicy(id string) error {
	return client.delete(id, accessPolicyTable)
}

func (client *client) delete(id, tableName string) error {
	return client.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(tableName))
		return bucket.Delete([]byte(id))
	})
}

func update[T any](client *client, t T, id, tableName string) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return client.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(tableName))

		if err := bucket.Put([]byte(id), data); err != nil {
			return err
		}
		return nil
	})
}

func get[T any](client *client, id, tableName string) (*T, error) {
	var data []byte
	if err := client.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(tableName))
		data = bucket.Get([]byte(id))
		return nil
	}); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var t T
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}

	return &t, nil
}

func list[T any](client *client, tableName string) ([]T, error) {
	var ts []T
	if err := client.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(tableName))
		return bucket.ForEach(func(k, v []byte) error {
			var t T
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			ts = append(ts, t)
			return nil
		})
	}); err != nil {
		return nil, err
	}

	return ts, nil
}
