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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/app-net-interface/awi-infra-guard/grpc/go/infrapb"
	"github.com/sirupsen/logrus"
	"gopkg.in/docker/docker.v20/pkg/namesgenerator"

	"awi-grpc-catalyst-sdwan/pkg/db"
	"awi-grpc-catalyst-sdwan/pkg/translator"

	awiGrpc "github.com/app-net-interface/awi-grpc/pb"
	"github.com/app-net-interface/awi-infra-guard/provider"
	"github.com/app-net-interface/awi-infra-guard/types"
	"github.com/app-net-interface/catalyst-sdwan-app-client/acl"
	"github.com/app-net-interface/catalyst-sdwan-app-client/client"
	"github.com/app-net-interface/catalyst-sdwan-app-client/common"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection"
	"github.com/app-net-interface/catalyst-sdwan-app-client/connection/sequence"
	"github.com/app-net-interface/catalyst-sdwan-app-client/feature"
	"github.com/app-net-interface/catalyst-sdwan-app-client/policy"
	"github.com/app-net-interface/catalyst-sdwan-app-client/urlallowlist"
	"github.com/app-net-interface/catalyst-sdwan-app-client/urldenylist"
	"github.com/app-net-interface/catalyst-sdwan-app-client/urlfiltering"
	"github.com/app-net-interface/catalyst-sdwan-app-client/vmanage"
)

type SelectorType int

const (
	Service SelectorType = iota
	Endpoint
	Subnet
	Namespace
	ExternalEntities
	SGT
)

const AllNetworkDomainConnectionsIdentifier = "all"

const NetworkDomainConnectionWatchLabelKey = "awi_watching"

var supportedClouds = []string{"AWS", "GCP"}

func getSelectorType(selectorType string) (SelectorType, error) {
	switch selectorType {
	case "service":
		return Service, nil
	case "endpoint":
		return Endpoint, nil
	case "subnet":
		return Subnet, nil
	case "namespace":
		return Namespace, nil
	case "external":
		return ExternalEntities, nil
	case "sgt":
		return SGT, nil
	default:
		return -1, fmt.Errorf("unknown type of selector %s", selectorType)
	}
}

func (r SelectorType) toString() string {
	switch r {
	case Service:
		return "service"
	case Endpoint:
		return "endpoint"
	case Subnet:
		return "subnet"
	case Namespace:
		return "namespace"
	case ExternalEntities:
		return "external"
	case SGT:
		return "sgt"
	default:
		return "unknown"
	}
}

const (
	CiscoProvider         = "Cisco SDWAN"
	computeTypeKubernetes = "kubernetes"
	computeTypeLabel      = "awi/compute_type"
	computeIdLabel        = "awi/compute_id"
)

type clock interface {
	Sleep(duration time.Duration)
}

type Credentials struct {
	username string
	password string
}

type AWIConnectionController struct {
	Credentials
	logger          *logrus.Logger
	client          vmanage.Client
	dbClient        db.Client
	clock           clock
	strategy        provider.Strategy
	supportedClouds []string
	connector       NetworkDomainConnector
	// connections marked as already deleted or pending deleted
	// TODO this information will be lost after app restart so we should consider storing it in db
	deletedConnection map[string]struct{}
}

type realClock struct{}

func (*realClock) Sleep(duration time.Duration) {
	time.Sleep(duration)
}

func NewDefaultConnectionController(
	fileName string,
	client vmanage.Client,
	logger *logrus.Logger,
	strategy provider.Strategy,
	connector NetworkDomainConnector,
) (*AWIConnectionController, error) {
	dbClient := db.NewClient()
	if err := dbClient.Open(fileName); err != nil {
		return nil, err
	}
	return NewConnectionControllerWithDb(client, logger, dbClient, strategy, connector), nil
}

func NewConnectionControllerWithDb(
	client vmanage.Client,
	logger *logrus.Logger,
	dbClient db.Client,
	strategy provider.Strategy,
	connector NetworkDomainConnector,
) *AWIConnectionController {
	clock := &realClock{}
	return newConnectionController(client, logger, dbClient, clock, strategy, connector)
}

func newConnectionController(
	client vmanage.Client,
	logger *logrus.Logger,
	dbClient db.Client,
	clock clock,
	strategy provider.Strategy,
	connector NetworkDomainConnector,
) *AWIConnectionController {

	return &AWIConnectionController{
		Credentials: Credentials{
			username: os.Getenv("VMANAGE_USERNAME"),
			password: os.Getenv("VMANAGE_PASSWORD"),
		},
		client:            client,
		logger:            logger,
		dbClient:          dbClient,
		clock:             clock,
		strategy:          strategy,
		supportedClouds:   supportedClouds,
		connector:         connector,
		deletedConnection: map[string]struct{}{},
	}
}

func (m *AWIConnectionController) GetConnectorType() NetworkDomainConnector {
	return m.connector
}

func (m *AWIConnectionController) isControllerVManage() bool {
	return m.connector == VManageConnector
}

func (m *AWIConnectionController) Close() error {
	if m.dbClient != nil {
		if err := m.dbClient.Close(); err != nil {
			m.logger.Errorf("could not Close DB connection: %v", err)
		}
	}
	return nil
}

func (m *AWIConnectionController) RefreshAppConnections(ctx context.Context) error {
	acls, err := m.dbClient.ListAppConnections()
	if err != nil {
		return err
	}

	for _, dbACL := range acls {
		if dbACL.ConnectionID == AllNetworkDomainConnectionsIdentifier {
			return nil
		}
		m.logger.Infof("Refreshing AppConnection %s", dbACL.ID)
		if dbACL.ConnectionID == "" {
			return nil
		}
		cloudProvider, err := m.strategy.GetProvider(ctx, dbACL.Provider)
		if err != nil {
			return err
		}
		if err := m.refreshSecurityGroups(ctx, cloudProvider, &dbACL); err != nil {
			return err
		}
		if err := m.dbClient.UpdateAppConnection(&dbACL, dbACL.ID); err != nil {
			return err
		}
	}
	return nil
}

func (m *AWIConnectionController) RefreshNetworkDomainConnections(ctx context.Context) error {
	connRequests, err := m.dbClient.ListConnectionRequests()
	if err != nil {
		return err
	}

	for _, requestConfig := range connRequests {
		if requestConfig.Status != db.StateWatching {
			continue
		}
		m.logger.Infof("Refreshing connection config %v", requestConfig.Name)
		requests, err := m.findNetworkDomains(requestConfig, requestConfig.Config)
		if err != nil {
			return err
		}
		for _, request := range requests {
			err := m.createNetworkDomainConnection(ctx, request, false)
			if err != nil {
				m.logger.Errorf("Failed to create network domain connection from %s to %s: %v",
					request.Source.ID, request.Destination.ID, err)
			}
		}
	}

	return nil
}

func (m *AWIConnectionController) CreateConnection(ctx context.Context, csr *awiGrpc.ConnectionRequest) error {
	requests, err := m.processConnectionConfig(csr)
	if err != nil {
		return err
	}
	var failedErr error = nil
	for _, request := range requests {
		err = m.createNetworkDomainConnection(ctx, request, true)
		if err != nil {
			failedErr = err
		}
	}

	return failedErr
}

func (m *AWIConnectionController) createNetworkDomainConnection(ctx context.Context,
	request *db.ConnectionRequest, force bool) error {
	m.logger.Infof("Creating connection request: %v", request.Name)
	normalizeNames(request)
	id := fmt.Sprintf("%s:%s", request.Source.ID, request.Destination.ID)
	if !force {
		_, markedAsDeleted := m.deletedConnection[id]
		if markedAsDeleted {
			m.logger.Infof("Connection from %s to %s already deleted or pending delete", request.Source.ID, request.Destination.ID)
			return nil
		}
	} else {
		delete(m.deletedConnection, id) // user recreated connection
	}

	request.ID = id
	m.logger.Infof("Creating connection request: %s, id: %s", request.Name, request.ID)
	oldRequest, err := m.dbClient.GetConnectionRequest(request.ID)
	if err != nil {
		return err
	}
	if oldRequest != nil {
		m.logger.Infof("Connection from %s to %s already exsits", request.Source.ID, request.Destination.ID)
		return nil
	}
	if err := m.processSubnets(ctx, request); err != nil {
		return err
	}
	if m.isControllerVManage() {
		if err := m.tagConnection(ctx, request); err != nil {
			return err
		}
	}
	if err := m.createVPCPolicy(ctx, request); err != nil {
		return err
	}
	if m.isControllerVManage() {
		if err := m.createACLPolicy(ctx, namesgenerator.GetRandomName(0), request.Name, request, nil, nil, nil); err != nil {
			return err
		}
	}
	if m.isControllerVManage() {
		if _, err := m.updateConnectionRequest(ctx, request, oldRequest, true); err != nil {
			return err
		}
	}
	if m.connector == AwiConnector {
		if err := m.connectVPCsWithAWI(ctx, request); err != nil {
			return err
		}
	}
	if err := m.createDBConnectionRequest(request); err != nil {
		return err
	}
	if m.isControllerVManage() {
		m.logger.Infof("Succssfully initialized network domain connection creation in vManage %s", request.Name)
	}
	if m.connector == AwiConnector {
		m.logger.Infof("Succssfully created network domain connection %s", request.Name)
	}
	err = m.applyAppConnectionsForAllNDConnections(ctx, request)
	if err != nil {
		m.logger.Errorf("Failed to apply app connections created for all network domain connections for "+
			"network domain connection %s: %v", request.Name, err)
	}
	return nil
}

func (m *AWIConnectionController) processConnectionConfig(csr *awiGrpc.ConnectionRequest) ([]*db.ConnectionRequest, error) {
	baseRequest := db.ConnectionRequest{
		Config:   csr.GetSpec(),
		Metadata: csr.GetMetadata(),
		Name:     csr.GetMetadata().GetName(),
		Status:   db.StatePending,
		Source: db.ConnectionRequestSource{
			Metadata: db.Metadata{
				Name:        csr.GetSpec().GetSource().GetMetadata().GetName(),
				Description: csr.GetSpec().GetSource().GetMetadata().GetDescription(),
			},
			SiteID: csr.GetSpec().GetSource().GetNetworkDomain().GetSelector().GetMatchSite().GetId(),
			ID:     csr.GetSpec().GetSource().GetNetworkDomain().GetSelector().GetMatchId().GetId(),
		},
		Destination: db.ConnectionRequestDestination{
			Metadata: db.Metadata{
				Name:        csr.GetSpec().GetDestination().GetMetadata().GetName(),
				Description: csr.GetSpec().GetDestination().GetMetadata().GetDescription(),
			},
			SiteID: csr.GetSpec().GetDestination().GetNetworkDomain().GetSelector().GetMatchSite().GetId(),
			ID:     csr.GetSpec().GetDestination().GetNetworkDomain().GetSelector().GetMatchId().GetId(),
		},
	}

	matchedPolicies, err := m.findAccessPoliciesForNetworkDomainConfig(csr.GetSpec())
	if err != nil {
		return nil, err
	}
	// TODO this should be translated into proper access rules with protocols and ports with all policies
	// and priorities considered
	if len(matchedPolicies) > 0 {
		baseRequest.Destination.DefaultAccess = strings.ToLower(matchedPolicies[0].AccessPolicy.GetAccessType())
	}

	err = m.createNetworkDomainConnectionWatch(baseRequest)
	if err != nil {
		return nil, err
	}

	return m.findNetworkDomains(baseRequest, csr.GetSpec())
}

func (m *AWIConnectionController) createNetworkDomainConnectionWatch(baseRequest db.ConnectionRequest) error {
	m.logger.Infof("Labels %v", baseRequest.Metadata.Labels)
	if baseRequest.Metadata.Labels == nil {
		return nil
	}
	val, ok := baseRequest.Metadata.Labels[NetworkDomainConnectionWatchLabelKey]
	v := strings.ToLower(val)
	if ok && (v == "true" || v == "ok" || v == "1" || v == "yes" || v == "aye") {
		baseRequest.Status = db.StateWatching
		baseRequest.ID = fmt.Sprintf("watch-of-%s", strings.Replace(baseRequest.Name, " ", "-", -1))
		m.logger.Infof("Creating Watcher for network domain connection: %s", baseRequest.Name)
		err := m.createDBConnectionRequest(&baseRequest)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *AWIConnectionController) findAccessPoliciesForNetworkDomainConfig(spec *awiGrpc.NetworkDomainConnectionConfig) ([]*db.AccessPolicyDB, error) {
	if spec.GetAccessPolicy() == nil {
		return nil, nil
	}
	matchID := spec.GetAccessPolicy().GetSelector().GetMatchId().GetId()
	matchName := spec.GetAccessPolicy().GetSelector().GetMatchName().GetName()
	matchLabels := spec.GetAccessPolicy().GetSelector().GetMatchLabels()
	return m.findAccessPolicies(matchID, matchName, matchLabels)
}

func (m *AWIConnectionController) findAccessPoliciesForAppConnection(spec *awiGrpc.AppConnection) ([]*db.AccessPolicyDB, error) {
	if spec.GetAccessPolicy() == nil {
		return nil, nil
	}
	matchID := spec.GetAccessPolicy().GetSelector().GetMatchId().GetId()
	matchName := spec.GetAccessPolicy().GetSelector().GetMatchName().GetName()
	matchLabels := spec.GetAccessPolicy().GetSelector().GetMatchLabels()
	return m.findAccessPolicies(matchID, matchName, matchLabels)
}

func accessPolicyToNetworkAccessControl(policies []*db.AccessPolicyDB) []*awiGrpc.NetworkAccessControl {
	var nac []*awiGrpc.NetworkAccessControl
	for _, policy := range policies {
		for _, rule := range policy.AccessPolicy.GetAccessProtocols() {
			nac = append(nac, &awiGrpc.NetworkAccessControl{
				Protocol: rule.Protocol,
				Port:     rule.Port,
			})
		}
	}
	return nac
}

func (m *AWIConnectionController) findAccessPolicies(matchID string, matchName string,
	matchLabels map[string]string) ([]*db.AccessPolicyDB, error) {
	if matchID != "" {
		policy, err := m.dbClient.GetAccessPolicy(matchID)
		if err != nil {
			return nil, err
		}
		if policy == nil {
			return nil, fmt.Errorf("couldn't find access policy with ID: %v", matchID)
		}
		return []*db.AccessPolicyDB{policy}, nil
	}

	if matchName != "" {
		policies, err := m.dbClient.ListAccessPolicies()
		if err != nil {
			return nil, err
		}
		for _, policy := range policies {
			if policy.AccessPolicy.GetMetadata().GetName() == matchName {
				return []*db.AccessPolicyDB{&policy}, nil
			}
		}
		return nil, fmt.Errorf("couldn't find matching network policy with name %s", matchName)
	}

	if matchLabels != nil &&
		len(matchLabels) > 0 {
		policies, err := m.dbClient.ListAccessPolicies()
		if err != nil {
			return nil, err
		}
		var matchedPolicies []*db.AccessPolicyDB
		for _, policy := range policies {
			matched := true
			for k, v := range matchLabels {
				if policy.AccessPolicy.GetMetadata().GetLabels()[k] != v {
					matched = false
					break
				}
			}
			if matched {
				m.logger.Infof("Matched access policy %s to network domain connection",
					policy.AccessPolicy.GetMetadata().GetName())
				matchedPolicies = append(matchedPolicies, &policy)
			}
			return matchedPolicies, nil
		}
	}
	return nil, fmt.Errorf("couldn't find matching network policy")
}

func (m *AWIConnectionController) findNetworkDomains(baseRequest db.ConnectionRequest, spec *awiGrpc.NetworkDomainConnectionConfig) ([]*db.ConnectionRequest, error) {
	sourceId := baseRequest.Source.ID
	destinationId := baseRequest.Destination.ID

	networkDomains, err := m.dbClient.ListNetworkDomains()
	if err != nil {
		return nil, err
	}

	sourceMatchName := spec.GetSource().GetNetworkDomain().GetSelector().GetMatchName().GetName()
	destinationMatchName := spec.GetDestination().GetNetworkDomain().GetSelector().GetMatchName().GetName()
	sourceMatchLabels := spec.GetSource().GetNetworkDomain().GetSelector().GetMatchLabels()
	destinationMatchLabels := spec.GetDestination().GetNetworkDomain().GetSelector().GetMatchLabels()

	var withSource []db.ConnectionRequest
	for _, nd := range networkDomains {
		request := baseRequest

		if (sourceId != "" && nd.NetworkDomain.GetId() == sourceId) ||
			(sourceMatchName != "" && nd.NetworkDomain.Name == sourceMatchName) ||
			checkLabelsMatch(sourceMatchLabels, nd.NetworkDomain.GetLabels()) {
			request.Source.ID = nd.NetworkDomain.GetId()
			request.Source.Type = nd.NetworkDomain.GetType()
			request.Source.Provider = nd.NetworkDomain.GetProvider()
			withSource = append(withSource, request)
		}
	}

	if len(withSource) == 0 {
		return nil, fmt.Errorf("couldn't determine source network domain")
	}

	var connectionRequests []*db.ConnectionRequest
	for _, nd := range networkDomains {
		if (destinationId != "" && nd.NetworkDomain.GetId() == destinationId) ||
			(destinationMatchName != "" && nd.NetworkDomain.Name == destinationMatchName) ||
			checkLabelsMatch(destinationMatchLabels, nd.NetworkDomain.GetLabels()) {
			for _, req := range withSource {
				request := req
				request.Destination.ID = nd.NetworkDomain.GetId()
				request.Destination.Type = nd.NetworkDomain.GetType()
				request.Destination.Provider = nd.NetworkDomain.GetProvider()
				connectionRequests = append(connectionRequests, &request)
			}
		}
	}
	if len(connectionRequests) == 0 {
		return nil, fmt.Errorf("couldn't determine destination network domain")
	}
	return connectionRequests, nil
}

func checkLabelsMatch(match, all map[string]string) bool {
	if len(match) == 0 {
		return false
	}
	counter := len(match)
	for k, v := range match {
		for ka, va := range all {
			// ignoring case sensitivity
			if strings.EqualFold(k, ka) && strings.EqualFold(v, va) {
				counter--
			}
		}
	}
	return counter == 0
}

func (m *AWIConnectionController) CreateAppConnection(ctx context.Context, appConnectionRequest *awiGrpc.AppConnection) error {
	if appConnectionRequest.GetNetworkDomainConnection().GetSelector().GetMatchName() == AllNetworkDomainConnectionsIdentifier {
		return m.createAppConnectionForAllNDConnections(ctx, appConnectionRequest)
	} else {
		domainConnection, err := m.getNetworkDomainConnection(appConnectionRequest.GetNetworkDomainConnection().GetSelector().GetMatchName())
		if err != nil {
			return err
		}
		err = m.createAppConnection(ctx, domainConnection, appConnectionRequest, nil)
		if err == nil {
			m.logger.Infof("Successfully created AppConnection %s", appConnectionRequest.GetMetadata().GetName())
		} else {
			m.logger.Errorf("Failed to create AppConnection %s: %v", appConnectionRequest.GetMetadata().GetName(), err)
		}
	}

	return nil
}

func (m *AWIConnectionController) createAppConnectionForAllNDConnections(ctx context.Context,
	appConnectionRequest *awiGrpc.AppConnection) error {
	appliedFor := make(map[string]struct{})
	allNetworkDomainConnections, err := m.getAllNetworkDomainConnections()
	if err != nil {
		return err
	}
	appConnName := appConnectionRequest.GetMetadata().GetName()
	counter := 0
	// if app connection is created for all network domain connections,
	// create app connections for all existing network domain connections
	for _, conn := range allNetworkDomainConnections {
		counter++
		newAppConnName := appConnName + "-" + strconv.Itoa(counter)
		appConnectionRequest.Metadata.Name = newAppConnName
		conn := conn
		err = m.createAppConnection(ctx, &conn, appConnectionRequest, nil)
		if err == nil {
			appliedFor[conn.ID] = struct{}{}
			m.logger.Infof("Successfully created AppConnection %s for network domain connection %s",
				appConnectionRequest.GetMetadata().GetName(), conn.Name)
		} else {
			m.logger.Errorf("Failed to create AppConnection %s for network domain connection %s: %v", appConnectionRequest.GetMetadata().GetName(), conn.Name, err)
		}
	}
	// create reference app connection,
	// which will be used for creating such app connection for all new network domain connections.
	appConnectionRequest.Metadata.Name = appConnName
	allNetworkDomainsStatus := &db.AppConnForAllNetworkDomainStatus{
		Counter: counter,
	}
	err = m.createAppConnection(ctx, nil, appConnectionRequest, allNetworkDomainsStatus)
	if err != nil {
		return err
	}
	if err == nil {
		m.logger.Infof("Successfully created reference AppConnection %s for all network domain connections",
			appConnectionRequest.GetMetadata().GetName())
	} else {
		m.logger.Errorf("Failed to created reference AppConnection %s for all network domain connections: %v",
			appConnectionRequest.GetMetadata().GetName(), err)
		return err
	}
	return nil
}

func (m *AWIConnectionController) applyAppConnectionsForAllNDConnections(ctx context.Context,
	domainConnection *db.ConnectionRequest) error {
	appConns, err := m.dbClient.ListAppConnections()
	if err != nil {
		return err
	}

	for _, appConn := range appConns {
		if appConn.ConnectionID != AllNetworkDomainConnectionsIdentifier {
			continue
		}
		counter := appConn.AllNetworkDomainsStatus.Counter + 1
		appConnName := appConn.Config.GetMetadata().GetName()
		appConn.Config.Metadata.Name = appConnName + "-" + strconv.Itoa(counter)
		err := m.createAppConnection(ctx, domainConnection, appConn.Config, nil)
		if err != nil {
			return err
		}
		appConn.Config.Metadata.Name = appConnName
		appConn.AllNetworkDomainsStatus.Counter++
		if err := m.dbClient.UpdateAppConnection(&appConn, appConn.ID); err != nil {
			return err
		}

	}
	return nil
}

func (m *AWIConnectionController) createAppConnection(ctx context.Context, domainConnection *db.ConnectionRequest,
	appConnectionRequest *awiGrpc.AppConnection, allNetworkDomainsStatus *db.AppConnForAllNetworkDomainStatus) error {
	id, ok := ctx.Value("id").(string)
	if !ok || id == "" {
		id = namesgenerator.GetRandomName(0)
	}
	switch {
	case domainConnection == nil: // app connections without related network domain connection
		// create reference app connection for all network domain connections
		if allNetworkDomainsStatus != nil {
			connectionId := AllNetworkDomainConnectionsIdentifier
			aclDB := &db.AppConnection{
				ID:                      id,
				ConnectionID:            connectionId,
				Name:                    appConnectionRequest.GetMetadata().GetName(),
				Config:                  appConnectionRequest,
				Status:                  "SUCCESS",
				AllNetworkDomainsStatus: *allNetworkDomainsStatus,
			}
			return m.dbClient.UpdateAppConnection(aclDB, aclDB.ID)
		}
		if appConnectionRequest.GetTo().GetExternalEntities() != nil {
			return m.createURLFiltering(ctx, id, appConnectionRequest)
		}
		return nil
	case domainConnection.Source.Type == db.ConnectionTypeVPC || domainConnection.Source.Type == db.ConnectionTypeTag: // cloud ACL
		return m.updateCloudAccessControl(ctx, id, appConnectionRequest, domainConnection)
	default: // vManage ACL
		sequences, err := m.createACLSequence(ctx, appConnectionRequest, domainConnection)
		if err != nil {
			return err
		}
		matchingPolicies, err := m.findAccessPoliciesForAppConnection(appConnectionRequest)
		if err != nil {
			return err
		}
		nac := accessPolicyToNetworkAccessControl(matchingPolicies)
		return m.createACLPolicy(ctx, id, appConnectionRequest.GetMetadata().GetName(), domainConnection, sequences,
			nac, appConnectionRequest)
	}
}

func (m *AWIConnectionController) GetMatchedResources(ctx context.Context, appConnection *awiGrpc.AppConnection) (
	source *awiGrpc.MatchedResources, destination *awiGrpc.MatchedResources, err error) {
	conn, err := m.getNetworkDomainConnection(appConnection.GetNetworkDomainConnection().GetSelector().GetMatchName())
	if conn == nil {
		return nil, nil, err
	}

	// source side
	switch {
	case conn.Source.Type == db.ConnectionTypeVPC || conn.Source.Type == db.ConnectionTypeTag:
		cloudProvider, err := m.strategy.GetProvider(ctx, conn.Source.Provider)
		if err != nil {
			return nil, nil, err
		}
		sourceData, err := m.getSourceACLData(ctx, cloudProvider, appConnection.GetFrom(), conn.Source.Tag, conn.Source.ID, conn.Source.Region)
		if err != nil {
			return nil, nil, err
		}
		source = sourceData.Matched
	}

	// dest side
	switch {
	case conn.Destination.Type == db.ConnectionTypeVPC || conn.Destination.Type == db.ConnectionTypeTag:
		cloudProvider, err := m.strategy.GetProvider(ctx, conn.Destination.Provider)
		if err != nil {
			return nil, nil, err
		}
		destination, err = m.getDestinationMatchedData(ctx, cloudProvider, appConnection.GetTo(), conn.Destination.Tag, conn.Destination.ID, conn.Destination.Region)
		if err != nil {
			return nil, nil, err
		}
	}

	return
}

func (m *AWIConnectionController) GetMatchedTo(ctx context.Context, networkDomainConnectionName string, to *awiGrpc.To) (*awiGrpc.MatchedResources, error) {
	return nil, nil
}

func (m *AWIConnectionController) getNetworkDomainConnection(networkDomainConnectionIdentifier string) (*db.ConnectionRequest, error) {
	if networkDomainConnectionIdentifier == "" {
		return nil, nil
	}
	// look by connection ID
	conn, err := m.dbClient.GetConnectionRequest(networkDomainConnectionIdentifier)
	if conn != nil && err == nil {
		return conn, nil
	}
	// look by connection name
	connections, err := m.dbClient.ListConnectionRequests()
	if err != nil {
		return nil, err
	}
	for _, conn := range connections {
		if conn.Name == networkDomainConnectionIdentifier {
			return &conn, nil
		}
	}
	return nil, fmt.Errorf("couldn't find related network domain connection: %s",
		networkDomainConnectionIdentifier)
}

func (m *AWIConnectionController) getAllNetworkDomainConnections() ([]db.ConnectionRequest, error) {
	return m.dbClient.ListConnectionRequests()
}

func updateIDs(request *db.ConnectionRequest) {
	if request.ID == "" {
		request.ID = namesgenerator.GetRandomName(0)
	}
	if request.Destination.DbID == "" {
		request.Destination.DbID = namesgenerator.GetRandomName(0)
	}
}

func (m *AWIConnectionController) processSubnets(ctx context.Context, request *db.ConnectionRequest) error {
	if request.Source.Type == db.ConnectionTypeSubnet {
		if len(request.Source.Networks) == 0 {
			return fmt.Errorf("subnet not specified in source.networks")
		}
		vpcID, err := m.getVPCIDForCIDR(ctx, request.Source.Region, request.Source.Networks[0], request.Source.Provider)
		if err != nil {
			return fmt.Errorf("could not find subnet: %v", err)
		}
		request.Source.ID = vpcID
		request.Source.Type = db.ConnectionTypeVPC
	}

	destination := &request.Destination
	if destination.Type == db.ConnectionTypeSubnet {
		vpcID, err := m.getVPCIDForCIDR(ctx, destination.Region, destination.ID, destination.Provider)
		if err != nil {
			return fmt.Errorf("could not find subnet: %v", err)
		}
		destination.ID = vpcID
		destination.Type = db.ConnectionTypeVPC
	}
	return nil
}

func (m *AWIConnectionController) getVPCIDForCIDR(ctx context.Context, region, cidr, cloud string) (string, error) {
	cloudProvider, err := m.strategy.GetProvider(ctx, cloud)
	if err != nil {
		return "", err
	}
	return cloudProvider.GetVPCIDForCIDR(ctx, &infrapb.GetVPCIDForCIDRRequest{
		Region: region,
		Cidr:   cidr,
	})
}

func (m *AWIConnectionController) tagConnection(ctx context.Context, connection *db.ConnectionRequest) error {
	if err := m.tagSource(ctx, &connection.Source); err != nil {
		return err
	}
	if err := m.tagDestination(ctx, &connection.Destination); err != nil {
		return err
	}

	return nil
}

func (m *AWIConnectionController) tagDestination(ctx context.Context, destination *db.ConnectionRequestDestination) error {
	if destination.Type != db.ConnectionTypeVPC {
		return nil
	}
	tag, region, err := m.getOrCreateVPCTag(ctx, destination.Provider, destination.ID)
	if err != nil {
		return err
	}
	destination.Tag = tag
	destination.Region = region
	if destination.Metadata.Name == "" {
		destination.Metadata.Name = tag
	}
	destination.Type = "tag"
	return nil
}

func (m *AWIConnectionController) tagSource(ctx context.Context, source *db.ConnectionRequestSource) error {
	if source.Type != db.ConnectionTypeVPC {
		return nil
	}
	tag, region, err := m.getOrCreateVPCTag(ctx, source.Provider, source.ID)
	if err != nil {
		return err
	}
	source.Tag = tag
	source.Region = region
	if source.Metadata.Name == "" {
		source.Metadata.Name = tag
	}
	source.Type = "tag"
	return nil
}

func (m *AWIConnectionController) getOrCreateVPCTag(ctx context.Context, cloudType, vpcID string) (tag string, region string, err error) {
	if !m.isControllerVManage() {
		return "", "", fmt.Errorf("can't get tag for non-vManage connector")
	}
	vpc, err := m.client.VPC().Get(ctx, strings.ToUpper(cloudType), vpcID)
	if err != nil {
		return "", "", err
	}
	if vpc.Tag != "" {
		return vpc.Tag, vpc.Region, nil
	}
	var newTagName string
	if vpc.HostVPCName != "" {
		newTagName = vpc.HostVPCName
	} else {
		newTagName = namesgenerator.GetRandomName(0)
	}
	m.logger.Infof("Creating tag %s for VPC %s", newTagName, vpcID)
	id, err := m.client.VPC().CreateVPCTag(ctx, newTagName, vpc)
	if err != nil {
		return "", "", fmt.Errorf("could not create tag: %v", err)
	}
	if err := m.client.Status().ActionStatusLongPoll(ctx, id); err != nil {
		return "", "", fmt.Errorf("error while waiting for status: %v", err)
	}
	return newTagName, vpc.Region, nil
}

func (m *AWIConnectionController) getVPCIdByTag(ctx context.Context, cloudType, tag string) (string, string, error) {
	if !m.isControllerVManage() {
		return "", "", fmt.Errorf("can't get VPCIDByTag for non-vmanage connector")
	}
	vpcs, err := m.client.VPC().ListWithTag(ctx, strings.ToUpper(cloudType), tag)
	if err != nil {
		return "", "", err
	}
	if len(vpcs) != 1 {
		return "", "", fmt.Errorf("expected one VPC with tag %s, found %d", tag, len(vpcs))
	}
	return vpcs[0].HostVPCID, vpcs[0].Region, nil
}

func (m *AWIConnectionController) getSubnetsByPrefixes(ctx context.Context, provider, region, vpcId string, prefixes []string) ([]types.Subnet, error) {
	cloudProvider, err := m.strategy.GetProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	var subnets []types.Subnet
	for _, cidr := range prefixes {
		subnet, err := cloudProvider.ListSubnets(ctx, &infrapb.ListSubnetsRequest{
			VpcId:  vpcId,
			Cidr:   cidr,
			Region: region,
		})
		if err != nil {
			return nil, err
		}
		subnets = append(subnets, subnet...)
	}
	return subnets, nil
}

func (m *AWIConnectionController) getInstancesBySubnets(ctx context.Context, provider, region, vpcId string, subnets []types.Subnet) ([]types.Instance, error) {
	cloudProvider, err := m.strategy.GetProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	allInstances, err := cloudProvider.ListInstances(ctx, &infrapb.ListInstancesRequest{
		VpcId:  vpcId,
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	subnetsIDMap := make(map[string]struct{})
	for _, subnet := range subnets {
		subnetsIDMap[subnet.SubnetId] = struct{}{}
		subnetsIDMap[subnet.Name] = struct{}{}
	}

	var instances []types.Instance
	for _, instance := range allInstances {
		_, ok := subnetsIDMap[instance.SubnetID]
		if ok {
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

func normalizeNames(connection *db.ConnectionRequest) {
	if strings.ToLower(connection.Source.Type) == "vrf" {
		connection.Source.Type = db.ConnectionTypeVPN
	}
	if strings.ToLower(connection.Destination.Type) == "vrf" {
		connection.Destination.Type = db.ConnectionTypeVPN
	}
	connection.Destination.Type = strings.ToLower(connection.Destination.Type)
	connection.Source.Type = strings.ToLower(connection.Source.Type)
	connection.Source.Provider = strings.ToLower(connection.Source.Provider)
	connection.Destination.Provider = strings.ToLower(connection.Destination.Provider)
	if connection.Name == "" {
		connection.Name = fmt.Sprintf("%s to %s", connection.Source.Metadata.Name, connection.Destination.Metadata.Name)
	}
}

func (m *AWIConnectionController) updateConnectionRequest(ctx context.Context, request *db.ConnectionRequest, oldRequest *db.ConnectionRequest, enable bool) (string, error) {
	if !m.isControllerVManage() {
		return "", fmt.Errorf("can't updateConnectionRequest for non-vManage connector")
	}
	connections := m.createConnectionMatricesAndRemoveNotUsedConnections(request, oldRequest, enable)
	if len(connections) == 0 {
		m.logger.Infof("Connection already exist - skipping")
		return "", nil
	}
	cloudType := request.Source.Provider
	if cloudType == CiscoProvider {
		cloudType = request.Destination.Provider
	}
	params := &connection.Parameters{
		CloudType: strings.ToUpper(cloudType),
		Matrices:  connections,
	}
	return m.client.Connection().Create(ctx, params)
}

func (m *AWIConnectionController) updateConnectionRequestAndWaitForStatus(
	ctx context.Context,
	request *db.ConnectionRequest,
	oldRequest *db.ConnectionRequest,
	enable bool,
) error {
	if !m.isControllerVManage() {
		return fmt.Errorf("can't update Connection request for non-vManage connector")
	}
	id, err := m.updateConnectionRequest(ctx, request, oldRequest, enable)
	if err != nil {
		return err
	}
	if err := m.client.Status().ActionStatusLongPoll(ctx, id); err != nil {
		return fmt.Errorf("error while waiting for status: %v", err)
	}

	return nil
}

func (m *AWIConnectionController) createDBConnectionRequest(request *db.ConnectionRequest) error {
	return m.dbClient.UpdateConnectionRequest(request, request.ID)
}

func (m *AWIConnectionController) connectVPCsWithAWI(ctx context.Context, request *db.ConnectionRequest) error {
	provider1 := request.Source.Provider
	provider2 := request.Destination.Provider
	if provider1 != provider2 {
		return m.connectVPCsWithAWIMultiCloud(ctx, request)
	}
	return m.connectVPCsWithAWISingleCloud(ctx, request)
}

func (m *AWIConnectionController) connectVPCsWithAWIMultiCloud(ctx context.Context, request *db.ConnectionRequest) error {
	firstProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
	if err != nil {
		return err
	}
	secondProvider, err := m.strategy.GetProvider(ctx, request.Destination.Provider)
	if err != nil {
		return err
	}

	firstOutput, err := firstProvider.ConnectVPC(ctx, types.SingleVPCConnectionParams{
		ConnID: request.ID,
		VpcID:  request.Source.ID,
		Region: request.Source.Region,
		Destination: types.DestinationDetails{
			Provider: strings.ToUpper(request.Destination.Provider),
			VPC:      request.Destination.ID,
			Region:   request.Destination.Region,
		},
	})
	if err != nil {
		return err
	}
	secondOutput, err := secondProvider.ConnectVPC(ctx, types.SingleVPCConnectionParams{
		ConnID: request.ID,
		VpcID:  request.Destination.ID,
		Region: request.Destination.Region,
		Destination: types.DestinationDetails{
			Provider: strings.ToUpper(request.Source.Provider),
			VPC:      request.Source.ID,
			Region:   request.Source.Region,
		},
	})
	if err != nil {
		return err
	}
	request.Source.Tag = request.Source.Metadata.Name
	request.Destination.Tag = request.Destination.Metadata.Name
	request.Source.Region = firstOutput.Region
	request.Destination.Region = secondOutput.Region
	request.Status = db.StateRunning
	return nil
}

func (m *AWIConnectionController) connectVPCsWithAWISingleCloud(ctx context.Context, request *db.ConnectionRequest) error {
	cloudProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
	if err != nil {
		return err
	}

	output, err := cloudProvider.ConnectVPCs(ctx, types.VPCConnectionParams{
		ConnID: request.ID,
		Vpc1ID: request.Source.ID,
		Vpc2ID: request.Destination.ID,
	})
	if err != nil {
		return err
	}
	request.Source.Tag = request.Source.Metadata.Name
	request.Destination.Tag = request.Destination.Metadata.Name
	request.Source.Region = output.Region1
	request.Destination.Region = output.Region2
	request.Status = db.StateRunning
	return nil
}

func (m *AWIConnectionController) createACLPolicy(
	ctx context.Context,
	id string,
	appConnectionName string,
	request *db.ConnectionRequest,
	sequences []acl.Sequence,
	protocolsAndPorts []*awiGrpc.NetworkAccessControl,
	appConnectionRequest *awiGrpc.AppConnection,
) error {
	if !m.isControllerVManage() {
		return fmt.Errorf("can't create vManage ACL policy for non-vManage connector")
	}
	if request.Source.SiteID == "" {
		return nil
	}
	aclName := fmt.Sprintf("%s-acl", request.Source.SiteID)
	if sequences == nil {
		sequences = make([]acl.Sequence, 0)
	}
	aclData, err := m.client.ACL().GetByName(ctx, aclName)
	if err != nil && !errors.Is(err, &common.NotFoundError{}) {
		return err
	}
	if aclData != nil {
		m.logger.Infof("Updating existing policy %s for site %s", aclData.DefinitionID, request.Source.SiteID)
		err := m.updateACL(ctx, request, sequences, aclData)
		if err != nil {
			return err
		}
		aclDB := &db.AppConnection{
			ConnectionID:      request.ID,
			ID:                id,
			Name:              appConnectionName,
			FeatureTemplateID: request.Source.FeatureTemplateID,
			Provider:          CiscoProvider,
			Status:            "SUCCESS",
			Config:            appConnectionRequest,
		}
		return m.dbClient.UpdateAppConnection(aclDB, aclDB.ID)
	}
	m.logger.Infof("Creating policy for site %s", request.Source.SiteID)
	aclID, aclName, err := m.createACL(ctx, request, sequences)
	if err != nil {
		return err
	}

	policyName := fmt.Sprintf("%s-policy", request.Source.SiteID)
	policyData := createPolicyData(policyName, aclID)
	policyID, err := m.client.Policy().Create(ctx, policyData)
	if err != nil {
		return err
	}

	deviceTemplate, err := m.client.Device().GetFromSite(ctx, request.Source.SiteID)
	if err != nil {
		return err
	}
	deviceTemplate.PolicyID = policyID
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

	ft, err := m.client.Feature().Get(ctx, request.Source.FeatureTemplateID)
	if err != nil {
		return fmt.Errorf("could not get feature template: %v", err)
	}
	ft.TemplateDefinition.AccessList = createAccessList(aclName)
	update, err := m.client.Feature().Update(ctx, request.Source.FeatureTemplateID, ft)
	if err != nil {
		return fmt.Errorf("could not update feature template: %v", err)
	}

	if err := m.pushConfiguration(ctx, update); err != nil {
		return err
	}

	aclDB := &db.AppConnection{
		ConnectionID:      request.ID,
		ID:                id,
		Name:              appConnectionName,
		FeatureTemplateID: request.Source.FeatureTemplateID,
		Provider:          CiscoProvider,
		Status:            "SUCCESS",
		ProtocolsAndPorts: translator.AwiGrpcProtocolsAndPortsToMap(protocolsAndPorts),
		Config:            appConnectionRequest,
	}
	return m.dbClient.UpdateAppConnection(aclDB, aclDB.ID)
}

func (m *AWIConnectionController) createACL(
	ctx context.Context,
	request *db.ConnectionRequest,
	sequences []acl.Sequence,
) (string, string, error) {
	if !m.isControllerVManage() {
		return "", "", fmt.Errorf("can't create vManage ACL for non-vManage connector")
	}
	aclName := fmt.Sprintf("%s-acl", request.Source.SiteID)
	aclData := createACLData(aclName, request.Destination.DefaultAccess, sequences)
	aclID, err := m.client.ACL().Create(ctx, aclData)
	if err != nil {
		return "", "", err
	}
	return aclID, aclName, nil
}

func (m *AWIConnectionController) updateACL(
	ctx context.Context,
	request *db.ConnectionRequest,
	sequences []acl.Sequence,
	acl *acl.ACL,
) error {
	if !m.isControllerVManage() {
		return fmt.Errorf("update vManage ACL for non-Vmanage connector")
	}
	aclName := fmt.Sprintf("%s-acl", request.Source.SiteID)
	data := createACLData(aclName, request.Destination.DefaultAccess, sequences)
	response, err := m.client.ACL().Update(ctx, acl.DefinitionID, data)
	if err != nil {
		return err
	}
	if err := m.pushConfiguration(ctx, response); err != nil {
		return err
	}
	return nil
}

func (m *AWIConnectionController) pushConfiguration(ctx context.Context, update *common.UpdateResponse) error {
	if !m.isControllerVManage() {
		return fmt.Errorf("can't push configuration for non-vManage connector")
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
	return nil
}

func (m *AWIConnectionController) createACLSequence(ctx context.Context, request *awiGrpc.AppConnection, connectionRequest *db.ConnectionRequest) ([]acl.Sequence, error) {
	seqManager := sequence.NewManager()
	cloud := connectionRequest.Source.Provider
	matchingPolicies, err := m.findAccessPoliciesForAppConnection(request)
	if err != nil {
		return nil, err
	}
	nac := accessPolicyToNetworkAccessControl(matchingPolicies)
	protocolsAndPorts := translator.AwiGrpcProtocolsAndPortsToVmanage(nac)
	fromType, err := getFromType(request.GetFrom())
	if err != nil {
		return nil, err
	}
	if err := m.createSequenceFromLabels(ctx, request.GetMetadata().GetName(), connectionRequest.Source.SiteID,
		fromType, getFromLabels(request.GetFrom()), protocolsAndPorts, cloud, seqManager); err != nil {
		return nil, err
	}

	for _, prefix := range request.GetFrom().GetSubnet().GetSelector().GetMatchPrefix() {
		seqManager.AddSequencesForCIDR(request.GetMetadata().GetName(), prefix, protocolsAndPorts)
	}

	toType, err := getToType(request.GetTo())
	if err != nil {
		return nil, err
	}
	cloud = connectionRequest.Destination.Provider
	if err := m.createSequenceFromLabels(ctx, request.GetMetadata().GetName(),
		connectionRequest.Destination.SiteID, toType, getToLabels(request.GetTo()), protocolsAndPorts, cloud, seqManager); err != nil {
		return nil, err
	}

	for _, prefix := range request.GetTo().GetSubnet().GetSelector().GetMatchPrefix() {
		seqManager.AddSequencesForCIDR(request.GetMetadata().GetName(), prefix, protocolsAndPorts)
	}
	return seqManager.Build(), nil
}

func getFromType(from *awiGrpc.From) (SelectorType, error) {
	if from.GetNamespace() != nil {
		return Namespace, nil
	}
	if from.GetSubnet() != nil {
		return Subnet, nil
	}
	if from.GetEndpoint() != nil {
		return Endpoint, nil
	}
	if from.GetSGT() != nil {
		return SGT, nil
	}

	return -1, fmt.Errorf("should be one of 'Subnet', 'Endpoint', 'Namespace', 'SGT'")
}

func getToType(to *awiGrpc.To) (SelectorType, error) {
	if to.GetService() != nil {
		return Service, nil
	}
	if to.GetNamespace() != nil {
		return Namespace, nil
	}
	if to.GetSubnet() != nil {
		return Subnet, nil
	}
	if to.GetEndpoint() != nil {
		return Endpoint, nil
	}
	if to.GetExternalEntities() != nil {
		return ExternalEntities, nil
	}

	return -1, fmt.Errorf("should be one of 'Service', 'Subnet', 'Endpoint', 'Namespace', 'ExternalEntities'")
}

func getFromLabels(from *awiGrpc.From) map[string]string {
	if from.GetNamespace() != nil {
		return from.GetNamespace().GetSelector().GetMatchLabels()
	}
	if from.GetSubnet() != nil {
		return from.GetSubnet().GetSelector().GetMatchLabels()
	}
	if from.GetEndpoint() != nil {
		return from.GetEndpoint().GetSelector().GetMatchLabels()
	}
	return nil
}

func getToLabels(to *awiGrpc.To) map[string]string {
	if to.GetService() != nil {
		return to.GetService().GetSelector().GetMatchLabels()
	}
	if to.GetNamespace() != nil {
		return to.GetNamespace().GetSelector().GetMatchLabels()
	}
	if to.GetSubnet() != nil {
		return to.GetSubnet().GetSelector().GetMatchLabels()
	}
	if to.GetEndpoint() != nil {
		return to.GetEndpoint().GetSelector().GetMatchLabels()
	}
	return nil
}

func (m *AWIConnectionController) createSequenceFromLabels(
	ctx context.Context,
	name string,
	region string,
	selectorType SelectorType,
	labels map[string]string,
	protocolsAndPort sequence.ProtocolsAndPorts,
	cloud string,
	seqManager *sequence.Manager,
) error {
	switch selectorType {
	case Subnet:
		return m.createSequenceFromLabelsForSubnet(ctx, name, region, protocolsAndPort, labels, cloud, seqManager)
	case Endpoint:
		return m.createSequenceFromLabelsForInstance(ctx, name, region, protocolsAndPort, labels, cloud, seqManager)
	}
	return fmt.Errorf("incorrect type for creating labels: %s", selectorType.toString())
}

func (m *AWIConnectionController) createSequenceFromLabelsForSubnet(ctx context.Context, name string, region string,
	protocols sequence.ProtocolsAndPorts, labels map[string]string, cloud string, seqManager *sequence.Manager) error {

	cloudProvider, err := m.strategy.GetProvider(ctx, cloud)
	if err != nil {
		return err
	}
	cidrs, err := cloudProvider.GetCIDRsForLabels(ctx, &infrapb.GetCIDRsForLabelsRequest{
		Labels: labels,
		Region: region,
	})
	if err != nil {
		return err
	}
	for _, cidr := range cidrs {
		seqManager.AddSequencesForCIDR(name, cidr, protocols)
	}
	return nil
}

func (m *AWIConnectionController) createSequenceFromLabelsForInstance(ctx context.Context, name, region string,
	protocols sequence.ProtocolsAndPorts, labels map[string]string, cloud string, seqManager *sequence.Manager) error {
	cloudProvider, err := m.strategy.GetProvider(ctx, cloud)
	if err != nil {
		return err
	}
	ips, err := cloudProvider.GetIPsForLabels(ctx, &infrapb.GetIPsForLabelsRequest{
		Labels: labels,
		Region: region,
	})
	if err != nil {
		return err
	}
	for _, ip := range ips {
		seqManager.AddSequencesForIP(name, ip, protocols)
	}
	return nil
}

func (m *AWIConnectionController) createConnectionMatricesAndRemoveNotUsedConnections(
	request, oldRequest *db.ConnectionRequest,
	enable bool,
) []connection.Matrix {
	connections := m.createConnectionMatrices(request, oldRequest, enable)
	updateIDs(request)
	return append(connections, m.createConnectionMatrices(oldRequest, request, !enable)...)
}

func (m *AWIConnectionController) createConnectionMatrices(
	request, oldRequest *db.ConnectionRequest,
	enable bool,
) []connection.Matrix {
	if request == nil {
		return nil
	}
	matrices := make([]connection.Matrix, 0, 2)
	conn := "disabled"
	if enable {
		conn = "enabled"
	}
	destination := &request.Destination
	if request != nil && oldRequest != nil && request.Destination.DbID == oldRequest.Destination.DbID {
		m.logger.Infof("Destination %s for request %s already exist - skipping", destination.Metadata.Name, request.Name)
		return matrices
	}
	srcId := request.Source.ID
	if request.Source.Tag != "" {
		srcId = request.Source.Tag
	}
	destId := destination.ID
	if destination.Tag != "" {
		destId = destination.Tag
	}
	matrix := connection.Matrix{
		SourceType:      request.Source.Type,
		SourceID:        srcId,
		DestinationType: destination.Type,
		DestinationID:   destId,
		Connection:      conn,
	}
	matrices = append(matrices, matrix)
	matrix = connection.Matrix{
		SourceType:      destination.Type,
		SourceID:        destId,
		DestinationType: request.Source.Type,
		DestinationID:   srcId,
		Connection:      conn,
	}
	matrices = append(matrices, matrix)
	return matrices
}

func createACLData(aclName, defaultAction string, sequences []acl.Sequence) *acl.Input {
	if defaultAction == "allow" {
		defaultAction = "accept"
	} else if defaultAction == "" || defaultAction == "deny" {
		defaultAction = "drop"
	}
	return &acl.Input{
		Name:        aclName,
		Description: aclName,
		Type:        "acl",
		DefaultAction: acl.DefaultAction{
			Type: defaultAction,
		},
		Sequences: sequences,
	}
}

func createPolicyData(policyName, aclID string) *policy.Input {
	return &policy.Input{
		PolicyName:        policyName,
		PolicyDescription: policyName,
		PolicyType:        "feature",
		PolicyDefinition: policy.Definition{
			Assemblies: []policy.Assembly{
				{
					DefinitionID: aclID,
					Type:         "acl",
				},
			},
			Settings: policy.Setting{
				ApplicationVisibility: true,
				FlowVisibility:        true,
			},
		},
	}
}

func createAccessList(aclName string) feature.VipPrimary[feature.AccessListValue] {
	return feature.VipPrimary[feature.AccessListValue]{
		VipObjectType: "tree",
		VipPrimaryKey: []string{"direction"},
		VipType:       "constant",
		VipValue: []feature.AccessListValue{
			{
				ACLName: feature.VipValue[string]{
					VipObjectType:   "object",
					VipType:         "constant",
					VipValue:        aclName,
					VipVariableName: "access_list_ingress_acl_name_ipv4",
				},
				Direction: feature.VipValue[string]{
					VipObjectType: "object",
					VipType:       "constant",
					VipValue:      "in",
				},
				PriorityOrder: []string{"direction", "acl-name"},
			},
			{
				ACLName: feature.VipValue[string]{
					VipObjectType:   "object",
					VipType:         "constant",
					VipValue:        aclName,
					VipVariableName: "access_list_egress_acl_name_ipv4",
				},
				Direction: feature.VipValue[string]{
					VipObjectType: "object",
					VipType:       "constant",
					VipValue:      "out",
				},
				PriorityOrder: []string{"direction", "acl-name"},
			},
		},
	}
}

func getACLName(request *db.ConnectionRequest) string {
	return fmt.Sprintf("%s-acl", request.Source.SiteID)
}

func getSGName(request *db.ConnectionRequest) string {
	return fmt.Sprintf("awi-%s-sg", strings.Replace(request.Name, " ", "-", -1))
}

func awiDefaultAccessTags(request *db.ConnectionRequest) map[string]string {
	return map[string]string{"awi": fmt.Sprintf("default-%s", request.Source.ID)}
}

func (m *AWIConnectionController) createVPCPolicy(
	ctx context.Context,
	request *db.ConnectionRequest,
) error {
	if !(request.Source.Type == db.ConnectionTypeVPC || request.Source.Type == db.ConnectionTypeTag) ||
		!(request.Destination.Type == db.ConnectionTypeVPC || request.Destination.Type == db.ConnectionTypeTag) {
		return nil
	}

	if request.Destination.DefaultAccess == "allow" {
		// 1. get all subnets from src vpc
		// 2. allow subnets from 1 in dest
		srcCloudProvider, err := m.strategy.GetProvider(ctx, request.Source.Provider)
		if err != nil {
			return err
		}
		// TODO this should be changed to get VPC CIDR and allow this whole CIDR
		srcSubnets, err := srcCloudProvider.ListSubnets(ctx, &infrapb.ListSubnetsRequest{
			VpcId:  request.Source.ID,
			Region: request.Source.Region,
		})
		if err != nil {
			return err
		}
		if len(srcSubnets) == 0 {
			return fmt.Errorf("couldn't find any subnets for VPC %s", request.Source.ID)
		}

		destCloudProvider, err := m.strategy.GetProvider(ctx, request.Destination.Provider)
		if err != nil {
			return err
		}

		var srcPrefixes []string
		for _, subnet := range srcSubnets {
			srcPrefixes = append(srcPrefixes, subnet.CidrBlock)
		}
		return destCloudProvider.AddInboundAllowRuleInVPC(ctx, "", request.Destination.Region, request.Destination.ID, srcPrefixes, getSGName(request),
			awiDefaultAccessTags(request))
	}
	return nil
}

func (m *AWIConnectionController) createURLFiltering(ctx context.Context, id string, appConnectionRequest *awiGrpc.AppConnection) error {
	if !m.isControllerVManage() {
		return fmt.Errorf("can't create URLFilering for non-vManage connector")
	}
	if strings.ToLower(appConnectionRequest.GetFrom().GetNetworkDomain().GetKind()) != "vrf" {
		return fmt.Errorf("for ExternalEntities destination source must be of type NetworkDomain, kind VRF")
	}
	vpnSegmentID, err := m.findVRF(ctx, appConnectionRequest.GetFrom().GetNetworkDomain())
	if err != nil {
		return err
	}
	var denylistId, allowlistId string
	name := appConnectionRequest.GetMetadata().GetName()
	description := appConnectionRequest.GetMetadata().GetDescription()

	accessType := "deny"
	matchingPolicies, err := m.findAccessPoliciesForAppConnection(appConnectionRequest)
	if err != nil {
		return err
	}
	if len(matchingPolicies) > 0 {
		accessType = strings.ToLower(matchingPolicies[0].AccessPolicy.GetAccessType())
	}

	// TODO only one matching policy is considered for now
	if accessType == "deny" {
		entries := make([]urldenylist.Entry, 0, len(appConnectionRequest.GetTo().GetExternalEntities()))
		for _, v := range appConnectionRequest.GetTo().GetExternalEntities() {
			entries = append(entries, urldenylist.Entry{
				Pattern: v,
			})
		}
		denylistId, err = m.client.URLDenylist().Create(ctx, &urldenylist.URLDenylistInput{
			Name:        name,
			Description: description,
			Type:        "urlblacklist",
			Entries:     entries,
		})
		if err != nil {
			return err
		}
		m.logger.Infof("Created URLDenylist %s", name)
	} else if accessType == "allow" {
		entries := make([]urlallowlist.Entry, 0, len(appConnectionRequest.GetTo().GetExternalEntities()))
		for _, v := range appConnectionRequest.GetTo().GetExternalEntities() {
			entries = append(entries, urlallowlist.Entry{
				Pattern: v,
			})
		}
		allowlistId, err = m.client.URLAllowlist().Create(ctx, &urlallowlist.URLAllowlistInput{
			Name:        name,
			Description: description,
			Type:        "urlwhitelist",
			Entries:     entries,
		})
		if err != nil {
			return err
		}
		m.logger.Infof("Created UrlAllowlist %s", name)
	} else {
		return fmt.Errorf("unsupported access type, must be: 'allow' or 'deny'")
	}

	urlDefinition := urlfiltering.URLFilteringDefinition{
		Name:        name,
		Type:        "urlFiltering",
		Description: description,
		Definition: urlfiltering.Definition{
			WebCategoriesAction: "allow",
			WebCategories:       []string{"uncategorized"},
			WebReputation:       "moderate-risk",
			UrlAllowlist: urlfiltering.UrlRef{
				Ref: allowlistId,
			},
			UrlDenylist: urlfiltering.UrlRef{
				Ref: denylistId,
			},
			BlockPageAction:   "text",
			BlockPageContents: "Access to the requested page has been denied. Please contact your Network Administrator",
			EnableAlerts:      false,
			Alerts:            []string{},
			Logging:           []string{},
			TargetVpns:        []string{vpnSegmentID},
		},
	}

	urlFilteringID, err := m.client.URLFiltering().Create(context.TODO(), &urlDefinition)
	if err != nil {
		return err
	}
	m.logger.Infof("Created URLFiltering %s", name)

	aclDB := &db.AppConnection{
		ID:             id,
		Name:           name,
		Status:         "SUCCESS",
		URLFilteringID: urlFilteringID,
		Config:         appConnectionRequest,
	}
	return m.dbClient.UpdateAppConnection(aclDB, aclDB.ID)
}

func (m *AWIConnectionController) findVRF(ctx context.Context, networkDomain *awiGrpc.NetworkDomain) (string, error) {
	if !m.isControllerVManage() {
		return "", fmt.Errorf("can't findVRF, non-vManage connector")
	}
	if strings.ToLower(networkDomain.GetKind()) != "vrf" {
		return "", fmt.Errorf("not a VRF type")
	}
	list, err := m.client.VPN().List(ctx, "")
	if err != nil {
		return "", err
	}
	for _, vpn := range list {
		if networkDomain.GetSelector().GetMatchID().GetId() != "" {
			if vpn.ID == networkDomain.GetSelector().GetMatchID().GetId() ||
				vpn.SegmentID == networkDomain.GetSelector().GetMatchID().GetId() {
				return vpn.SegmentID, nil
			}
		}
		if networkDomain.GetSelector().GetMatchName().GetName() != "" {
			if vpn.SegmentName == networkDomain.GetSelector().GetMatchName().GetName() {
				return vpn.SegmentID, nil
			}
		}
	}
	return "", fmt.Errorf("couldn't find required VRF")
}

func (m *AWIConnectionController) Login() error {
	if !m.isControllerVManage() {
		return fmt.Errorf("not a vManage connector")
	}
	m.client.SetToken("")
	return m.client.Login(context.Background(), m.username, m.password)
}

func (m *AWIConnectionController) RunMapping(deadline time.Duration) {
	if !m.isControllerVManage() {
		return
	}
	statuses := make([]*connection.Status, 0)
	for _, cloud := range m.supportedClouds {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		status, err := m.client.Connection().GetStatus(ctx, cloud)
		if err != nil {
			if errors.Is(err, &client.LoginError{}) {
				if err := m.Login(); err != nil {
					m.logger.Errorf("could not Login: %v", err)
				}
				cancel()
				return
			}
			m.logger.Errorf("could not get status: %v", err)
			cancel()
			return
		}
		statuses = append(statuses, status...)
		cancel()
	}
	if err := m.dbClient.UpdateConnectionStatus(statuses); err != nil {
		m.logger.Errorf("could not update status: %v", err)
		return
	}
	m.logger.Infof("Updated network domain mapping status")
}

func (m *AWIConnectionController) DiscoverNetworkDomains(ctx context.Context) {
	for _, cloud := range m.supportedClouds {
		prov, err := m.strategy.GetProvider(ctx, cloud)
		if err != nil {
			m.logger.Errorf("Failed to get provider for cloud %s: %v", cloud, err)
			continue
		}
		vpcs, err := prov.ListVPC(ctx, &infrapb.ListVPCRequest{})
		if err != nil {
			m.logger.Errorf("Failed to list VPCs for cloud %s: %v", cloud, err)
			continue
		}
		for _, vpc := range vpcs {
			err := m.dbClient.UpdateNetworkDomain(translator.InfraVPCToNetworkDomainDB(&vpc), vpc.ID)
			if err != nil {
				m.logger.Errorf("Failed to update network domain %s %s in database", "vpn", vpc.ID)
			}
		}
	}
	if m.isControllerVManage() {
		vpns, err := m.client.VPN().List(ctx, "")
		if err != nil {
			m.logger.Errorf("Failed to list VPNs: %v", err)
		}
		for _, vpn := range vpns {
			err := m.dbClient.UpdateNetworkDomain(translator.VmanageVPNToNetworkDomainDB(vpn), vpn.ID)
			if err != nil {
				m.logger.Errorf("Failed to update network domain %s %s in database", "vpn", vpn.ID)
			}
		}
	}
	m.logger.Infof("Updated network domains status")
}
