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
	"awi-grpc-catalyst-sdwan/pkg/db"
	"awi-grpc-catalyst-sdwan/pkg/translator"
	"context"
	"fmt"
	"strings"

	awigrpc "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/awi-infra-guard/grpc/go/infrapb"
	"github.com/app-net-interface/awi-infra-guard/provider"
	"github.com/app-net-interface/awi-infra-guard/types"
)

func (m *AWIConnectionController) updateCloudAccessControl(ctx context.Context, id string, acl *awigrpc.AppConnection,
	request *db.ConnectionRequest) error {
	destCloudProvider, err := m.strategy.GetProvider(ctx, request.Destination.Provider)
	if err != nil {
		return err
	}
	sourceCloudProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
	if err != nil {
		return err
	}
	aclDB, err := m.updateSecurityGroupsInProvider(ctx, destCloudProvider, sourceCloudProvider, acl, request)
	if err != nil {
		return err
	}
	aclDB.ID = id
	aclDB.RequestID = request.ID
	if err := m.dbClient.UpdateAppConnection(aclDB, aclDB.ID); err != nil {
		return err
	}
	return nil
}

func (m *AWIConnectionController) updateSecurityGroupsInProvider(ctx context.Context,
	provider provider.CloudProvider, sourceProvider provider.CloudProvider,
	acl *awigrpc.AppConnection,
	request *db.ConnectionRequest) (*db.AppConnection, error) {
	sourceACLData, err := m.getSourceACLData(ctx, sourceProvider, acl.GetFrom(), request.Source.Tag, request.Source.ID, request.Source.Region)
	if err != nil {
		return nil, err
	}
	m.logger.Infof("Source IPs found for AppConnection %s: %v",
		acl.GetMetadata().GetName(), sourceACLData.CIDRs)

	destinationACLData, securityGroupId, err := m.updateSecurityGroupsInDestination(ctx, provider, acl, request, sourceACLData)
	if err != nil {
		return nil, err
	}
	matchingPolicies, err := m.findAccessPoliciesForAppConnection(acl)
	if err != nil {
		return nil, err
	}
	nac := accessPolicyToNetworkAccessControl(matchingPolicies)
	aclDB := &db.AppConnection{
		ID:                securityGroupId,
		ConnectionID:      request.ID,
		Name:              acl.GetMetadata().GetName(),
		Provider:          provider.GetName(),
		SecurityGroupID:   securityGroupId,
		Source:            sourceACLData,
		Destination:       destinationACLData,
		ProtocolsAndPorts: translator.AwiGrpcProtocolsAndPortsToMap(nac),
		Status:            "SUCCESS",
		Config:            acl,
	}
	return aclDB, nil
}

func (m *AWIConnectionController) getSourceACLData(ctx context.Context, cloudProvider provider.CloudProvider,
	from *awigrpc.From, sourceTag, sourceID, sourceRegion string) (instancesData *db.AppConnectionSource, err error) {

	labels := getFromLabels(from)
	prefixes := from.GetSubnet().GetSelector().GetMatchPrefix()

	vpcID := sourceID
	region := sourceRegion
	if vpcID == "" || region == "" {
		vpcID, region, err = m.getVPCIdByTag(ctx, cloudProvider.GetName(), sourceTag)
		if err != nil {
			return nil, err
		}
	}
	switch {
	case len(labels) == 0 && len(prefixes) == 0:
		return nil, fmt.Errorf("either labels or prefix should be set for source")
	case len(labels) > 0: // use labels
		if strings.ToLower(from.GetEndpoint().GetKind()) == "pod" {
			if from.GetEndpoint().GetSelector().GetMatchCluster().GetName() == "" {
				return nil, fmt.Errorf("matchCluster must be provided for endpoint type POD")
			}
			return m.getSourceDataForKubernetesLabels(ctx,
				from.GetEndpoint().GetSelector().GetMatchCluster().GetName(), computeTypeKubernetes, labels)
		}
		return m.getSourceDataForLabels(ctx, cloudProvider, region, labels, vpcID)
	case len(prefixes) > 0: // use prefixes
		subnets, err := m.getSubnetsByPrefixes(ctx, cloudProvider.GetName(), region, vpcID, prefixes)
		if err != nil {
			return nil, err
		}
		instances, err := m.getInstancesBySubnets(ctx, cloudProvider.GetName(), region, vpcID, subnets)
		if err != nil {
			return nil, err
		}
		return &db.AppConnectionSource{
			Region: region,
			VPCID:  vpcID,
			CIDRs:  prefixes,
			Matched: &awigrpc.MatchedResources{
				MatchedSubnets:   translator.ComputeSubnetsToAwiGrpcSubnets(subnets),
				MatchedInstances: translator.ComputeInstancesToAwiGrpcInstances(instances),
			},
		}, nil
	}
	return nil, fmt.Errorf("something went wrong in getSourceACLData")
}

func (m *AWIConnectionController) getSourceDataForLabels(ctx context.Context,
	cloudProvider provider.CloudProvider, region string, labels map[string]string, vpcID string) (*db.AppConnectionSource, error) {

	instances, err := cloudProvider.GetInstancesForLabels(ctx, &infrapb.GetInstancesForLabelsRequest{
		VpcId:  vpcID,
		Labels: labels,
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	ipsMap := map[string]struct{}{}
	for _, instance := range instances {
		ipsMap[instance.PrivateIP] = struct{}{}
	}
	ips := make([]string, 0, len(ipsMap))
	for ip := range ipsMap {
		ips = append(ips, ip)
	}
	condition := labels[db.ConditionLabel]
	if condition != db.OrCondition {
		condition = db.AndCondition
	}

	instancesData := db.AppConnectionSource{
		Region:    region,
		VPCID:     vpcID,
		Labels:    labels,
		Condition: condition,
		Matched: &awigrpc.MatchedResources{
			MatchedInstances: translator.ComputeInstancesToAwiGrpcInstances(instances),
		},
	}
	instancesData.CIDRs = make([]string, 0, len(ips))
	for _, ip := range ips {
		instancesData.CIDRs = append(instancesData.CIDRs, fmt.Sprintf("%s/32", ip))
	}
	return &instancesData, nil
}

// TODO refactor to use have a common function with getSourceData
func (m *AWIConnectionController) getDestinationMatchedData(ctx context.Context, cloudProvider provider.CloudProvider,
	to *awigrpc.To, destTag, destID, destRegion string) (matchedResources *awigrpc.MatchedResources, err error) {

	labels := getToLabels(to)
	prefixes := to.GetSubnet().GetSelector().GetMatchPrefix()
	service := to.GetService()
	vpcID := destID
	region := destRegion
	if vpcID == "" || region == "" {
		vpcID, region, err = m.getVPCIdByTag(ctx, cloudProvider.GetName(), destTag)
		if err != nil {
			return nil, err
		}
	}
	switch {
	case len(labels) == 0 && len(prefixes) == 0 && service == nil:
		return nil, fmt.Errorf("either labels, prefix or service should be set for destination")
	case len(labels) > 0: // use labels
		if strings.ToLower(to.GetEndpoint().GetKind()) == "pod" {
			if to.GetEndpoint().GetSelector().GetMatchCluster().GetName() == "" {
				return nil, fmt.Errorf("matchCluster must be provided for endpoint type POD")
			}
			data, err := m.getSourceDataForKubernetesLabels(ctx,
				to.GetEndpoint().GetSelector().GetMatchCluster().GetName(), computeTypeKubernetes, labels)
			if err != nil {
				return nil, err
			}
			matchedResources = data.Matched
		}

		data, err := m.getSourceDataForLabels(ctx, cloudProvider, region, labels, vpcID)
		if err != nil {
			return nil, err
		}
		matchedResources = data.Matched
	case len(prefixes) > 0: // use prefixes

		subnets, err := m.getSubnetsByPrefixes(ctx, cloudProvider.GetName(), region, vpcID, prefixes)
		if err != nil {
			return nil, err
		}
		instances, err := m.getInstancesBySubnets(ctx, cloudProvider.GetName(), region, vpcID, subnets)
		if err != nil {
			return nil, err
		}
		matchedResources = &awigrpc.MatchedResources{
			MatchedSubnets:   translator.ComputeSubnetsToAwiGrpcSubnets(subnets),
			MatchedInstances: translator.ComputeInstancesToAwiGrpcInstances(instances),
		}
	case service != nil:
		m.logger.Warnf("couldn't find region for tag %s", region)
		_, _, matchedCluster, matchedService, _, _, _, err := m.updateInboundAllowRulesForService(ctx, cloudProvider, nil, nil, service, "", "", region, true)
		if err != nil {
			return nil, err
		}
		matchedResources = &awigrpc.MatchedResources{
			MatchedServices: translator.TypesServiceToAwiServices(matchedCluster, []*types.K8SService{matchedService}),
		}
	}
	return
}

func (m *AWIConnectionController) updateSecurityGroupsInDestination(ctx context.Context,
	provider provider.CloudProvider,
	acl *awigrpc.AppConnection,
	request *db.ConnectionRequest,
	sourceACLData *db.AppConnectionSource) (instancesData *db.AppConnectionDestination, securityGroupId string, err error) {
	destinationVpcID := request.Destination.ID
	region := request.Destination.Region
	if destinationVpcID == "" || region == "" {
		if request.Destination.Tag == "" {
			m.logger.Infof("%s not found", acl.GetNetworkDomainConnection().GetSelector().GetMatchName())
			return
		}
		destinationVpcID, region, err = m.getVPCIdByTag(ctx, provider.GetName(), request.Destination.Tag)
		if err != nil {
			return
		}
	}

	matchingPolicies, err := m.findAccessPoliciesForAppConnection(acl)
	if err != nil {
		return nil, "", err
	}
	nac := accessPolicyToNetworkAccessControl(matchingPolicies)

	securityGroupName := fmt.Sprintf("awi-rule-%s", acl.GetMetadata().GetName())
	var instances []types.Instance
	var subnets []types.Subnet
	var loadBalancersIds []string
	var computeId string
	var dbService *db.Service
	var matchedService *types.K8SService
	var matchedCluster string

	labels := getToLabels(acl.GetTo())
	prefixes := acl.GetTo().GetSubnet().GetSelector().GetMatchPrefix()
	service := acl.GetTo().GetService()

	switch {
	case service != nil && service.GetKind().GetK8SService() != nil: // case 1: Destination is a service
		securityGroupId, computeId, matchedCluster, matchedService, dbService,
			loadBalancersIds, instances, err = m.updateInboundAllowRulesForService(ctx,
			provider, sourceACLData, acl, service, securityGroupName, destinationVpcID, region, false)
		if err != nil {
			m.logger.Errorf("Failure during updating security groups for service for acl %v: %v", acl.GetMetadata().GetName(), err)
			return nil, "", err
		}
	case len(labels) > 0: // case 2: Destination in format of labels match (VM instances matching to labels)
		securityGroupId, instances, err = provider.AddInboundAllowRuleByLabelsMatch(ctx, "", region, destinationVpcID,
			securityGroupName, labels, sourceACLData.CIDRs,
			translator.AwiGrpcProtocolsAndPortsToCompute(nac))
		if err != nil {
			m.logger.Errorf("Failure during updating security groups for labels for acl %v: %v", acl.GetMetadata().GetName(), err)
			return nil, "", err
		}
	case len(prefixes) > 0: // case 3: Destination in format of subnet prefixes (looking for VMs which belong to subnets which have given prefixes)
		securityGroupId, instances, subnets, err = provider.AddInboundAllowRuleBySubnetMatch(ctx, "", region,
			destinationVpcID, securityGroupName, prefixes, sourceACLData.CIDRs,
			translator.AwiGrpcProtocolsAndPortsToCompute(nac))
		if err != nil {
			m.logger.Errorf("Failure during updating security groups for prefixes for acl %v: %v", acl.GetMetadata().GetName(), err)
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("couldn't determine what kind of destination it is")
	}

	m.logger.Infof("Created security group %s", securityGroupId)
	var instancesIds []string
	for _, v := range instances {
		instancesIds = append(instancesIds, v.ID)
	}
	return &db.AppConnectionDestination{
		VPCID:            destinationVpcID,
		Region:           region,
		InstanceIDs:      instancesIds,
		LoadBalancersIDs: loadBalancersIds,
		Condition:        labels[db.ConditionLabel],
		Labels:           labels,
		CIDRs:            prefixes,
		Compute: &db.Compute{
			Id: computeId,
		},
		Service: dbService,
		Matched: &awigrpc.MatchedResources{
			MatchedInstances: translator.ComputeInstancesToAwiGrpcInstances(instances),
			MatchedSubnets:   translator.ComputeSubnetsToAwiGrpcSubnets(subnets),
			MatchedServices:  translator.TypesServiceToAwiServices(matchedCluster, []*types.K8SService{matchedService}),
		},
	}, securityGroupId, nil
}

func (m *AWIConnectionController) refreshSecurityGroups(ctx context.Context,
	cloudProvider provider.CloudProvider, acl *db.AppConnection) error {
	m.logger.Infof("Periodic refresh of ACL %s", acl.Name)
	var err error
	var ipsToAdd []string
	var ipsToRemove []string
	newACLSource := acl.Source
	if len(acl.Source.Labels) > 0 {
		if acl.Source.Compute != nil && acl.Source.Compute.Type == computeTypeKubernetes {
			newACLSource, err = m.getSourceDataForKubernetesLabels(ctx, acl.Source.Compute.Id,
				acl.Source.Compute.Type, acl.Source.Labels)
			if err != nil {
				return err
			}
		} else {
			newACLSource, err = m.getSourceDataForLabels(ctx, cloudProvider, acl.Source.Region, acl.Source.Labels, acl.Source.VPCID)
			if err != nil {
				return err
			}
		}
	}

	for _, ip := range newACLSource.CIDRs {
		found := false
		for _, dbIP := range acl.Source.CIDRs {
			if dbIP == ip {
				found = true
				break
			}
		}
		if found {
			continue
		}
		ipsToAdd = append(ipsToAdd, ip)
	}

	for _, dbIP := range acl.Source.CIDRs {
		found := false
		for _, ip := range newACLSource.CIDRs {
			if ip == dbIP {
				found = true
				break
			}
		}
		if found {
			continue
		}
		ipsToRemove = append(ipsToRemove, dbIP)
	}

	m.logger.Infof("ACL Refresh: old Source CIDRs: %v, new Source CIDRs: %v", acl.Source.CIDRs, newACLSource.CIDRs)
	acl.Source.CIDRs = newACLSource.CIDRs
	if newACLSource.Matched != nil {
		acl.Source.Matched = newACLSource.Matched
	}

	if acl.Destination.Service != nil && strings.ToLower(acl.Provider) == "gcp" {
		if len(ipsToAdd) == 0 && len(ipsToRemove) == 0 {
			return nil
		}
		return m.updateServiceSourceRanges(ctx, acl.Destination.Service.Cluster, acl.Destination.Service.Namespace, acl.Destination.Service.Name, ipsToAdd, ipsToRemove)
	} else {
		instances, subnets, err := cloudProvider.RefreshInboundAllowRule(ctx, "", acl.Destination.Region, acl.SecurityGroupID, ipsToAdd, ipsToRemove,
			acl.Destination.Labels, acl.Destination.CIDRs, acl.Destination.VPCID,
			translator.ProtocolsAndPortsMapToCompute(acl.ProtocolsAndPorts))
		if err != nil {
			return err
		}
		var instancesIDs []string
		for _, v := range instances {
			instancesIDs = append(instancesIDs, v.ID)
		}
		// for now don't update destination if it is a kubernetes service
		if acl.Destination.Compute == nil || acl.Destination.Compute.Type != computeTypeKubernetes {
			acl.Destination.InstanceIDs = instancesIDs
			acl.Destination.Matched = &awigrpc.MatchedResources{
				MatchedInstances: translator.ComputeInstancesToAwiGrpcInstances(instances),
				MatchedSubnets:   translator.ComputeSubnetsToAwiGrpcSubnets(subnets),
			}
		}
	}

	if err := m.dbClient.UpdateAppConnection(acl, acl.ID); err != nil {
		return err
	}

	return nil
}
