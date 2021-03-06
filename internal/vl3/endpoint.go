package vl3

import (
	"context"
	"sync"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/ligato/vpp-agent/api/models/vpp"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/connection"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/connection/mechanisms/memif"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/connectioncontext"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/networkservice"
	"github.com/networkservicemesh/networkservicemesh/controlplane/api/registry"
	"github.com/networkservicemesh/networkservicemesh/sdk/client"
	"github.com/networkservicemesh/networkservicemesh/sdk/common"
	"github.com/networkservicemesh/networkservicemesh/sdk/endpoint"
	"github.com/sirupsen/logrus"

	"github.com/danielvladco/k8s-vnet/internal/cnf"
	"github.com/danielvladco/k8s-vnet/internal/ipam"
	"github.com/danielvladco/k8s-vnet/internal/vppagent"
)

const LabelNseSource = "vl3Nse/nseSource/endpointName"

type vL3PeerState int

const (
	PeerStateNotConn vL3PeerState = iota
	PeerStateConn
	PeerStateConnErr
	PeerStateConnInProg
	PeerStateConnRx
)

type vL3NsePeer struct {
	sync.RWMutex
	endpointName              string
	networkServiceManagerName string
	state                     vL3PeerState
	connHdl                   *connection.Connection
	connErr                   error
	excludedPrefixes          []string
	remoteIp                  string
}

type vL3ConnectComposite struct {
	sync.RWMutex

	myEndpointName    string
	nsConfig          *common.NSConfiguration
	remoteNsIpList    []string
	ipamCidr          string
	vl3NsePeers       map[string]*vL3NsePeer
	nsDiscoveryClient registry.NetworkServiceDiscoveryClient
	nsmClient         *client.NsmClient
	ipamEndpoint      *endpoint.IpamEndpoint
	backend           vppagent.Service
	myNseNameFunc     fnGetNseName
}

type fnGetNseName func() string

func (vxc *vL3ConnectComposite) addPeer(endpointName, networkServiceManagerName, remoteIp string) *vL3NsePeer {
	vxc.Lock()
	defer vxc.Unlock()
	_, ok := vxc.vl3NsePeers[endpointName]
	if !ok {
		vxc.vl3NsePeers[endpointName] = &vL3NsePeer{
			endpointName:              endpointName,
			networkServiceManagerName: networkServiceManagerName,
			state:                     PeerStateNotConn,
			remoteIp:                  remoteIp,
		}
	}
	return vxc.vl3NsePeers[endpointName]
}
func (vxc *vL3ConnectComposite) SetMyNseName(request *networkservice.NetworkServiceRequest) {
	vxc.Lock()
	defer vxc.Unlock()
	if vxc.myEndpointName == "" {
		nseName := vxc.myNseNameFunc()
		logrus.Infof("Setting vL3connect composite endpoint name to \"%s\"--req contains \"%s\"", nseName, request.GetConnection().GetNetworkServiceEndpointName())
		if request.GetConnection().GetNetworkServiceEndpointName() != "" {
			vxc.myEndpointName = request.GetConnection().GetNetworkServiceEndpointName()
		} else {
			vxc.myEndpointName = nseName
		}
	}
}

func (vxc *vL3ConnectComposite) GetMyNseName() string {
	vxc.Lock()
	defer vxc.Unlock()
	return vxc.myEndpointName
}

func (vxc *vL3ConnectComposite) processPeerRequest(vl3SrcEndpointName string, request *networkservice.NetworkServiceRequest, incoming *connection.Connection) error {
	logrus.Infof("vL3ConnectComposite received connection request from vL3 NSE %s", vl3SrcEndpointName)
	peer := vxc.addPeer(vl3SrcEndpointName, request.GetConnection().GetSourceNetworkServiceManagerName(), "")
	peer.Lock()
	defer peer.Unlock()
	logrus.WithFields(logrus.Fields{
		"endpointName":              peer.endpointName,
		"networkServiceManagerName": peer.networkServiceManagerName,
		"prior_state":               peer.state,
		"new_state":                 PeerStateConnRx,
	}).Infof("vL3ConnectComposite vl3 NSE peer %s added", vl3SrcEndpointName)
	peer.excludedPrefixes = removeDuplicates(append(peer.excludedPrefixes, incoming.Context.IpContext.ExcludedPrefixes...))
	incoming.Context.IpContext.ExcludedPrefixes = peer.excludedPrefixes
	peer.connHdl = request.GetConnection()

	/* tell my peer to route to me for my ipamCIDR */
	mySubnetRoute := connectioncontext.Route{
		Prefix: vxc.ipamCidr,
	}
	incoming.Context.IpContext.DstRoutes = append(incoming.Context.IpContext.DstRoutes, &mySubnetRoute)
	peer.state = PeerStateConnRx
	return nil
}

func (vxc *vL3ConnectComposite) Request(ctx context.Context,
	request *networkservice.NetworkServiceRequest) (*connection.Connection, error) {
	logger := logrus.New() // endpoint.Log(ctx)
	logger.WithFields(logrus.Fields{
		"endpointName":              request.GetConnection().GetNetworkServiceEndpointName(),
		"networkServiceManagerName": request.GetConnection().GetSourceNetworkServiceManagerName(),
	}).Infof("vL3ConnectComposite Request handler")
	//var err error
	/* NOTE: for IPAM we assume there's no IPAM endpoint in the composite endpoint list */
	/* -we are taking care of that here in this handler */
	/*incoming, err := vxc.GetNext().Request(ctx, request)
	if err != nil {
		logrus.Error(err)
		return nil, err
	}*/

	if vl3SrcEndpointName, ok := request.GetConnection().GetLabels()[LabelNseSource]; ok {
		// request is from another vl3 NSE
		_ = vxc.processPeerRequest(vl3SrcEndpointName, request, request.Connection)

	} else {
		/* set NSC route to this NSE for full vL3 CIDR */
		nscVL3Route := connectioncontext.Route{
			Prefix: vxc.nsConfig.IPAddress,
		}
		request.Connection.Context.IpContext.DstRoutes = append(request.Connection.Context.IpContext.DstRoutes, &nscVL3Route)

		vxc.SetMyNseName(request)
		logger.Infof("vL3ConnectComposite serviceRegistry.DiscoveryClient")
		if vxc.nsDiscoveryClient == nil {
			logger.Error("nsDiscoveryClient is nil")
		} else {
			/* Find all NSEs registered as the same type as this one */
			req := &registry.FindNetworkServiceRequest{
				NetworkServiceName: request.GetConnection().GetNetworkService(),
			}
			logger.Infof("vL3ConnectComposite FindNetworkService for NS=%s", request.GetConnection().GetNetworkService())
			response, err := vxc.nsDiscoveryClient.FindNetworkService(context.Background(), req)
			if err != nil {
				logger.Error(err)
			} else {
				logger.Infof("vL3ConnectComposite found network service; processing endpoints")
				go vxc.processNsEndpoints(context.TODO(), response, "")
			}
			vxc.nsmClient.Configuration.OutgoingNscName = req.NetworkServiceName
			logger.Infof("vL3ConnectComposite check remotes for endpoints")
			for _, remoteIp := range vxc.remoteNsIpList {
				req.NetworkServiceName = req.NetworkServiceName + "@" + remoteIp
				logger.Infof("vL3ConnectComposite querying remote NS %s", req.NetworkServiceName)
				response, err := vxc.nsDiscoveryClient.FindNetworkService(context.Background(), req)
				if err != nil {
					logger.Error(err)
				} else {
					logger.Infof("vL3ConnectComposite found network service; processing endpoints from remote %s", remoteIp)
					go vxc.processNsEndpoints(context.TODO(), response, remoteIp)
				}
			}
		}
	}
	logger.Infof("vL3ConnectComposite request done")
	//return incoming, nil
	if endpoint.Next(ctx) != nil {
		return endpoint.Next(ctx).Request(ctx, request)
	}
	return request.GetConnection(), nil
}

func (vxc *vL3ConnectComposite) Close(ctx context.Context, conn *connection.Connection) (*empty.Empty, error) {
	// remove from connections
	// TODO: should we be removing all peer connections here or no?
	if endpoint.Next(ctx) != nil {
		return endpoint.Next(ctx).Close(ctx, conn)
	}
	return &empty.Empty{}, nil
}

// Name returns the composite name
func (vxc *vL3ConnectComposite) Name() string {
	return "vL3 NSE"
}

func (vxc *vL3ConnectComposite) processNsEndpoints(ctx context.Context, response *registry.FindNetworkServiceResponse, remoteIp string) error {
	/* TODO: For NSs with multiple endpoint types how do we know their type?
	   - do we need to match the name portion?  labels?
	*/
	// just create a new logger for this go thread
	logger := logrus.New()
	for _, vl3endpoint := range response.GetNetworkServiceEndpoints() {
		if vl3endpoint.GetName() != vxc.GetMyNseName() {
			logger.Infof("Found vL3 service %s peer %s", vl3endpoint.NetworkServiceName,
				vl3endpoint.GetName())
			peer := vxc.addPeer(vl3endpoint.GetName(), vl3endpoint.NetworkServiceManagerName, remoteIp)
			peer.Lock()
			//peer.excludedPrefixes = removeDuplicates(append(peer.excludedPrefixes, incoming.Context.IpContext.ExcludedPrefixes...))
			err := vxc.ConnectPeerEndpoint(ctx, peer, logger)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"peerEndpoint": vl3endpoint.GetName(),
				}).Errorf("Failed to connect to vL3 Peer")
			} else {
				if peer.connHdl != nil {
					logger.WithFields(logrus.Fields{
						"peerEndpoint":         vl3endpoint.GetName(),
						"srcIP":                peer.connHdl.Context.IpContext.SrcIpAddr,
						"ConnExcludedPrefixes": peer.connHdl.Context.IpContext.ExcludedPrefixes,
						"peerExcludedPrefixes": peer.excludedPrefixes,
						"peer.DstRoutes":       peer.connHdl.Context.IpContext.DstRoutes,
					}).Infof("Connected to vL3 Peer")
				} else {
					logger.WithFields(logrus.Fields{
						"peerEndpoint":         vl3endpoint.GetName(),
						"peerExcludedPrefixes": peer.excludedPrefixes,
					}).Infof("Connected to vL3 Peer but connhdl == nil")
				}
			}
			peer.Unlock()
		} else {
			logger.Infof("Found my vL3 service %s instance endpoint name: %s", vl3endpoint.NetworkServiceName,
				vl3endpoint.GetName())
		}
	}
	return nil
}

func (vxc *vL3ConnectComposite) createPeerConnectionRequest(ctx context.Context, peer *vL3NsePeer, routes []string, logger logrus.FieldLogger) error {
	/* expected to be called with peer.Lock() */
	if peer.state == PeerStateConn || peer.state == PeerStateConnInProg {
		logger.WithFields(logrus.Fields{
			"peer.Endpoint": peer.endpointName,
		}).Infof("Already connected to peer")
		return peer.connErr
	}
	peer.state = PeerStateConnInProg
	logger.WithFields(logrus.Fields{
		"peer.Endpoint": peer.endpointName,
	}).Infof("Performing connect to peer")
	dpconfig := &vpp.ConfigData{}
	peer.connHdl, peer.connErr = vxc.performPeerConnectRequest(ctx, peer, routes, dpconfig, logger)
	if peer.connErr != nil {
		logger.WithFields(logrus.Fields{
			"peer.Endpoint": peer.endpointName,
		}).Errorf("NSE peer connection failed - %v", peer.connErr)
		peer.state = PeerStateConnErr
		return peer.connErr
	}

	if peer.connErr = vxc.backend.ProcessClientDP(ctx, &vppagent.ProcessDataPlaneReq{
		Vppconfig:   dpconfig,
		ServiceName: "",
		Ifname:      peer.endpointName,
		Connection:  peer.connHdl,
	}); peer.connErr != nil {
		logger.Errorf("endpoint %s Error processing dpconfig: %+v -- %v", peer.endpointName, dpconfig, peer.connErr)
		peer.state = PeerStateConnErr
		return peer.connErr
	}

	peer.state = PeerStateConn
	logger.WithFields(logrus.Fields{
		"peer.Endpoint": peer.endpointName,
	}).Infof("Done with connect to peer")
	return nil
}

func (vxc *vL3ConnectComposite) performPeerConnectRequest(ctx context.Context, peer *vL3NsePeer, routes []string, dpconfig interface{}, logger logrus.FieldLogger) (*connection.Connection, error) {
	/* expected to be called with peer.Lock() */
	ifName := peer.endpointName
	vxc.nsmClient.OutgoingNscLabels[LabelNseSource] = vxc.GetMyNseName()
	conn, err := vxc.nsmClient.ConnectToEndpoint(ctx, peer.remoteIp, peer.endpointName, peer.networkServiceManagerName, ifName, memif.MECHANISM, "VPP interface "+ifName, routes)
	if err != nil {
		logger.Errorf("Error creating %s: %v", ifName, err)
		return nil, err
	}

	//err = vxc.backend.ProcessClient(dpconfig, ifName, conn)

	return conn, nil
}

func (vxc *vL3ConnectComposite) ConnectPeerEndpoint(ctx context.Context, peer *vL3NsePeer, logger logrus.FieldLogger) error {
	/* expected to be called with peer.Lock() */
	// build connection object
	// perform remote networkservice request
	state := peer.state
	logger.WithFields(logrus.Fields{
		"endpointName":              peer.endpointName,
		"networkServiceManagerName": peer.networkServiceManagerName,
		"state":                     state,
	}).Info("newVL3Connect ConnectPeerEndpoint")

	switch state {
	case PeerStateNotConn:
		// TODO do connection request
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("request remote connection")
		routes := []string{vxc.ipamCidr}
		return vxc.createPeerConnectionRequest(ctx, peer, routes, logger)
	case PeerStateConn:
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("remote connection already established")
	case PeerStateConnErr:
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("remote connection attempted prior and errored")
	case PeerStateConnInProg:
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("remote connection in progress")
	case PeerStateConnRx:
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("remote connection already established--rx from peer")
	default:
		logger.WithFields(logrus.Fields{
			"endpointName":              peer.endpointName,
			"networkServiceManagerName": peer.networkServiceManagerName,
		}).Info("remote connection state unknown")
	}
	return nil
}

func removeDuplicates(elements []string) []string {
	encountered := map[string]bool{}
	result := []string{}

	for v := range elements {
		if !encountered[elements[v]] {
			encountered[elements[v]] = true
			result = append(result, elements[v])
		}
	}
	return result
}

// NewVppAgentComposite creates a new VPP Agent composite
func MakeNewVL3Endpoint(ipamCidrGen ipam.PrefixPoolGenerator, backend vppagent.Service, remoteIpList []string, nsDiscoveryClient registry.NetworkServiceDiscoveryClient) cnf.CompositeEndpointFactory {
	return func(nsConfig *common.NSConfiguration, serviceEndpointName *string) (networkservice.NetworkServiceServer, error) {
		ipamCidr, err := ipamCidrGen(nsConfig)
		if err != nil {
			return nil, err
		}

		// ensure the env variables are processed
		if nsConfig == nil {
			nsConfig = &common.NSConfiguration{}
			nsConfig.FromEnv()
		}

		//nsConfig := nsConfig
		nsConfig.OutgoingNscLabels = ""
		nsmClient, err := client.NewNSMClient(context.TODO(), nsConfig)
		if err != nil {
			logrus.Errorf("Unable to create the NSM client %v", err)
		}

		newVL3ConnectComposite := &vL3ConnectComposite{
			nsConfig:          nsConfig,
			remoteNsIpList:    remoteIpList,
			ipamCidr:          ipamCidr,
			myEndpointName:    "",
			vl3NsePeers:       make(map[string]*vL3NsePeer),
			nsDiscoveryClient: nsDiscoveryClient,
			nsmClient:         nsmClient,
			backend:           backend,
			myNseNameFunc: func() string {
				if serviceEndpointName != nil {
					return *serviceEndpointName
				}
				logrus.Errorf("service endpoint name is not set")
				return ""
			},
		}

		logrus.Infof("newVL3ConnectComposite returning")

		return newVL3ConnectComposite, nil
	}
}
