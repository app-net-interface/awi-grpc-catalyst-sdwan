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
	"time"

	"github.com/app-net-interface/awi-infra-guard/types"

	"awi-grpc-catalyst-sdwan/pkg/db"

	"github.com/app-net-interface/catalyst-sdwan-app-client/feature"
)

func (m *AWIConnectionController) DeleteConnection(ctx context.Context, connections []string) error {
	for _, connectionName := range connections {
		m.deletedConnection[connectionName] = struct{}{}
		if err := m.disconnectConnections(ctx, connectionName); err != nil {
			return err
		}
	}

	return nil
}

func (m *AWIConnectionController) DeleteAppConnection(ctx context.Context, aclID string) error {

	deleteFunc := func() error {
		acl, err := m.dbClient.GetAppConnection(aclID)
		if err != nil {
			return err
		}
		if acl == nil {
			m.logger.Infof("ACL %s no longer exists", aclID)
			return nil
		}
		if acl.URLFilteringID != "" {
			if err := m.deleteURLFiltering(ctx, acl.URLFilteringID); err != nil {
				return err
			}
		} else if acl.FeatureTemplateID != "" {
			if err := m.deleteACLPolicy(ctx, acl.SiteID, acl.FeatureTemplateID); err != nil {
				return err
			}
		} else if acl.ConnectionID == "" {

		} else {
			if err := m.deleteInboundRules(ctx, aclID, acl.Provider); err != nil {
				return err
			}
		}

		err = m.dbClient.DeleteAppConnection(aclID)
		if err != nil {
			return err
		}
		return nil
	}

	err := deleteFunc()
	if err != nil {
		m.logger.Errorf("Error during DeleteAppConnection: %v", err)
	} else {
		m.logger.Infof("Successfully removed AppConnection %s", aclID)
	}
	return err
}

func (m *AWIConnectionController) disconnectConnections(ctx context.Context, connection string) error {
	oldRequest, err := m.dbClient.GetConnectionRequest(connection)
	if err != nil {
		return err
	}
	if oldRequest == nil {
		m.logger.Infof("Connection no longer exists")
		return nil
	}
	if oldRequest.Status == db.StateWatching {
		return m.dbClient.DeleteConnectionRequest(connection)
	}
	if err := m.disconnectSource(ctx, connection); err != nil {
		return err
	}
	return nil
}

func (m *AWIConnectionController) disconnectSource(ctx context.Context, conn string) error {
	oldRequest, err := m.dbClient.GetConnectionRequest(conn)
	if err != nil {
		return err
	}
	if oldRequest == nil {
		m.logger.Infof("Connection no longer exists")
		return nil
	}
	if m.isControllerVManage() {
		if err := m.tagConnection(ctx, oldRequest); err != nil {
			return err
		}
	}
	normalizeNames(oldRequest)
	if m.isControllerVManage() {
		if _, err := m.updateConnectionRequest(ctx, oldRequest, nil, false); err != nil {
			return fmt.Errorf("could not delete connection: %v", err)
		}
		if oldRequest.Source.SiteID != "" {
			if err := m.deleteACLPolicy(ctx, oldRequest.Source.SiteID, oldRequest.Source.FeatureTemplateID); err != nil {
				return err
			}
		}
	}
	if m.connector == AwiConnector {
		if err := m.disconnectVPCsWithAWI(ctx, oldRequest); err != nil {
			return err
		}
	}

	if err := m.removeRulesFromSecurityGroup(ctx, oldRequest); err != nil {
		return err
	}

	if err := m.dbClient.DeleteConnectionRequest(conn); err != nil {
		return err
	}
	return nil
}

func (m *AWIConnectionController) deleteACLPolicy(ctx context.Context, siteID, featureTemplateID string) error {
	m.logger.Infof("Deleting policy for site %s", siteID)

	ft, err := m.client.Feature().Get(ctx, featureTemplateID)
	if err != nil {
		return fmt.Errorf("could not get feature template: %v", err)
	}
	addAccessListToFeatureTemplate(ft)
	update, err := m.client.Feature().Update(ctx, featureTemplateID, ft)
	if err != nil {
		return fmt.Errorf("could not update feature template: %v", err)
	}
	for _, masterTemplateID := range update.MasterTemplatesAffected {
		attachedDevices, err := m.client.Device().GetAttachedDevices(ctx, masterTemplateID)
		if err != nil {
			return err
		}
		if len(attachedDevices) == 0 {
			continue
		}
		id, err := m.client.Device().PushConfiguration(ctx, attachedDevices, masterTemplateID)
		if err != nil {
			return fmt.Errorf("could not push configuration: %v", err)
		}
		if err := m.client.Status().ActionStatusLongPoll(ctx, id); err != nil {
			return err
		}
	}

	deviceTemplate, err := m.client.Device().GetFromSite(ctx, siteID)
	if err != nil {
		return err
	}
	policyID := deviceTemplate.PolicyID
	deviceTemplate.PolicyID = ""

	m.clock.Sleep(10 * time.Second) // FIXME previous operation still in progress despite "success" returned
	response, err := m.client.Device().Update(ctx, deviceTemplate)
	if err != nil {
		return fmt.Errorf("could not update template policy: %v", err)
	}

	if len(response.AttachedDevices) != 0 {
		id, err := m.client.Device().PushConfiguration(ctx, response.AttachedDevices, deviceTemplate.TemplateID)
		if err != nil {
			return fmt.Errorf("could not push configuration: %v", err)
		}
		if err := m.client.Status().ActionStatusLongPoll(ctx, id); err != nil {
			return err
		}
	}

	policy, err := m.client.Policy().Get(ctx, policyID)
	if err != nil {
		return err
	}
	// FIXME: cannot delete policy immediately - vManage needs some time to notice that policy is no longer used
	m.clock.Sleep(5 * time.Second)
	if err := m.client.Policy().Delete(ctx, policyID); err != nil {
		return err
	}
	for _, assembly := range policy.PolicyDefinition.Assemblies {
		if err := m.client.ACL().Delete(ctx, assembly.DefinitionID); err != nil {
			return err
		}
	}

	return nil
}

func addAccessListToFeatureTemplate(ft *feature.Template) {
	ft.TemplateDefinition.AccessList = feature.VipPrimary[feature.AccessListValue]{
		VipObjectType: "tree",
		VipPrimaryKey: []string{"direction"},
		VipType:       "ignore",
		VipValue:      []feature.AccessListValue{},
	}
}

func (m *AWIConnectionController) deleteInboundRules(ctx context.Context, aclID, cloud string) error {
	acl, err := m.dbClient.GetAppConnection(aclID)
	if err != nil {
		return err
	}
	if acl.ConnectionID == AllNetworkDomainConnectionsIdentifier {
		return nil
	}
	if acl.Destination.Service != nil {
		return m.updateServiceSourceRanges(ctx, acl.Destination.Service.Cluster, acl.Destination.Service.Namespace,
			acl.Destination.Service.Name, nil, acl.Source.CIDRs)
	} else {
		provider, err := m.strategy.GetProvider(ctx, cloud)
		if err != nil {
			return err
		}
		if err := provider.RemoveInboundAllowRulesFromVPCById(ctx, "", acl.Destination.Region,
			acl.Destination.VPCID, acl.Destination.InstanceIDs, acl.Destination.LoadBalancersIDs,
			acl.SecurityGroupID); err != nil {
			return err
		}
		if err := m.dbClient.DeleteAppConnection(aclID); err != nil {
			return err
		}
	}
	return nil
}

func (m *AWIConnectionController) removeRulesFromSecurityGroup(ctx context.Context, request *db.ConnectionRequest) error {
	if !(request.Source.Type == db.ConnectionTypeVPC || request.Source.Type == db.ConnectionTypeTag) ||
		!(request.Destination.Type == db.ConnectionTypeVPC || request.Destination.Type == db.ConnectionTypeTag) {
		return nil
	}
	provider, err := m.strategy.GetProvider(ctx, request.Destination.Provider)
	if err != nil {
		return err
	}
	if request.Destination.DefaultAccess == "allow" {
		return provider.RemoveInboundAllowRuleRulesByTags(ctx, "", request.Destination.Region, request.Destination.ID, getSGName(request), awiDefaultAccessTags(request))
	}
	return nil
}

func (m *AWIConnectionController) deleteURLFiltering(ctx context.Context, id string) error {
	filtering, err := m.client.URLFiltering().Get(ctx, id)
	if err != nil {
		return err
	}
	err = m.client.URLFiltering().Delete(ctx, id)
	if err != nil {
		return err
	}
	m.logger.Infof("Deleted URLFiltering %s", filtering.Name)
	if filtering.Definition.UrlAllowlist.Ref != "" {
		err = m.client.URLAllowlist().Delete(ctx, filtering.Definition.UrlAllowlist.Ref)
		if err != nil {
			return err
		}
		m.logger.Infof("Deleted URLAllowlist with id %s", filtering.Definition.UrlAllowlist.Ref)
	}
	if filtering.Definition.UrlDenylist.Ref != "" {
		err = m.client.URLDenylist().Delete(ctx, filtering.Definition.UrlDenylist.Ref)
		if err != nil {
			return err
		}
		m.logger.Infof("Deleted URLDenylist with id %s", filtering.Definition.UrlDenylist.Ref)
	}
	return nil
}

func (m *AWIConnectionController) disconnectVPCsWithAWIMultiCloud(ctx context.Context, request *db.ConnectionRequest) error {
	firstProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
	if err != nil {
		return err
	}
	secondProvider, err := m.strategy.GetProvider(ctx, request.Destination.Provider)
	if err != nil {
		return err
	}

	_, err = firstProvider.DisconnectVPC(ctx, types.SingleVPCDisconnectionParams{
		ConnID:   request.ID,
		ConnName: request.Name,
		VpcID:    request.Source.ID,
		Region:   request.Source.Region,
	})
	if err != nil {
		return fmt.Errorf(
			"failed to disconnect VPC from Connection '%s' from Source. ID: '%s', err: %w",
			request.ID, request.Source.ID, err,
		)
	}
	_, err = secondProvider.DisconnectVPC(ctx, types.SingleVPCDisconnectionParams{
		ConnID:   request.ID,
		ConnName: request.Name,
		VpcID:    request.Destination.ID,
		Region:   request.Destination.Region,
	})
	if err != nil {
		return fmt.Errorf(
			"failed to disconnect VPC from Connection '%s' from Destination. ID: '%s', err: %w",
			request.ID, request.Destination.ID, err,
		)
	}
	return nil
}

func (m *AWIConnectionController) disconnectVPCsWithAWISingleCloud(ctx context.Context, request *db.ConnectionRequest) error {
	cloudProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
	if err != nil {
		return err
	}

	_, err = cloudProvider.DisconnectVPCs(ctx, types.VPCDisconnectionParams{
		ConnID:   request.ID,
		ConnName: request.Name,
		Vpc1ID:   request.Source.ID,
		Vpc2ID:   request.Destination.ID,
		Region1:  request.Source.Region,
		Region2:  request.Destination.Region,
	})
	return err
}

func (m *AWIConnectionController) disconnectVPCsWithAWI(ctx context.Context, request *db.ConnectionRequest) error {
	if request.Source.Provider != request.Destination.Provider {
		return m.disconnectVPCsWithAWIMultiCloud(ctx, request)
	}
	return m.disconnectVPCsWithAWISingleCloud(ctx, request)
}
