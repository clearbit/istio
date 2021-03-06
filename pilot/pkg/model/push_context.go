// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pkg/features/pilot"
)

// PushContext tracks the status of a push - metrics and errors.
// Metrics are reset after a push - at the beginning all
// values are zero, and when push completes the status is reset.
// The struct is exposed in a debug endpoint - fields public to allow
// easy serialization as json.
type PushContext struct {
	proxyStatusMutex sync.RWMutex
	// ProxyStatus is keyed by the error code, and holds a map keyed
	// by the ID.
	ProxyStatus map[string]map[string]ProxyPushStatus

	// Start represents the time of last config change that reset the
	// push status.
	Start time.Time
	End   time.Time

	// Mutex is used to protect the below store.
	// All data is set when the PushContext object is populated in `InitContext`,
	// data should not be changed by plugins.
	Mutex sync.Mutex `json:"-"`

	// privateServices are reachable within the same namespace.
	privateServicesByNamespace map[string][]*Service
	// publicServices are services reachable within the mesh.
	publicServices []*Service

	privateVirtualServicesByNamespace map[string][]Config
	publicVirtualServices             []Config

	privateDestRuleHostsByNamespace  map[string][]Hostname
	privateDestRuleByHostByNamespace map[string]map[Hostname]*combinedDestinationRule
	publicDestRuleHosts              []Hostname
	publicDestRuleByHost             map[Hostname]*combinedDestinationRule
	////////// END ////////

	// The following data is either a global index or used in the inbound path.
	// Namespace specific views do not apply here.

	// ServiceByHostname has all services, indexed by hostname.
	ServiceByHostname map[Hostname]*Service `json:"-"`

	// AuthzPolicies stores the existing authorization policies in the cluster. Could be nil if there
	// are no authorization policies in the cluster.
	AuthzPolicies *AuthorizationPolicies `json:"-"`

	// Env has a pointer to the shared environment used to create the snapshot.
	Env *Environment `json:"-"`

	// ServicePort2Name is used to keep track of service name and port mapping.
	// This is needed because ADS names use port numbers, while endpoints use
	// port names. The key is the service name. If a service or port are not found,
	// the endpoint needs to be re-evaluated later (eventual consistency)
	ServicePort2Name map[string]PortList `json:"-"`

	initDone bool
}

// XDSUpdater is used for direct updates of the xDS model and incremental push.
// Pilot uses multiple registries - for example each K8S cluster is a registry instance,
// as well as consul and future EDS or MCP sources. Each registry is responsible for
// tracking a set of endpoints associated with mesh services, and calling the EDSUpdate
// on changes. A registry may group endpoints for a service in smaller subsets - for
// example by deployment, or to deal with very large number of endpoints for a service.
// We want to avoid passing around large objects - like full list of endpoints for a registry,
// or the full list of endpoints for a service across registries, since it limits scalability.
//
// Future optimizations will include grouping the endpoints by labels, gateway or region to
// reduce the time when subsetting or split-horizon is used. This design assumes pilot
// tracks all endpoints in the mesh and they fit in RAM - so limit is few M endpoints.
// It is possible to split the endpoint tracking in future.
type XDSUpdater interface {

	// EDSUpdate is called when the list of endpoints or labels in a ServiceEntry is
	// changed. For each cluster and hostname, the full list of active endpoints (including empty list)
	// must be sent. The shard name is used as a key - current implementation is using the registry
	// name.
	EDSUpdate(shard, hostname string, entry []*IstioEndpoint) error

	// SvcUpdate is called when a service port mapping definition is updated.
	// This interface is WIP - labels, annotations and other changes to service may be
	// updated to force a EDS and CDS recomputation and incremental push, as it doesn't affect
	// LDS/RDS.
	SvcUpdate(shard, hostname string, ports map[string]uint32, rports map[uint32]string)

	// WorkloadUpdate is called by a registry when the labels or annotations on a workload have changed.
	// The 'id' is the IP address of the pod for k8s if the pod is in the main/default network.
	// In future it will include the 'network id' for pods in a different network, behind a zvpn gate.
	// The IP is used because K8S Endpoints object associated with a Service only include the IP.
	// We use Endpoints to track the membership to a service and readiness.
	WorkloadUpdate(id string, labels map[string]string, annotations map[string]string)

	// ConfigUpdate is called to notify the XDS server of config updates and request a push.
	// The requests may be collapsed and throttled.
	// This replaces the 'cache invalidation' model.
	ConfigUpdate(full bool)
}

// ProxyPushStatus represents an event captured during config push to proxies.
// It may contain additional message and the affected proxy.
type ProxyPushStatus struct {
	Proxy   string `json:"proxy,omitempty"`
	Message string `json:"message,omitempty"`
}

// PushMetric wraps a prometheus metric.
type PushMetric struct {
	Name  string
	gauge prometheus.Gauge
}

type combinedDestinationRule struct {
	subsets map[string]struct{} // list of subsets seen so far
	// We are not doing ports
	config *Config
}

func newPushMetric(name, help string) *PushMetric {
	pm := &PushMetric{
		gauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: name,
			Help: help,
		}),
		Name: name,
	}
	prometheus.MustRegister(pm.gauge)
	metrics = append(metrics, pm)
	return pm
}

// Add will add an case to the metric.
func (ps *PushContext) Add(metric *PushMetric, key string, proxy *Proxy, msg string) {
	if ps == nil {
		log.Infof("Metric without context %s %v %s", key, proxy, msg)
		return
	}
	ps.proxyStatusMutex.Lock()
	defer ps.proxyStatusMutex.Unlock()

	metricMap, f := ps.ProxyStatus[metric.Name]
	if !f {
		metricMap = map[string]ProxyPushStatus{}
		ps.ProxyStatus[metric.Name] = metricMap
	}
	ev := ProxyPushStatus{Message: msg}
	if proxy != nil {
		ev.Proxy = proxy.ID
	}
	metricMap[key] = ev
}

var (

	// EndpointNoPod tracks endpoints without an associated pod. This is an error condition, since
	// we can't figure out the labels. It may be a transient problem, if endpoint is processed before
	// pod.
	EndpointNoPod = newPushMetric(
		"endpoint_no_pod",
		"Endpoints without an associated pod.",
	)

	// ProxyStatusNoService represents proxies not selected by any service
	// This can be normal - for workloads that act only as client, or are not covered by a Service.
	// It can also be an error, for example in cases the Endpoint list of a service was not updated by the time
	// the sidecar calls.
	// Updated by GetProxyServiceInstances
	ProxyStatusNoService = newPushMetric(
		"pilot_no_ip",
		"Pods not found in the endpoint table, possibly invalid.",
	)

	// ProxyStatusEndpointNotReady represents proxies found not be ready.
	// Updated by GetProxyServiceInstances. Normal condition when starting
	// an app with readiness, error if it doesn't change to 0.
	ProxyStatusEndpointNotReady = newPushMetric(
		"pilot_endpoint_not_ready",
		"Endpoint found in unready state.",
	)

	// ProxyStatusConflictOutboundListenerTCPOverHTTP metric tracks number of
	// wildcard TCP listeners that conflicted with existing wildcard HTTP listener on same port
	ProxyStatusConflictOutboundListenerTCPOverHTTP = newPushMetric(
		"pilot_conflict_outbound_listener_tcp_over_current_http",
		"Number of conflicting wildcard tcp listeners with current wildcard http listener.",
	)

	// ProxyStatusConflictOutboundListenerTCPOverTCP metric tracks number of
	// TCP listeners that conflicted with existing TCP listeners on same port
	ProxyStatusConflictOutboundListenerTCPOverTCP = newPushMetric(
		"pilot_conflict_outbound_listener_tcp_over_current_tcp",
		"Number of conflicting tcp listeners with current tcp listener.",
	)

	// ProxyStatusConflictOutboundListenerHTTPOverTCP metric tracks number of
	// wildcard HTTP listeners that conflicted with existing wildcard TCP listener on same port
	ProxyStatusConflictOutboundListenerHTTPOverTCP = newPushMetric(
		"pilot_conflict_outbound_listener_http_over_current_tcp",
		"Number of conflicting wildcard http listeners with current wildcard tcp listener.",
	)

	// ProxyStatusConflictInboundListener tracks cases of multiple inbound
	// listeners - 2 services selecting the same port of the pod.
	ProxyStatusConflictInboundListener = newPushMetric(
		"pilot_conflict_inbound_listener",
		"Number of conflicting inbound listeners.",
	)

	// DuplicatedClusters tracks duplicate clusters seen while computing CDS
	DuplicatedClusters = newPushMetric(
		"pilot_duplicate_envoy_clusters",
		"Duplicate envoy clusters caused by service entries with same hostname",
	)

	// ProxyStatusClusterNoInstances tracks clusters (services) without workloads.
	ProxyStatusClusterNoInstances = newPushMetric(
		"pilot_eds_no_instances",
		"Number of clusters without instances.",
	)

	// DuplicatedDomains tracks rejected VirtualServices due to duplicated hostname.
	DuplicatedDomains = newPushMetric(
		"pilot_vservice_dup_domain",
		"Virtual services with dup domains.",
	)

	// DuplicatedSubsets tracks duplicate subsets that we rejected while merging multiple destination rules for same host
	DuplicatedSubsets = newPushMetric(
		"pilot_destrule_subsets",
		"Duplicate subsets across destination rules for same host",
	)

	// LastPushStatus preserves the metrics and data collected during lasts global push.
	// It can be used by debugging tools to inspect the push event. It will be reset after each push with the
	// new version.
	LastPushStatus *PushContext
	// LastPushMutex will protect the LastPushStatus
	LastPushMutex sync.Mutex

	// All metrics we registered.
	metrics []*PushMetric
)

// NewPushContext creates a new PushContext structure to track push status.
func NewPushContext() *PushContext {
	// TODO: detect push in progress, don't update status if set
	return &PushContext{
		publicServices:                    []*Service{},
		privateServicesByNamespace:        map[string][]*Service{},
		publicVirtualServices:             []Config{},
		privateVirtualServicesByNamespace: map[string][]Config{},
		publicDestRuleByHost:              map[Hostname]*combinedDestinationRule{},
		publicDestRuleHosts:               []Hostname{},
		privateDestRuleByHostByNamespace:  map[string]map[Hostname]*combinedDestinationRule{},
		privateDestRuleHostsByNamespace:   map[string][]Hostname{},

		ServiceByHostname: map[Hostname]*Service{},
		ProxyStatus:       map[string]map[string]ProxyPushStatus{},
		ServicePort2Name:  map[string]PortList{},
		Start:             time.Now(),
	}
}

// JSON implements json.Marshaller, with a lock.
func (ps *PushContext) JSON() ([]byte, error) {
	if ps == nil {
		return []byte{'{', '}'}, nil
	}
	ps.proxyStatusMutex.RLock()
	defer ps.proxyStatusMutex.RUnlock()
	return json.MarshalIndent(ps, "", "    ")
}

// OnConfigChange is called when a config change is detected.
func (ps *PushContext) OnConfigChange() {
	LastPushMutex.Lock()
	LastPushStatus = ps
	LastPushMutex.Unlock()
	ps.UpdateMetrics()
}

// UpdateMetrics will update the prometheus metrics based on the
// current status of the push.
func (ps *PushContext) UpdateMetrics() {
	ps.proxyStatusMutex.RLock()
	defer ps.proxyStatusMutex.RUnlock()

	for _, pm := range metrics {
		mmap, f := ps.ProxyStatus[pm.Name]
		if f {
			pm.gauge.Set(float64(len(mmap)))
		} else {
			pm.gauge.Set(0)
		}
	}
}

// Services returns the list of services that are visible to a Proxy in a given config namespace
func (ps *PushContext) Services(proxy *Proxy) []*Service {
	out := []*Service{}

	// First add private services
	if proxy == nil {
		for _, privateServices := range ps.privateServicesByNamespace {
			out = append(out, privateServices...)
		}
	} else {
		out = append(out, ps.privateServicesByNamespace[proxy.ConfigNamespace]...)
	}

	// Second add public services
	out = append(out, ps.publicServices...)

	return out
}

// UpdateNodeIsolation will update per-node data holding visible services and configs for the node.
// It is called:
// - on connect
// - on config change events (full push)
// - TODO: on-demand events from Envoy
func (ps *PushContext) UpdateNodeIsolation(proxy *Proxy) {
	// For now Router (Gateway) is not using the isolation - the Gateway already has explicit
	// bindings.
	if pilot.NetworkScopes != "" && proxy.Type == Sidecar {
		// Add global namespaces. This may be loaded from mesh config ( after the API is stable and
		// reviewed ), or from an env variable.
		adminNs := strings.Split(pilot.NetworkScopes, ",")
		globalDeps := map[string]bool{}
		for _, ns := range adminNs {
			globalDeps[ns] = true
		}

		proxy.mutex.RLock()
		defer proxy.mutex.RUnlock()
		res := []*Service{}
		for _, s := range ps.publicServices {
			serviceNamespace := s.Attributes.Namespace
			if serviceNamespace == "" {
				res = append(res, s)
			} else if globalDeps[serviceNamespace] || serviceNamespace == proxy.ConfigNamespace {
				res = append(res, s)
			}
		}
		res = append(res, ps.privateServicesByNamespace[proxy.ConfigNamespace]...)
		proxy.serviceDependencies = res

		// TODO: read Gateways,NetworkScopes/etc to populate additional entries
	}
}

// VirtualServices lists all virtual services bound to the specified gateways
// This replaces store.VirtualServices
func (ps *PushContext) VirtualServices(proxy *Proxy, gateways map[string]bool) []Config {
	configs := make([]Config, 0)
	out := make([]Config, 0)

	// filter out virtual services not reachable
	// First private virtual service
	if proxy == nil {
		for _, virtualSvcs := range ps.privateVirtualServicesByNamespace {
			configs = append(configs, virtualSvcs...)
		}
	} else {
		configs = append(configs, ps.privateVirtualServicesByNamespace[proxy.ConfigNamespace]...)
	}
	// Second public virtual service
	configs = append(configs, ps.publicVirtualServices...)

	for _, config := range configs {
		rule := config.Spec.(*networking.VirtualService)
		if len(rule.Gateways) == 0 {
			// This rule applies only to IstioMeshGateway
			if gateways[IstioMeshGateway] {
				out = append(out, config)
			}
		} else {
			for _, g := range rule.Gateways {
				// note: Gateway names do _not_ use wildcard matching, so we do not use Hostname.Matches here
				if gateways[string(ResolveShortnameToFQDN(g, config.ConfigMeta))] {
					out = append(out, config)
					break
				} else if g == IstioMeshGateway && gateways[g] {
					// "mesh" gateway cannot be expanded into FQDN
					out = append(out, config)
					break
				}
			}
		}
	}

	return out
}

// DestinationRule returns a destination rule for a service name in a given domain.
func (ps *PushContext) DestinationRule(proxy *Proxy, hostname Hostname) *Config {
	if proxy == nil {
		for ns, privateDestHosts := range ps.privateDestRuleHostsByNamespace {
			if host, ok := MostSpecificHostMatch(hostname, privateDestHosts); ok {
				return ps.privateDestRuleByHostByNamespace[ns][host].config
			}
		}
		if host, ok := MostSpecificHostMatch(hostname, ps.publicDestRuleHosts); ok {
			return ps.publicDestRuleByHost[host].config
		}
		return nil
	}
	// take private DestinationRule in same namespace first
	if host, ok := MostSpecificHostMatch(hostname, ps.privateDestRuleHostsByNamespace[proxy.ConfigNamespace]); ok {
		return ps.privateDestRuleByHostByNamespace[proxy.ConfigNamespace][host].config
	}

	// if no private rule matched, then match public rule
	if host, ok := MostSpecificHostMatch(hostname, ps.publicDestRuleHosts); ok {
		return ps.publicDestRuleByHost[host].config
	}

	return nil
}

// SubsetToLabels returns the labels associated with a subset of a given service.
func (ps *PushContext) SubsetToLabels(subsetName string, hostname Hostname) LabelsCollection {
	// empty subset
	if subsetName == "" {
		return nil
	}

	config := ps.DestinationRule(nil, hostname)
	if config == nil {
		return nil
	}

	rule := config.Spec.(*networking.DestinationRule)
	for _, subset := range rule.Subsets {
		if subset.Name == subsetName {
			return []Labels{subset.Labels}
		}
	}

	return nil
}

// InitContext will initialize the data structures used for code generation.
// This should be called before starting the push, from the thread creating
// the push context.
func (ps *PushContext) InitContext(env *Environment) error {
	ps.Mutex.Lock()
	defer ps.Mutex.Unlock()
	if ps.initDone {
		return nil
	}
	ps.Env = env
	var err error

	if err = ps.initServiceRegistry(env); err != nil {
		return err
	}

	if err = ps.initVirtualServices(env); err != nil {
		return err
	}

	if err = ps.initDestinationRules(env); err != nil {
		return err
	}

	if err = ps.initAuthorizationPolicies(env); err != nil {
		rbacLog.Errorf("failed to initialize authorization policies: %v", err)
		return err
	}

	// TODO: everything else that is used in config generation - the generation
	// should not have any deps on config store.
	ps.initDone = true
	return nil
}

// Caches list of services in the registry, and creates a map
// of hostname to service
func (ps *PushContext) initServiceRegistry(env *Environment) error {
	services, err := env.Services()
	if err != nil {
		return err
	}
	// Sort the services in order of creation.
	allServices := sortServicesByCreationTime(services)
	for _, s := range allServices {
		ns := s.Attributes.Namespace
		switch s.Attributes.ConfigScope {
		case networking.ConfigScope_PRIVATE:
			ps.privateServicesByNamespace[ns] = append(ps.privateServicesByNamespace[ns], s)
		default:
			ps.publicServices = append(ps.publicServices, s)
		}
		ps.ServiceByHostname[s.Hostname] = s
		ps.ServicePort2Name[string(s.Hostname)] = s.Ports
	}
	return nil
}

// sortServicesByCreationTime sorts the list of services in ascending order by their creation time (if available).
func sortServicesByCreationTime(services []*Service) []*Service {
	sort.SliceStable(services, func(i, j int) bool {
		return services[i].CreationTime.Before(services[j].CreationTime)
	})
	return services
}

// Caches list of virtual services
func (ps *PushContext) initVirtualServices(env *Environment) error {
	vservices, err := env.List(VirtualService.Type, NamespaceAll)
	if err != nil {
		return err
	}

	sortConfigByCreationTime(vservices)

	// convert all shortnames in virtual services into FQDNs
	for _, r := range vservices {
		rule := r.Spec.(*networking.VirtualService)
		// resolve top level hosts
		for i, h := range rule.Hosts {
			rule.Hosts[i] = string(ResolveShortnameToFQDN(h, r.ConfigMeta))
		}
		// resolve gateways to bind to
		for i, g := range rule.Gateways {
			if g != IstioMeshGateway {
				rule.Gateways[i] = string(ResolveShortnameToFQDN(g, r.ConfigMeta))
			}
		}
		// resolve host in http route.destination, route.mirror
		for _, d := range rule.Http {
			for _, m := range d.Match {
				for i, g := range m.Gateways {
					if g != IstioMeshGateway {
						m.Gateways[i] = string(ResolveShortnameToFQDN(g, r.ConfigMeta))
					}
				}
			}
			for _, w := range d.Route {
				w.Destination.Host = string(ResolveShortnameToFQDN(w.Destination.Host, r.ConfigMeta))
			}
			if d.Mirror != nil {
				d.Mirror.Host = string(ResolveShortnameToFQDN(d.Mirror.Host, r.ConfigMeta))
			}
		}
		//resolve host in tcp route.destination
		for _, d := range rule.Tcp {
			for _, m := range d.Match {
				for i, g := range m.Gateways {
					if g != IstioMeshGateway {
						m.Gateways[i] = string(ResolveShortnameToFQDN(g, r.ConfigMeta))
					}
				}
			}
			for _, w := range d.Route {
				w.Destination.Host = string(ResolveShortnameToFQDN(w.Destination.Host, r.ConfigMeta))
			}
		}
		//resolve host in tls route.destination
		for _, tls := range rule.Tls {
			for _, m := range tls.Match {
				for i, g := range m.Gateways {
					if g != IstioMeshGateway {
						m.Gateways[i] = string(ResolveShortnameToFQDN(g, r.ConfigMeta))
					}
				}
			}
			for _, w := range tls.Route {
				w.Destination.Host = string(ResolveShortnameToFQDN(w.Destination.Host, r.ConfigMeta))
			}
		}
	}

	for _, virtualService := range vservices {
		ns := virtualService.Namespace
		rule := virtualService.Spec.(*networking.VirtualService)
		switch rule.ConfigScope {
		case networking.ConfigScope_PRIVATE:
			ps.privateVirtualServicesByNamespace[ns] = append(ps.privateVirtualServicesByNamespace[ns], virtualService)
		default:
			ps.publicVirtualServices = append(ps.publicVirtualServices, virtualService)
		}
	}

	return nil
}

// Split out of DestinationRule expensive conversions - once per push.
func (ps *PushContext) initDestinationRules(env *Environment) error {
	configs, err := env.List(DestinationRule.Type, NamespaceAll)
	if err != nil {
		return err
	}
	ps.SetDestinationRules(configs)
	return nil
}

// SetDestinationRules is updates internal structures using a set of configs.
// Split out of DestinationRule expensive conversions, computed once per push.
// This also allows tests to inject a config without having the mock.
func (ps *PushContext) SetDestinationRules(configs []Config) {
	// Sort by time first. So if two destination rule have top level traffic policies
	// we take the first one.
	sortConfigByCreationTime(configs)
	privateHostsByNamespace := make(map[string][]Hostname, 0)
	privateCombinedDestRuleMap := make(map[string]map[Hostname]*combinedDestinationRule, 0)
	publicHosts := make([]Hostname, 0)
	publicCombinedDestRuleMap := make(map[Hostname]*combinedDestinationRule, 0)

	for i := range configs {
		rule := configs[i].Spec.(*networking.DestinationRule)
		if rule.ConfigScope == networking.ConfigScope_PRIVATE {
			if _, exist := privateCombinedDestRuleMap[configs[i].Namespace]; !exist {
				privateCombinedDestRuleMap[configs[i].Namespace] = map[Hostname]*combinedDestinationRule{}
			}
			privateHostsByNamespace[configs[i].Namespace], _ = ps.combineSingleDestinationRule(
				privateHostsByNamespace[configs[i].Namespace],
				privateCombinedDestRuleMap[configs[i].Namespace],
				configs[i])

		} else {
			publicHosts, _ = ps.combineSingleDestinationRule(
				publicHosts,
				publicCombinedDestRuleMap,
				configs[i])
		}
	}

	// presort it so that we don't sort it for each DestinationRule call.
	for ns := range privateHostsByNamespace {
		sort.Sort(Hostnames(privateHostsByNamespace[ns]))
	}
	ps.privateDestRuleHostsByNamespace = privateHostsByNamespace
	ps.privateDestRuleByHostByNamespace = privateCombinedDestRuleMap
	sort.Sort(Hostnames(publicHosts))
	ps.publicDestRuleHosts = publicHosts
	ps.publicDestRuleByHost = publicCombinedDestRuleMap
}

func (ps *PushContext) initAuthorizationPolicies(env *Environment) error {
	var err error
	if ps.AuthzPolicies, err = NewAuthzPolicies(env); err != nil {
		rbacLog.Errorf("failed to initialize authorization policies: %v", err)
		return err
	}
	return nil
}
