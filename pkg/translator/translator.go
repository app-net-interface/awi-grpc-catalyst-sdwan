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

package translator

import (
	"awi-grpc-catalyst-sdwan/pkg/db"

	awi "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/awi-infra-guard/types"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection/sequence"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vpn"
	v1 "k8s.io/api/core/v1"
)

func ComputeInstancesToAwiGrpcInstances(instances []types.Instance) []*awi.Instance {
	awiInstances := make([]*awi.Instance, 0, len(instances))
	for _, instance := range instances {
		awiInstances = append(awiInstances, &awi.Instance{
			ID:        instance.ID,
			Name:      instance.Name,
			PublicIP:  instance.PublicIP,
			PrivateIP: instance.PrivateIP,
			SubnetID:  instance.SubnetID,
			VPCID:     instance.VPCID,
			State:     instance.State,
			Labels:    instance.Labels,
		})
	}
	return awiInstances
}

func ComputeSubnetsToAwiGrpcSubnets(subnets []types.Subnet) []*awi.Subnet {
	awiSubnets := make([]*awi.Subnet, 0, len(subnets))
	for _, subnet := range subnets {
		awiSubnets = append(awiSubnets, &awi.Subnet{
			SubnetId:  subnet.SubnetId,
			CidrBlock: subnet.CidrBlock,
			VpcId:     subnet.VpcId,
			Zone:      subnet.Zone,
			Name:      subnet.Name,
			Labels:    subnet.Labels,
		})
	}
	return awiSubnets
}

func AwiGrpcProtocolsAndPortsToCompute(nac []*awi.NetworkAccessControl) types.ProtocolsAndPorts {
	computeProtocolsAndPorts := make(types.ProtocolsAndPorts, len(nac))
	for _, v := range nac {
		if v.GetProtocol() == "" {
			continue
		}
		if v.GetPort() == "" {
			// allow all ports
			computeProtocolsAndPorts[v.GetProtocol()] = []string{}
		} else if ports, exists := computeProtocolsAndPorts[v.GetProtocol()]; exists {
			computeProtocolsAndPorts[v.GetProtocol()] = append(ports, v.GetPort())
		} else {
			computeProtocolsAndPorts[v.GetProtocol()] = []string{v.GetPort()}
		}
	}
	return computeProtocolsAndPorts
}

func AwiGrpcProtocolsAndPortsToMap(nac []*awi.NetworkAccessControl) map[string][]string {
	m := make(map[string][]string, len(nac))
	for _, v := range nac {
		if v.GetProtocol() == "" {
			continue
		}
		if v.GetPort() == "" {
			// allow all ports
			m[v.GetProtocol()] = []string{}
		} else if ports, exists := m[v.GetProtocol()]; exists {
			m[v.GetProtocol()] = append(ports, v.GetPort())
		} else {
			m[v.GetProtocol()] = []string{v.GetPort()}
		}
	}
	return m
}

func ProtocolsAndPortsMapToCompute(protocolsAndPort map[string][]string) types.ProtocolsAndPorts {
	m := make(types.ProtocolsAndPorts, len(protocolsAndPort))
	for k, v := range protocolsAndPort {
		m[k] = v
	}
	return m
}

func AwiGrpcProtocolsAndPortsToVmanage(nac []*awi.NetworkAccessControl) sequence.ProtocolsAndPorts {
	vManageProtocolsAndPorts := make(sequence.ProtocolsAndPorts, len(nac))
	for _, v := range nac {
		if v.GetProtocol() == "" {
			continue
		}
		if v.GetPort() == "" {
			// allow all ports
			vManageProtocolsAndPorts[v.GetProtocol()] = []string{}
		} else if ports, exists := vManageProtocolsAndPorts[v.GetProtocol()]; exists {
			vManageProtocolsAndPorts[v.GetProtocol()] = append(ports, v.GetPort())
		} else {
			vManageProtocolsAndPorts[v.GetProtocol()] = []string{v.GetPort()}
		}
	}
	return vManageProtocolsAndPorts
}

func PodsToAwiPods(clusterName string, pods []v1.Pod) []*awi.Pod {
	awiPods := make([]*awi.Pod, 0, len(pods))
	for _, pod := range pods {
		awiPods = append(awiPods, &awi.Pod{
			Cluster:   clusterName,
			Namespace: pod.GetNamespace(),
			Name:      pod.GetName(),
			Ip:        pod.Status.PodIP,
		})
	}
	return awiPods
}

func InfraPodsToAwiPods(clusterName string, pods []types.Pod) []*awi.Pod {
	awiPods := make([]*awi.Pod, 0, len(pods))
	for _, pod := range pods {
		awiPods = append(awiPods, &awi.Pod{
			Cluster:   clusterName,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Ip:        pod.Ip,
		})
	}
	return awiPods
}

func DbServicesToAwiServices(services []*db.Service) []*awi.K8SService {
	k8sServices := make([]*awi.K8SService, len(services))
	for _, svc := range services {
		k8sServices = append(k8sServices, &awi.K8SService{
			Cluster:   svc.Cluster,
			Namespace: svc.Namespace,
			Name:      svc.Name,
		})
	}
	return k8sServices
}

func K8sServicesToAwiServices(cluster string, services []*v1.Service) []*awi.K8SService {
	k8sServices := make([]*awi.K8SService, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		var ingresses []*awi.K8SService_Ingress
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			var ports []string
			for _, portStatus := range ing.Ports {
				ports = append(ports, string(portStatus.Port))
			}
			ingresses = append(ingresses, &awi.K8SService_Ingress{
				Hostname: ing.Hostname,
				IP:       ing.IP,
				Ports:    ports,
			})
		}
		k8sServices = append(k8sServices, &awi.K8SService{
			Cluster:   cluster,
			Namespace: svc.Namespace,
			Name:      svc.Name,
			Ingresses: ingresses,
		})
	}
	return k8sServices
}

func TypesServiceToAwiServices(cluster string, services []*types.K8SService) []*awi.K8SService {
	k8sServices := make([]*awi.K8SService, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		var ingresses []*awi.K8SService_Ingress
		for _, ing := range svc.Ingresses {
			ingresses = append(ingresses, &awi.K8SService_Ingress{
				Hostname: ing.Hostname,
				IP:       ing.IP,
				Ports:    ing.Ports,
			})
		}
		k8sServices = append(k8sServices, &awi.K8SService{
			Cluster:   cluster,
			Namespace: svc.Namespace,
			Name:      svc.Name,
			Ingresses: ingresses,
		})
	}
	return k8sServices
}

func VmanageVPNToNetworkDomainDB(in *vpn.Data) *db.NetworkDomainDB {
	return &db.NetworkDomainDB{
		NetworkDomain: &awi.NetworkDomainObject{
			Type:      db.ConnectionTypeVPN,
			Provider:  "Cisco SDWAN",
			Id:        in.SegmentID,
			Name:      in.SegmentName,
			AccountId: "",
			SideId:    "",
			Labels:    nil,
		},
	}
}

func InfraVPCToNetworkDomainDB(in *types.VPC) *db.NetworkDomainDB {
	return &db.NetworkDomainDB{
		NetworkDomain: &awi.NetworkDomainObject{
			Type:      db.ConnectionTypeVPC,
			Provider:  in.Provider,
			Id:        in.ID,
			Name:      in.Name,
			AccountId: in.AccountID,
			SideId:    "",
			Labels:    in.Labels,
		},
	}
}
