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

	awigrpc "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/awi-infra-guard/provider"
	"github.com/app-net-interface/awi-infra-guard/types"
	corev1 "k8s.io/api/core/v1"

	"awi-grpc-catalyst-sdwan/pkg/db"
	"awi-grpc-catalyst-sdwan/pkg/translator"
)

func (m *AWIConnectionController) getSourceDataForKubernetesLabels(ctx context.Context,
	computeId string, computeType string, labels map[string]string) (*db.AppConnectionSource, error) {
	condition := labels[db.ConditionLabel]
	if condition != db.OrCondition {
		condition = db.AndCondition
	}
	var ips []string
	var err error
	var pods []types.Pod
	k8sProvider, err := m.strategy.GetKubernetesProvider()
	if err != nil {
		return nil, err
	}
	switch condition {
	case db.OrCondition:
		// using map to avoid duplicates
		podIpMap := make(map[string]struct{})
		for k, v := range labels {
			if k == db.ConditionLabel {
				continue
			}
			pods, err = k8sProvider.ListPods(ctx, computeId, map[string]string{k: v})
			if err != nil {
				return nil, err
			}
			for _, pod := range pods {
				podIpMap[pod.Ip] = struct{}{}
			}
		}
		for ip := range podIpMap {
			ips = append(ips, ip)
		}
	case db.AndCondition:
		pods, err = k8sProvider.ListPods(ctx, computeId, labels)
		if err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		ips = make([]string, 0, len(pods))
		for _, pod := range pods {
			ips = append(ips, pod.Ip)
		}
	}
	instancesData := db.AppConnectionSource{
		Compute: &db.Compute{
			Type: computeType,
			Id:   computeId,
		},
		Labels:    labels,
		Condition: condition,
		Matched: &awigrpc.MatchedResources{
			MatchedPods: translator.InfraPodsToAwiPods(computeId, pods),
		},
	}
	instancesData.CIDRs = make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip != "" {
			instancesData.CIDRs = append(instancesData.CIDRs, fmt.Sprintf("%s/32", ip))
		}
	}

	return &instancesData, nil
}

func (m *AWIConnectionController) getKubernetesNodesIPs(ctx context.Context,
	clusterId string) ([]string, error) {

	k8sProvider, err := m.strategy.GetKubernetesProvider()
	if err != nil {
		return nil, err
	}
	nodes, err := k8sProvider.ListNodes(ctx, clusterId, nil)
	ips := make([]string, 0, len(nodes))
	for _, node := range nodes {
		for _, addr := range node.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ips = append(ips, addr.Address)
			}
		}
	}
	return ips, nil
}

func (m *AWIConnectionController) getServiceInfo(ctx context.Context, name, namespace, cluster, ip string) (
	*types.K8SService, error) {

	var service *types.K8SService
	k8sProvider, err := m.strategy.GetKubernetesProvider()
	if err != nil {
		return nil, err
	}

	services, err := k8sProvider.ListServices(ctx, cluster, nil)
	if err != nil {
		return nil, err
	}
	if ip != "" {
		for _, svc := range services {
			for _, ingress := range svc.Ingresses {
				if ingress.IP == ip {
					service = &svc
					goto serviceFound
				}
			}
		}
	} else {
		for _, svc := range services {
			if svc.Name == name && svc.Namespace == namespace {
				service = &svc
				goto serviceFound
			}
		}
	}
	if service == nil {
		return nil, fmt.Errorf("couldn't find matching service")
	}

serviceFound:
	m.logger.Infof("Found matching service, name %s, namespace %s, cluster %s", service.Name, service.Namespace, service.Cluster)
	return service, nil
}

func (m *AWIConnectionController) updateServiceSourceRanges(ctx context.Context, clusterName, namespace, name string, cidrsToAdd, cidrsToRemove []string) error {
	m.logger.Infof("Adding CIDRs %v and removing CIDRs %v from service's %s/%s loadBalancerSourceRanges", cidrsToAdd, cidrsToRemove, namespace, name)
	k8sProvider, err := m.strategy.GetKubernetesProvider()
	if err != nil {
		return err
	}
	return k8sProvider.UpdateServiceSourceRanges(ctx, clusterName, namespace, name, cidrsToAdd, cidrsToRemove)
}

func (m *AWIConnectionController) findServiceMatchingToHostname(ctx context.Context, fqdn string) (*types.K8SService, error) {
	k8sProvider, err := m.strategy.GetKubernetesProvider()
	if err != nil {
		return nil, err
	}
	allServices, err := k8sProvider.ListServices(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	for _, svc := range allServices {
		for _, ing := range svc.Ingresses {
			if ing.Hostname == fqdn {
				return &svc, nil
			}
		}
	}

	return nil, fmt.Errorf("couldn't find matching service for hostname: %v", fqdn)
}

func (m *AWIConnectionController) updateInboundAllowRulesForService(ctx context.Context,
	provider provider.CloudProvider,
	sourceACLData *db.AppConnectionSource,
	appConnection *awigrpc.AppConnection,
	service *awigrpc.Service,
	securityGroupName string, destinationVpcID string, destinationRegion string,
	findOnly bool) (
	securityGroupId string, computeId string, matchedCluster string,
	matchedService *types.K8SService, dbService *db.Service,
	loadBalancersIds []string, instances []types.Instance, err error) {
	// service identifier can be provided in following forms: (TODO: at some point this design should be rethought)
	// 1a. Service provided as a FQDN of load balancer in service.selector.matchName.name  (for example "internal-a0d80264a23e54fe0ae9e8c396605471-376844005.us-west-2.elb.amazonaws.com")
	// 1b. Service provided as a service IP in service.selector.matchHost.ip together with namespace in service.selector.matchNamespace and cluster in service.selector.matchCluster
	// 1c. Service provided as a name/namespace/cluster of k8s service in service.selector.matchName.name/selector.matchNamespace/service.selector.matchCluster
	// check for case 1a
	if service.GetSelector().GetMatchName().GetName() != "" && service.GetSelector().GetMatchNamespace() == nil {
		var lbName string
		if !findOnly {
			matchingPolicies, findErr := m.findAccessPoliciesForAppConnection(appConnection)
			if err != nil {
				err = findErr
				return
			}
			lbName, securityGroupId, err = provider.AddInboundAllowRuleForLoadBalancerByDNS(
				ctx, "", destinationRegion, service.GetSelector().GetMatchName().GetName(),
				destinationVpcID, securityGroupName, sourceACLData.CIDRs,
				translator.AwiGrpcProtocolsAndPortsToCompute(accessPolicyToNetworkAccessControl(matchingPolicies)))
			if err != nil {
				m.logger.Errorf("couldn't find a service with FQDN name %s",
					service.GetSelector().GetMatchName().GetName())
				return
			}
			loadBalancersIds = append(loadBalancersIds, lbName)
		}
		matchedService, err = m.findServiceMatchingToHostname(ctx, service.GetSelector().GetMatchName().GetName())
		if err != nil {
			m.logger.Errorf("couldn't find matching service with hostname %s: %v",
				service.GetSelector().GetMatchName().GetName(), err)
		}
		return
	}

	serviceName := service.GetSelector().GetMatchName().GetName()
	serviceNamespace := service.GetSelector().GetMatchNamespace().GetName()
	clusterName := service.GetSelector().GetMatchCluster().GetName()
	serviceIP := service.GetSelector().GetMatchHost().GetIp()

	svc, gerr := m.getServiceInfo(ctx, serviceName, serviceNamespace, clusterName, serviceIP)
	if gerr != nil {
		err = fmt.Errorf("couldn't find a service with identifiers"+
			"name %s, namespace: %s, cluster: %s, IP: %s, error: %v", serviceName, serviceNamespace, clusterName, serviceIP, gerr)
		return
	}
	matchedService = svc
	matchedCluster = clusterName
	computeId = clusterName
	var ips []string
	switch svc.Type {
	case string(corev1.ServiceTypeNodePort):
		ips, err = m.getKubernetesNodesIPs(ctx, clusterName)
		if err != nil {
			return
		}
		if !findOnly {
			securityGroupId, instances, err = provider.AddInboundAllowRuleByInstanceIPMatch(ctx, "", destinationRegion, destinationVpcID,
				securityGroupName, ips, sourceACLData.CIDRs, svc.ProtocolsAndPorts)
			if err != nil {
				m.logger.Errorf("Failure during updating security groups for acl %v: %v", appConnection.GetMetadata().GetName(), err)
				return
			}
		}
		dbService = &db.Service{
			Cluster:   clusterName,
			Name:      svc.Name,
			Namespace: svc.Namespace,
		}
		return
	case string(corev1.ServiceTypeLoadBalancer):
		if provider.GetName() == "GCP" {
			if !findOnly {
				err = m.updateServiceSourceRanges(ctx, clusterName, svc.Namespace, svc.Name, sourceACLData.CIDRs, nil)
				if err != nil {
					return
				}
			}
			dbService = &db.Service{
				Cluster:   clusterName,
				Name:      svc.Name,
				Namespace: svc.Namespace,
			}
			return
		}
		if !findOnly {
			var lbName string
			for _, ingress := range svc.Ingresses {
				lbName, securityGroupId, err = provider.AddInboundAllowRuleForLoadBalancerByDNS(ctx, "", destinationRegion, ingress.Hostname,
					destinationVpcID, securityGroupName, sourceACLData.CIDRs, svc.ProtocolsAndPorts)
				if err != nil {
					return
				}
				loadBalancersIds = append(loadBalancersIds, lbName)
			}
		}
		return
	default:
		err = fmt.Errorf("unsupported service type %s", svc.Type)
		return
	}
}
