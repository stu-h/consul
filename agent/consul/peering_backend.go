package consul

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/acl/resolver"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/consul/state"
	"github.com/hashicorp/consul/agent/consul/stream"
	"github.com/hashicorp/consul/agent/grpc-external/services/peerstream"
	"github.com/hashicorp/consul/agent/rpc/peering"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/ipaddr"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/proto/pbpeering"
)

type PeeringBackend struct {
	// TODO(peering): accept a smaller interface; maybe just funcs from the server that we actually need: DC, IsLeader, etc
	srv *Server

	leaderAddrLock sync.RWMutex
	leaderAddr     string
}

var _ peering.Backend = (*PeeringBackend)(nil)
var _ peerstream.Backend = (*PeeringBackend)(nil)

// NewPeeringBackend returns a peering.Backend implementation that is bound to the given server.
func NewPeeringBackend(srv *Server) *PeeringBackend {
	return &PeeringBackend{
		srv: srv,
	}
}

// SetLeaderAddress is called on a raft.LeaderObservation in a go routine
// in the consul server; see trackLeaderChanges()
func (b *PeeringBackend) SetLeaderAddress(addr string) {
	b.leaderAddrLock.Lock()
	b.leaderAddr = addr
	b.leaderAddrLock.Unlock()
}

// GetLeaderAddress provides the best hint for the current address of the
// leader. There is no guarantee that this is the actual address of the
// leader.
func (b *PeeringBackend) GetLeaderAddress() string {
	b.leaderAddrLock.RLock()
	defer b.leaderAddrLock.RUnlock()
	return b.leaderAddr
}

// GetTLSMaterials returns the TLS materials for the dialer to dial the acceptor using TLS.
// It returns the server name to validate, and the CA certificate to validate with.
func (b *PeeringBackend) GetTLSMaterials(generatingToken bool) (string, []string, error) {
	if generatingToken {
		if !b.srv.config.ConnectEnabled {
			return "", nil, fmt.Errorf("connect.enabled must be set to true in the server's configuration when generating peering tokens")
		}
		if b.srv.config.GRPCTLSPort <= 0 && !b.srv.tlsConfigurator.GRPCServerUseTLS() {
			return "", nil, fmt.Errorf("TLS for gRPC must be enabled when generating peering tokens")
		}
	}

	roots, err := b.srv.getCARoots(nil, b.srv.fsm.State())
	if err != nil {
		return "", nil, fmt.Errorf("failed to fetch roots: %w", err)
	}
	if len(roots.Roots) == 0 || roots.TrustDomain == "" {
		return "", nil, fmt.Errorf("CA has not finished initializing")
	}

	serverName := connect.PeeringServerSAN(b.srv.config.Datacenter, roots.TrustDomain)

	var caPems []string
	for _, r := range roots.Roots {
		caPems = append(caPems, lib.EnsureTrailingNewline(r.RootCert))
	}

	return serverName, caPems, nil
}

// GetServerAddresses looks up server or mesh gateway addresses from the state store.
func (b *PeeringBackend) GetServerAddresses() ([]string, error) {
	_, rawEntry, err := b.srv.fsm.State().ConfigEntry(nil, structs.MeshConfig, structs.MeshConfigMesh, acl.DefaultEnterpriseMeta())
	if err != nil {
		return nil, fmt.Errorf("failed to read mesh config entry: %w", err)
	}

	meshConfig, ok := rawEntry.(*structs.MeshConfigEntry)
	if ok && meshConfig.Peering != nil && meshConfig.Peering.PeerThroughMeshGateways {
		return meshGatewayAdresses(b.srv.fsm.State())
	}
	return serverAddresses(b.srv.fsm.State())
}

func meshGatewayAdresses(state *state.Store) ([]string, error) {
	_, nodes, err := state.ServiceDump(nil, structs.ServiceKindMeshGateway, true, acl.DefaultEnterpriseMeta(), structs.DefaultPeerKeyword)
	if err != nil {
		return nil, fmt.Errorf("failed to dump gateway addresses: %w", err)
	}

	var addrs []string
	for _, node := range nodes {
		_, addr, port := node.BestAddress(true)
		addrs = append(addrs, ipaddr.FormatAddressPort(addr, port))
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("servers are configured to PeerThroughMeshGateways, but no mesh gateway instances are registered")
	}
	return addrs, nil
}

func serverAddresses(state *state.Store) ([]string, error) {
	_, nodes, err := state.ServiceNodes(nil, "consul", structs.DefaultEnterpriseMetaInDefaultPartition(), structs.DefaultPeerKeyword)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, node := range nodes {
		// Prefer the TLS port if it is defined.
		grpcPortStr := node.ServiceMeta["grpc_tls_port"]
		if v, err := strconv.Atoi(grpcPortStr); err == nil && v > 0 {
			addrs = append(addrs, node.Address+":"+grpcPortStr)
			continue
		}
		// Fallback to the standard port if TLS is not defined.
		grpcPortStr = node.ServiceMeta["grpc_port"]
		if v, err := strconv.Atoi(grpcPortStr); err == nil && v > 0 {
			addrs = append(addrs, node.Address+":"+grpcPortStr)
			continue
		}
		// Skip node if neither defined.
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("a grpc bind port must be specified in the configuration for all servers")
	}
	return addrs, nil
}

// EncodeToken encodes a peering token as a bas64-encoded representation of JSON (for now).
func (b *PeeringBackend) EncodeToken(tok *structs.PeeringToken) ([]byte, error) {
	jsonToken, err := json.Marshal(tok)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal token: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(jsonToken)), nil
}

// DecodeToken decodes a peering token from a base64-encoded JSON byte array (for now).
func (b *PeeringBackend) DecodeToken(tokRaw []byte) (*structs.PeeringToken, error) {
	tokJSONRaw, err := base64.StdEncoding.DecodeString(string(tokRaw))
	if err != nil {
		return nil, fmt.Errorf("failed to decode token: %w", err)
	}
	var tok structs.PeeringToken
	if err := json.Unmarshal(tokJSONRaw, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (s *PeeringBackend) Subscribe(req *stream.SubscribeRequest) (*stream.Subscription, error) {
	return s.srv.publisher.Subscribe(req)
}

func (b *PeeringBackend) Store() peering.Store {
	return b.srv.fsm.State()
}

func (b *PeeringBackend) EnterpriseCheckPartitions(partition string) error {
	return b.enterpriseCheckPartitions(partition)
}

func (b *PeeringBackend) EnterpriseCheckNamespaces(namespace string) error {
	return b.enterpriseCheckNamespaces(namespace)
}

func (b *PeeringBackend) IsLeader() bool {
	return b.srv.IsLeader()
}

func (b *PeeringBackend) CheckPeeringUUID(id string) (bool, error) {
	state := b.srv.fsm.State()
	if _, existing, err := state.PeeringReadByID(nil, id); err != nil {
		return false, err
	} else if existing != nil {
		return false, nil
	}

	return true, nil
}

func (b *PeeringBackend) ValidateProposedPeeringSecret(id string) (bool, error) {
	return b.srv.fsm.State().ValidateProposedPeeringSecretUUID(id)
}

func (b *PeeringBackend) PeeringSecretsWrite(req *pbpeering.SecretsWriteRequest) error {
	_, err := b.srv.raftApplyProtobuf(structs.PeeringSecretsWriteType, req)
	return err
}

func (b *PeeringBackend) PeeringWrite(req *pbpeering.PeeringWriteRequest) error {
	_, err := b.srv.raftApplyProtobuf(structs.PeeringWriteType, req)
	return err
}

// TODO(peering): This needs RPC metrics interceptor since it's not triggered by an RPC.
func (b *PeeringBackend) PeeringTerminateByID(req *pbpeering.PeeringTerminateByIDRequest) error {
	_, err := b.srv.raftApplyProtobuf(structs.PeeringTerminateByIDType, req)
	return err
}

func (b *PeeringBackend) PeeringTrustBundleWrite(req *pbpeering.PeeringTrustBundleWriteRequest) error {
	_, err := b.srv.raftApplyProtobuf(structs.PeeringTrustBundleWriteType, req)
	return err
}

func (b *PeeringBackend) CatalogRegister(req *structs.RegisterRequest) error {
	_, err := b.srv.leaderRaftApply("Catalog.Register", structs.RegisterRequestType, req)
	return err
}

func (b *PeeringBackend) CatalogDeregister(req *structs.DeregisterRequest) error {
	_, err := b.srv.leaderRaftApply("Catalog.Deregister", structs.DeregisterRequestType, req)
	return err
}

func (b *PeeringBackend) ResolveTokenAndDefaultMeta(token string, entMeta *acl.EnterpriseMeta, authzCtx *acl.AuthorizerContext) (resolver.Result, error) {
	return b.srv.ResolveTokenAndDefaultMeta(token, entMeta, authzCtx)
}
