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

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"

	"github.com/app-net-interface/awi-infra-guard/grpc/go/infrapb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/docker/docker.v20/pkg/namesgenerator"

	connection_controller "awi-grpc-catalyst-sdwan/pkg/connection-controller"
	"awi-grpc-catalyst-sdwan/pkg/db"
	"awi-grpc-catalyst-sdwan/pkg/manager"
	"awi-grpc-catalyst-sdwan/pkg/translator"

	awiGrpc "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/awi-infra-guard/provider"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vmanage"
)

type Server struct {
	awiGrpc.UnimplementedConnectionControllerServer
	awiGrpc.UnimplementedCloudServer
	awiGrpc.UnimplementedAppConnectionControllerServer
	awiGrpc.UnimplementedNetworkSLAServiceServer
	awiGrpc.UnimplementedSecurityPolicyServiceServer
	logger   *logrus.Logger
	db       db.Client
	mgr      *manager.Manager
	strategy provider.Strategy
}

const (
	configFlag                 = "config"
	globalsFlag                = "Globals"
	hostFlag                   = globalsFlag + ".hostname"
	portFlag                   = globalsFlag + ".port"
	logFileFlag                = globalsFlag + ".log_file"
	logLevelFlag               = globalsFlag + ".log_level"
	dbFileName                 = globalsFlag + ".db_name"
	kubeConfigFileName         = globalsFlag + ".kube_config_file"
	controllersFlag            = "controllers.sdwan"
	sessionIDFlag              = controllersFlag + ".session_id"
	tokenFlag                  = controllersFlag + ".token"
	urlFlag                    = controllersFlag + ".url"
	secureConnectionFlag       = controllersFlag + ".secure_connection"
	longPollRetriesFlag        = controllersFlag + ".controller_connection_retries"
	retriesIntervalFlag        = controllersFlag + ".retries_interval"
	networkDomainConnectorFlag = globalsFlag + ".network_domain_connector"
)

func NewServer(mgr *manager.Manager, dbClient db.Client, logger *logrus.Logger, providerStrategy provider.Strategy) *Server {
	return &Server{
		logger:   logger,
		db:       dbClient,
		mgr:      mgr,
		strategy: providerStrategy,
	}
}

func Run() {
	configPath := "config.yaml"
	if err := initConfig(configPath); err != nil {
		fmt.Printf("could not initialize config: %v", err)
	}

	logger := initLogger()
	client, err := getClient(logger)
	if err != nil {
		logger.Errorf("could not create client: %v", err)
	}

	dbClient := db.NewClient()
	if err := dbClient.Open(viper.GetString(dbFileName)); err != nil {
		logger.Errorf("could not opend db: %v", err)
		return
	}
	defer dbClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	providerStrategy := provider.NewRealProviderStrategy(ctx, logger, viper.GetString(kubeConfigFileName))

	ndConnector := connection_controller.VManageConnector
	if strings.ToLower(viper.GetString(networkDomainConnectorFlag)) == "awi" {
		ndConnector = connection_controller.AwiConnector
	}
	logger.Infof("Using %s as Network Domain connector.", ndConnector)

	mgr := manager.NewManager(client, dbClient, logger, providerStrategy, ndConnector)
	s := NewServer(mgr, dbClient, logger, providerStrategy)

	go mgr.RunMapping(ctx)
	go mgr.RunNetworkDomainDiscovery(ctx)
	go mgr.ProcessQueue(ctx)
	go mgr.Refresh(ctx)

	hostname := viper.GetString(hostFlag)
	port := viper.GetString(portFlag)
	if port == "" {
		port = "50051"
	}
	lis, err := net.Listen("tcp", hostname+":"+port)
	if err != nil {
		logger.Fatalf("failed to listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	awiGrpc.RegisterConnectionControllerServer(grpcServer, s)
	awiGrpc.RegisterCloudServer(grpcServer, s)
	awiGrpc.RegisterAppConnectionControllerServer(grpcServer, s)
	awiGrpc.RegisterNetworkSLAServiceServer(grpcServer, s)
	awiGrpc.RegisterSecurityPolicyServiceServer(grpcServer, s)
	logger.Infof("server listening at %v", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatalf("failed to serve: %v", err)
	}
	cancel()
}

func initLogger() *logrus.Logger {
	logger := logrus.New()
	logger.Formatter = &logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	}
	if viper.GetString(logLevelFlag) != "" {
		lvl, err := logrus.ParseLevel(viper.GetString(logLevelFlag))
		if err != nil {
			logger.Errorf("Failed to parse log level: %v", err)
		} else {
			logger.Level = lvl
		}
	}
	if viper.GetString(logFileFlag) != "" {
		f, err := os.OpenFile(viper.GetString(logFileFlag), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			fmt.Printf("error opening file: %v", err)
		} else {
			logger.Out = f
		}
	}
	return logger
}

func (s *Server) Connect(ctx context.Context, csr *awiGrpc.ConnectionRequest) (*awiGrpc.ConnectionResponse, error) {
	s.logger.Infof("Connect %v", csr)
	if err := s.validateConnectionRequest(csr); err != nil {
		s.logger.Errorf("validation error: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	id := fmt.Sprintf("%s:%s",
		csr.GetSpec().GetSource().GetNetworkDomain().GetSelector().GetMatchId().GetId(),
		csr.GetSpec().GetDestination().GetNetworkDomain().GetSelector().GetMatchId().GetId())
	req, err := s.db.GetConnectionRequest(id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if req != nil {
		return &awiGrpc.ConnectionResponse{
			Status: getRequestStatus(req),
		}, nil
	}
	s.mgr.AddConnectionRequest(ctx, csr)

	return &awiGrpc.ConnectionResponse{
		Status: awiGrpc.Status_IN_PROGRESS,
	}, nil

}

func (s *Server) Disconnect(ctx context.Context, csr *awiGrpc.DisconnectRequest) (*awiGrpc.DisconnectResponse, error) {
	s.logger.Infof("Disconnect %v", csr)
	if err := s.validateDisconnectionRequest(csr); err != nil {
		s.logger.Errorf("validation error: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	req, err := s.db.GetConnectionRequest(csr.ConnectionId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if req != nil {
		s.mgr.AddDisconnectRequest(ctx, csr)
		status := awiGrpc.Status_FAILED
		if req.Status == db.StateRunning || req.Status == db.StatePending || req.Status == db.StateWatching {
			status = awiGrpc.Status_IN_PROGRESS
		}
		return &awiGrpc.DisconnectResponse{
			Status: status,
		}, nil
	}

	return &awiGrpc.DisconnectResponse{
		Status: awiGrpc.Status_SUCCESS,
	}, nil

}

func (s *Server) GetConnection(ctx context.Context, gsr *awiGrpc.GetConnectionRequest) (*awiGrpc.ConnectionResponse, error) {
	if err := s.validateGetConnectionRequest(gsr); err != nil {
		s.logger.Errorf("validation error: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	req, err := s.db.GetConnectionRequest(gsr.ConnectionId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &awiGrpc.ConnectionResponse{
		Status: getRequestStatus(req),
	}, nil
}

func (s *Server) ListConnections(ctx context.Context, lc *awiGrpc.ListConnectionsRequest) (*awiGrpc.ListConnectionsResponse, error) {
	if err := s.validateListConnectionRequest(lc); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	requests, err := s.db.ListConnectionRequests()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	responses := make([]*awiGrpc.ConnectionInformation, 0, len(requests))
	for _, request := range requests {
		connInfo := &awiGrpc.ConnectionInformation{
			Status:   getRequestStatus(&request),
			Id:       request.ID,
			Metadata: request.Metadata,
			Config:   request.Config,
			Source: &awiGrpc.NetworkDomainObject{
				Type:      strings.ToUpper(request.Source.Type),
				Provider:  strings.ToUpper(request.Source.Provider),
				Id:        request.Source.ID,
				Name:      request.Source.Metadata.Name,
				AccountId: "", //TODO
				SideId:    request.Source.SiteID,
				Labels:    nil, //TODO
			},
			Destination: &awiGrpc.NetworkDomainObject{
				Type:      strings.ToUpper(request.Destination.Type),
				Provider:  strings.ToUpper(request.Destination.Provider),
				Id:        request.Destination.ID,
				Name:      request.Destination.Metadata.Name,
				AccountId: "", //TODO
				SideId:    request.Destination.SiteID,
				Labels:    nil, //TODO
			},
			CreationTimestamp:     request.CreationTimestamp,
			ModificationTimestamp: request.ModificationTimestamp,
		}
		if connInfo.GetMetadata().GetName() == "" {
			if connInfo.Metadata == nil {
				connInfo.Metadata = &awiGrpc.ConnectionMetadata{}
			}
			connInfo.Metadata.Name = request.Name
		}
		if connInfo.Source.Type == "TAG" {
			connInfo.Source.Type = "VPC"
		}
		if connInfo.Destination.Type == "TAG" {
			connInfo.Destination.Type = "VPC"
		}
		responses = append(responses, connInfo)
	}
	return &awiGrpc.ListConnectionsResponse{
		Connections: responses,
	}, nil
}

func (s *Server) GetConnectionStatus(ctx context.Context, gcs *awiGrpc.ConnectionStatusRequest) (*awiGrpc.ConnectionStatusResponse, error) {
	if err := s.validateConnectionStatusRequest(gcs); err != nil {
		s.logger.Errorf("validation error: %v", err)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	req, err := s.db.GetConnectionRequest(gcs.ConnectionId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &awiGrpc.ConnectionStatusResponse{
		ConnectionStatus: getRequestStatus(req),
	}, nil
}

func (s *Server) ListInstances(ctx context.Context, in *awiGrpc.ListInstancesRequest) (*awiGrpc.ListInstancesResponse, error) {
	provider, err := s.strategy.GetProvider(ctx, in.Provider)
	if err != nil {
		return nil, err
	}
	instances, err := provider.ListInstances(ctx, &infrapb.ListInstancesRequest{
		VpcId:  in.Vpc,
		Zone:   in.Zone,
		Labels: in.Labels,
		Region: in.Region,
	})
	if err != nil {
		return nil, err
	}

	return &awiGrpc.ListInstancesResponse{
		Instances: translator.ComputeInstancesToAwiGrpcInstances(instances),
	}, nil
}

func (s *Server) ListSubnets(ctx context.Context, in *awiGrpc.ListSubnetRequest) (*awiGrpc.ListSubnetResponse, error) {
	provider, err := s.strategy.GetProvider(ctx, in.Provider)
	if err != nil {
		return nil, err
	}
	subnets, err := provider.ListSubnets(ctx, &infrapb.ListSubnetsRequest{
		VpcId:  in.VPCID,
		Zone:   in.Zone,
		Cidr:   in.CIDR,
		Labels: in.Labels,
		Region: in.Region,
	})
	if err != nil {
		return nil, err
	}
	return &awiGrpc.ListSubnetResponse{
		Subnets: translator.ComputeSubnetsToAwiGrpcSubnets(subnets),
	}, nil
}

func (s *Server) ListSites(ctx context.Context, in *awiGrpc.ListSiteRequest) (*awiGrpc.ListSiteResponse, error) {
	return s.mgr.ListSites(ctx)
}

func (s *Server) ListVPCs(ctx context.Context, in *awiGrpc.ListVPCRequest) (*awiGrpc.ListVPCResponse, error) {
	if s.mgr.IsConnectionControllerVManage() {
		return s.mgr.ListVPCs(ctx, in)
	}
	provider, err := s.strategy.GetProvider(ctx, in.Provider)
	if err != nil {
		return nil, err
	}
	vpcs, err := provider.ListVPC(ctx, &infrapb.ListVPCRequest{
		AccountId: in.AccountIDs,
		Provider:  in.Provider,
		Region:    in.Region,
	})
	if err != nil {
		return nil, err
	}
	return &awiGrpc.ListVPCResponse{
		VPCs: translator.ComputeVPCsToAwiGrpcVPCs(vpcs),
	}, nil
}

func (s *Server) ListVPCTags(ctx context.Context, in *awiGrpc.ListVPCTagRequest) (*awiGrpc.ListVPCResponse, error) {
	return s.mgr.ListVPCTags(ctx, in)
}

func (s *Server) ListVPNs(ctx context.Context, in *awiGrpc.ListVPNRequest) (*awiGrpc.ListVPNResponse, error) {
	return s.mgr.ListVPNs(ctx, in)
}

func (s *Server) ConnectApps(ctx context.Context, request *awiGrpc.AppConnection) (*awiGrpc.AppConnectionResponse, error) {
	s.logger.Infof("App connection request: %v", request)
	acl, err := s.db.GetAppConnection(request.GetMetadata().GetName())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if acl != nil {
		return &awiGrpc.AppConnectionResponse{
			Status: getAppRequestStatus(acl),
		}, nil
	}
	id := ""
	if request.GetNetworkDomainConnection().GetSelector().GetMatchName() != connection_controller.AllNetworkDomainConnectionsIdentifier {
		id = namesgenerator.GetRandomName(0)
		ctx = context.WithValue(ctx, "id", id)
	}
	s.mgr.AddAppConnectionRequest(ctx, request)

	return &awiGrpc.AppConnectionResponse{
		Status:    awiGrpc.Status_IN_PROGRESS,
		AppConnId: id,
	}, nil
}

func (s *Server) DisconnectApps(ctx context.Context, request *awiGrpc.AppDisconnectionRequest) (*awiGrpc.AppDisconnectionResponse, error) {
	s.logger.Infof("App disconnect request: %v", request)
	acl, err := s.db.GetAppConnection(request.ConnectionId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if acl == nil {
		return &awiGrpc.AppDisconnectionResponse{
			Status: awiGrpc.Status_SUCCESS,
		}, nil
	}
	s.mgr.AddAppDisconnectRequest(ctx, request)

	return &awiGrpc.AppDisconnectionResponse{
		Status: awiGrpc.Status_IN_PROGRESS,
	}, nil
}

func (s *Server) GetAppConnection(ctx context.Context, request *awiGrpc.GetAppConnectionRequest) (*awiGrpc.GetAppConnectionResponse, error) {
	appConnDb, err := s.db.GetAppConnection(request.GetConnectionId())
	if err != nil {
		return nil, err
	}
	if appConnDb == nil {
		return nil, fmt.Errorf("no such app-connection")
	}
	networkDomainConnections, err := s.db.ListConnectionRequests()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var ndName string
	if appConnDb.ConnectionID != "" {
		for _, nd := range networkDomainConnections {
			if nd.ID == appConnDb.ConnectionID {
				ndName = nd.Name
				break
			}
		}
	}
	var sourceMatched *awiGrpc.MatchedResources
	if appConnDb.Source != nil {
		sourceMatched = appConnDb.Source.Matched
	}
	var destinationMatched *awiGrpc.MatchedResources
	if appConnDb.Destination != nil {
		destinationMatched = appConnDb.Destination.Matched
	}
	return &awiGrpc.GetAppConnectionResponse{
		AppConnection: &awiGrpc.AppConnectionInformation{
			Id:                          appConnDb.ID,
			AppConnectionConfig:         appConnDb.Config,
			Status:                      getAppRequestStatus(appConnDb),
			NetworkDomainConnectionName: ndName,
			SourceMatched:               sourceMatched,
			DestinationMatched:          destinationMatched,
		},
	}, nil
}

func (s *Server) ListConnectedApps(ctx context.Context, request *awiGrpc.ListAppConnectionsRequest) (*awiGrpc.ListAppConnectionsResponse, error) {
	if err := s.validateListAppConnectionRequest(request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	appConns, err := s.db.ListAppConnections()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	networkDomainConnections, err := s.db.ListConnectionRequests()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	appConnections := make([]*awiGrpc.AppConnectionInformation, 0, len(appConns))
	for _, conn := range appConns {
		var ndName string
		if conn.ConnectionID == connection_controller.AllNetworkDomainConnectionsIdentifier {
			ndName = connection_controller.AllNetworkDomainConnectionsIdentifier
		} else if conn.ConnectionID != "" {
			for _, nd := range networkDomainConnections {
				if nd.ID == conn.ConnectionID {
					ndName = nd.Name
					break
				}
			}
		}

		var sourceMatched *awiGrpc.MatchedResources
		if conn.Source != nil {
			sourceMatched = conn.Source.Matched
		}
		var destinationMatched *awiGrpc.MatchedResources
		if conn.Destination != nil {
			destinationMatched = conn.Destination.Matched
		}
		appConnections = append(appConnections, &awiGrpc.AppConnectionInformation{
			Id:                          conn.ID,
			AppConnectionConfig:         conn.Config,
			NetworkDomainConnectionName: ndName,
			Status:                      getAppRequestStatus(&conn),
			SourceMatched:               sourceMatched,
			DestinationMatched:          destinationMatched,
		})
	}
	return &awiGrpc.ListAppConnectionsResponse{
		AppConnections: appConnections,
	}, nil
}

func (s *Server) GetAppConnectionStatus(ctx context.Context, request *awiGrpc.GetAppConnectionStatusRequest) (*awiGrpc.AppConnectionStatusResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (s *Server) GetAppConnectionStatistics(ctx context.Context, request *awiGrpc.GetAppConnectionStatisticsRequest) (*awiGrpc.AppConnectionStatisticsResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (s *Server) GetAppConnectionEvents(ctx context.Context, request *awiGrpc.GetAppConnectionEventsRequest) (*awiGrpc.AppConnectionEventsResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (s *Server) CreateNetworkSLA(ctx context.Context, sla *awiGrpc.NetworkSLA) (*awiGrpc.NetworkSLACreateResponse, error) {
	if sla.GetMetadata().GetName() == "" {
		return &awiGrpc.NetworkSLACreateResponse{
			Status: awiGrpc.Status_FAILED,
		}, fmt.Errorf("metadata.name is required in NetworkSLA spec")
	}
	err := s.db.UpdateNetworkSLA(&db.NetworkSLADB{
		NetworkSLA: sla,
		Status:     awiGrpc.Status_name[int32(awiGrpc.Status_SUCCESS)],
	}, sla.GetMetadata().GetName())
	if err != nil {
		return &awiGrpc.NetworkSLACreateResponse{
			Status: awiGrpc.Status_FAILED,
		}, err
	}
	return &awiGrpc.NetworkSLACreateResponse{
		Status: awiGrpc.Status_SUCCESS,
	}, nil
}

func (s *Server) DeleteNetworkSLA(ctx context.Context, request *awiGrpc.NetworkSLADeleteRequest) (*awiGrpc.NetworkSLADeleteResponse, error) {
	return &awiGrpc.NetworkSLADeleteResponse{}, s.db.DeleteNetworkSLA(request.GetName())
}

func (s *Server) ListNetworkSLAs(ctx context.Context, reqest *awiGrpc.NetworkSLAListReqest) (*awiGrpc.NetworkSLAListResponse, error) {
	l, err := s.db.ListNetworkSLAs()
	if err != nil {
		return nil, err
	}
	nslas := make([]*awiGrpc.NetworkSLA, 0, 3)
	for _, v := range l {
		nslas = append(nslas, v.NetworkSLA)
	}
	return &awiGrpc.NetworkSLAListResponse{NetworkSLAs: nslas}, nil
}

func (s *Server) GetMatchedResources(ctx context.Context, in *awiGrpc.AppConnection) (*awiGrpc.GetMatchedResourcesResponse, error) {
	matchedSrc, matchedDest, err := s.mgr.GetMatchedResources(ctx, in)
	if err != nil {
		s.logger.Errorf("Failed to find matched resources: %v", err)
		return nil, err
	}
	return &awiGrpc.GetMatchedResourcesResponse{
		SourceMatched:      matchedSrc,
		DestinationMatched: matchedDest,
	}, nil
}

func (s *Server) CreateAppConnectionPolicy(ctx context.Context, in *awiGrpc.CreateAppConnectionPolicyRequest) (*awiGrpc.CreateAppConnectionPolicyResponse, error) {
	p := &db.AppConnectionPolicy{
		ID:     namesgenerator.GetRandomName(0),
		Config: in.GetAppConnection(),
	}
	err := s.db.UpdateAppConnectionPolicy(p, p.ID)
	if err != nil {
		s.logger.Errorf("Error during CreateAppConnectionPolicy: %v", err)
		return &awiGrpc.CreateAppConnectionPolicyResponse{
			Status: "FAILED",
		}, err
	}
	s.logger.Infof("Successfully created AppConnectionPolicy %s", p.Config.GetMetadata().GetName())
	return &awiGrpc.CreateAppConnectionPolicyResponse{
		Status: "SUCCESS",
		Id:     p.ID,
	}, nil
}

func (s *Server) GetAppConnectionPolicy(ctx context.Context, in *awiGrpc.GetAppConnectionPolicyRequest) (*awiGrpc.GetAppConnectionPolicyResponse, error) {
	appConnDb, err := s.db.GetAppConnectionPolicy(in.GetId())
	if err != nil {
		return nil, err
	}
	if appConnDb == nil {
		return nil, fmt.Errorf("no such app-connection-policy")
	}
	return &awiGrpc.GetAppConnectionPolicyResponse{AppConnectionPolicy: &awiGrpc.AppConnectionPolicy{
		Id:            appConnDb.ID,
		AppConnection: appConnDb.Config,
	}}, nil
}

func (s *Server) DeleteAppConnectionPolicy(ctx context.Context, in *awiGrpc.DeleteAppConnectionPolicyRequest) (*awiGrpc.DeleteAppConnectionPolicyResponse, error) {
	appConnectionPolicyID := in.GetId()
	deleteFunc := func() error {
		p, err := s.db.GetAppConnectionPolicy(appConnectionPolicyID)
		if err != nil {
			return err
		}
		if p == nil {
			s.logger.Infof("AppConnectionPolicy %s no longer exists", appConnectionPolicyID)
			return nil
		}
		err = s.db.DeleteAppConnectionPolicy(appConnectionPolicyID)
		if err != nil {
			return err
		}
		return nil
	}

	err := deleteFunc()
	if err != nil {
		s.logger.Errorf("Error during DeleteAppConnectionPolicy: %v", err)
		return &awiGrpc.DeleteAppConnectionPolicyResponse{
			Status: "FAILED",
		}, err
	}
	s.logger.Infof("Successfully removed AppConnectionPolicy %s", appConnectionPolicyID)
	return &awiGrpc.DeleteAppConnectionPolicyResponse{
		Status: "SUCCESS",
	}, nil
}

func (s *Server) ListAppConnectionPolicies(ctx context.Context, in *awiGrpc.ListAppConnectionPoliciesRequest) (*awiGrpc.ListAppConnectionPoliciesResponse, error) {
	appConns, err := s.db.ListAppConnectionsPolicy()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	respConn := make([]*awiGrpc.AppConnectionPolicy, 0, len(appConns))
	for _, app := range appConns {
		respConn = append(respConn, &awiGrpc.AppConnectionPolicy{
			Id:            app.ID,
			AppConnection: app.Config,
		})
	}
	return &awiGrpc.ListAppConnectionPoliciesResponse{AppConnectionPolicies: respConn}, nil

}

func (s *Server) CreateAccessPolicy(ctx context.Context, req *awiGrpc.AccessPolicyCreateRequest) (*awiGrpc.AccessPolicyCreateResponse, error) {
	if req.GetAccessPolicy().GetMetadata().GetName() == "" {
		return &awiGrpc.AccessPolicyCreateResponse{
			Status: awiGrpc.Status_FAILED,
		}, fmt.Errorf("metadata.name is required in AccessPolicy spec")
	}
	err := s.db.UpdateAccessPolicy(&db.AccessPolicyDB{
		AccessPolicy: req.GetAccessPolicy(),
	}, req.GetAccessPolicy().GetMetadata().GetName())
	if err != nil {
		return &awiGrpc.AccessPolicyCreateResponse{
			Status: awiGrpc.Status_FAILED,
		}, err
	}
	return &awiGrpc.AccessPolicyCreateResponse{
		Status: awiGrpc.Status_SUCCESS,
	}, nil
}

func (s *Server) DeleteAccessPolicy(ctx context.Context, request *awiGrpc.AccessPolicyDeleteRequest) (*awiGrpc.AccessPolicyDeleteResponse, error) {
	return &awiGrpc.AccessPolicyDeleteResponse{}, s.db.DeleteAccessPolicy(request.GetName())
}

func (s *Server) ListAccessPolicies(ctx context.Context, request *awiGrpc.AccessPolicyListRequest) (*awiGrpc.AccessPolicyListResponse, error) {
	l, err := s.db.ListAccessPolicies()
	if err != nil {
		return nil, err
	}
	policies := make([]*awiGrpc.Security_AccessPolicy, 0, len(l))
	for _, v := range l {
		policies = append(policies, v.AccessPolicy)
	}
	return &awiGrpc.AccessPolicyListResponse{AccessPolicies: policies}, nil
}

func (s *Server) validateConnectionRequest(csr *awiGrpc.ConnectionRequest) error {
	if csr == nil {
		return fmt.Errorf("the Connection Request cannot be nil")
	}
	if csr.Spec == nil {
		return fmt.Errorf("spec of Connection Request cannot be nil")
	}
	if csr.Metadata == nil {
		return fmt.Errorf("metadata of source of Connection Request cannot be nil")
	}
	return nil
}

func (s *Server) validateDisconnectionRequest(csr *awiGrpc.DisconnectRequest) error {
	if csr == nil {
		return fmt.Errorf("the Disonnect Request cannot be nil")
	}
	if csr.ConnectionId == "" {
		return fmt.Errorf("the Disonnect Request ID cannot be empty")
	}
	return nil
}

func (s *Server) validateGetConnectionRequest(gsr *awiGrpc.GetConnectionRequest) error {
	if gsr == nil {
		return fmt.Errorf("the Get Connection Request cannot be nil")
	}
	if gsr.ConnectionId == "" {
		return fmt.Errorf("the Get Connection Request ID cannot be empty")
	}
	return nil
}

func (s *Server) validateListConnectionRequest(lc *awiGrpc.ListConnectionsRequest) error {
	if lc == nil {
		return fmt.Errorf("the List Connection Request cannot be nil")
	}
	return nil
}

func (s *Server) validateConnectionStatusRequest(gcs *awiGrpc.ConnectionStatusRequest) error {
	if gcs == nil {
		return fmt.Errorf("the Connection Status Request cannot be nil")
	}
	if gcs.ConnectionId == "" {
		return fmt.Errorf("the Connection Status Request ID cannot be empty")
	}
	return nil
}

func (s *Server) validateListAppConnectionRequest(lc *awiGrpc.ListAppConnectionsRequest) error {
	if lc == nil {
		return fmt.Errorf("the List Connection Request cannot be nil")
	}
	return nil
}

func initConfig(configFilePath string) error {
	viper.AutomaticEnv()
	viper.SetConfigFile(configFilePath)

	if err := viper.ReadInConfig(); err != nil {
		if _, match := err.(viper.UnsupportedConfigError); match || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("unsupported Config or File doesn't exist")
		}
		return err
	}

	return nil
}

func getClient(logger *logrus.Logger) (vmanage.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("could not create cookie jar: %v", err)
	}
	client, err := getClientWithJar(jar, logger)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func getClientWithJar(jar *cookiejar.Jar, logger *logrus.Logger) (vmanage.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !viper.GetBool(secureConnectionFlag),
		MinVersion:         tls.VersionTLS12,
	}
	httpclient := &http.Client{
		Transport: transport,
		Jar:       jar,
	}
	vManageURL := viper.GetString(urlFlag)
	retries := viper.GetInt(longPollRetriesFlag)
	retriesInterval := viper.GetDuration(retriesIntervalFlag)
	client, err := vmanage.NewClient(vManageURL, httpclient, logger, retriesInterval, retries)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func getRequestStatus(req *db.ConnectionRequest) awiGrpc.Status {
	if req.Status == db.StateRunning {
		return awiGrpc.Status_SUCCESS
	}
	if req.Status == db.StatePending {
		return awiGrpc.Status_IN_PROGRESS
	}
	if req.Status == db.StateWatching {
		return awiGrpc.Status_WATCHING
	}

	return awiGrpc.Status_FAILED
}

func getAppRequestStatus(req *db.AppConnection) awiGrpc.Status {
	return awiGrpc.Status(awiGrpc.Status_value[req.Status])
}
