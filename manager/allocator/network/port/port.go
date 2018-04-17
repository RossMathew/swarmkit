package port

import (
	"fmt"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/manager/allocator/network/errors"
)

const (
	// DynamicPortStart is the start of the dynamic port range from which node
	// ports will be allocated when the user did not specify a port.
	DynamicPortStart uint32 = 30000

	// DynamicPortEnd is the end of the dynamic port range from which node
	// ports will be allocated when the user did not specify a port.
	DynamicPortEnd uint32 = 32767

	// The start of master port range which will hold all the allocation state
	// of ports allocated so far regardless of whether it was user defined or
	// not.
	masterPortStart uint32 = 1

	// The end of master port range which will hold all the allocation state of
	// ports allocated so far regardless of whether it was user defined or not.
	masterPortEnd uint32 = 65535
)

// Allocator is the interface for the port allocator, which chooses and keeps
// track of assigned and available port resources.
type Allocator interface {
	Restore([]*api.Endpoint)
	Allocate(*api.Endpoint, *api.EndpointSpec) (Proposal, error)
	Deallocate(*api.Endpoint) Proposal
}

// Allocator is an allocator component that manages the state of which
// ports (and which protocols) are in use in the the cluster.
//
// The Allocator totally owns the Endpoint.Ports field. It provides a slice
// of PortConfigs which should be written directly without modification to the
// the Endpoint.Ports field. Nothing should be modified about Endpoint.Ports
// outside of the port allocator. If this rule is followed, the the port
// allocator doesn't have to check for cases like two services having the same
// port allocated; we created all of the fields, and we can know they are
// consistent.
type allocator struct {
	// ports maps the ports in use. essentially, using map as a set
	ports map[port]struct{}
}

// port is the minimal representation for the Allocator of a single port.
// being composed of 2 numeric types, this type is comparable and can be used
// as a map key
type port struct {
	// protocol represented by this port space
	protocol api.PortConfig_Protocol
	port     uint32
}

// String is a quick implementation of the Stringer interface for the port
// object so that we can pass it to string format calls with just %v
func (p port) String() string {
	return fmt.Sprintf("%v/%v", p.port, p.protocol)
}

// Proposal is the return value of Allocate and DeallocateEndpoint,
// which contains the data needed to affirmatively alter the state of the
// Allocator. It exists so that Allocator changes can be abandoned
// without complex rollback logic. To commit changes to the Allocator, call
// the proposal's Commit function
type Proposal interface {
	// Ports returns the port assignments generated by this proposal
	Ports() []*api.PortConfig

	// Commit commits the result of the Allocator transaction and alters
	// the state of the port allocator. Part of the contract of the
	// Allocator is that it should always return a valid proposal, and so
	// Commit will always succeed and does not need to return an error.
	Commit()

	// IsNoop returns true if the Proposal doesn't actually modify the net
	// allocator state. IsNoop does not mean the ports for an endpoint haven't
	// changed. It only means that the publish ports marked in use by the
	// allocator haven't changed.
	IsNoop() bool
}

type proposal struct {
	pa         *allocator
	ports      []*api.PortConfig
	allocate   map[port]struct{}
	deallocate map[port]struct{}
}

func (prop *proposal) Ports() []*api.PortConfig {
	if prop.ports == nil {
		return []*api.PortConfig{}
	}
	return prop.ports
}

// Commit commits the proposal to the port allocator.
func (prop *proposal) Commit() {
	if prop.IsNoop() {
		// nothing to do if proposal is noop, short circuit a bit
		return
	}
	// The pattern here is we're going to free every port in p.deallocate and
	// then allocate every port in allocate. any overlap results in no net
	// change
	for p := range prop.deallocate {
		delete(prop.pa.ports, p)
	}
	for p := range prop.allocate {
		prop.pa.ports[p] = struct{}{}
	}
}

// IsNoop returns true if the ports in use before this proposal are the same as
// the ports in use after.
func (prop *proposal) IsNoop() bool {
	// if the allocate and deallocate sets are the same length, and every port
	// in allocate is also found in deallocate, then this proposal is a noop
	if len(prop.allocate) != len(prop.deallocate) {
		return false
	}
	for p := range prop.allocate {
		if _, ok := prop.deallocate[p]; !ok {
			return false
		}
	}
	return true
}

// NewAllocator returns a new instance of the Allocator object
func NewAllocator() Allocator {
	return &allocator{
		ports: make(map[port]struct{}),
	}
}

// Restore adds the current endpoints to the local state of the port allocator
// but does not perform any new allocation.
func (pa *allocator) Restore(endpoints []*api.Endpoint) {
	// NOTE(dperny) we can be sure that we're not allocating new or conflicting
	// state because if an endpoint is unallocated, it will not have any ports.
	// we can't look at the Spec in this method, because the spec isn't real
	// state, it's just what the user wants. no matter what changes, that
	// invariant needs to hold.

	// create and commit a proposal for port allocation
	prop := &proposal{
		pa: pa,
		// we only allocate
		allocate: make(map[port]struct{}),
	}
	for _, endpoint := range endpoints {
		for _, p := range endpoint.Ports {
			// ignore host-mode ports
			if p.PublishMode != api.PublishModeHost {
				prop.allocate[port{p.Protocol, p.PublishedPort}] = struct{}{}
			}
		}
	}
	prop.Commit()
}

// Deallocate takes an endpoint and provides a Proposal that will deallocate
// all of its ports.
func (pa *allocator) Deallocate(endpoint *api.Endpoint) Proposal {
	prop := &proposal{
		pa: pa,
		// we only need deallocate
		deallocate: make(map[port]struct{}, len(endpoint.Ports)),
	}
	for _, p := range endpoint.Ports {
		// don't deallocate host-mode ports
		if p.PublishMode == api.PublishModeHost {
			continue
		}
		prop.deallocate[port{p.Protocol, p.PublishedPort}] = struct{}{}
	}
	return prop
}

// Allocate takes an endpoint and a spec and allocates the given ports.
// If any ports are in the endpoint but not in the spec, they are removed. The
// spec is needed because we will compare the ports in the endpoint's spec
// (which will not have changed) with the ports on the spec's endpoint (which
// might have).
//
// At the end of this function, if the allocation would succeed, a Proposal is
// returned with the port assignments, which should be assigned directly to
// endpoint.Ports
//
// Allocate does _not_ change the state of the Allocator. To prevent the caller
// from having to do bulky rollback logic, we return a "Proposal" that has all
// the information required. In order to correctly allocate another endpoint
// with this same Allocator, the caller must later call Commit with the
// returned proposal. If the allocation is abandoned, meaning it's not going to
// be committed to the store and otherwise would need to be rolled back, then
// the proposal can just be ignored.
//
// In the case of dynamically allocated ports, Allocate will try to prefer not
// reassigning the same dynamically allocated port number that is being removed
// to a new port that is being added. However, this behavior _is not tested_
// and should _not_ be relied on as part of the API. It's a nice-to-have
// implementation detail.
//
// Because I'm reluctant to alter what I have that already works, we will not
// return ErrAlreadyAllocated if the endpoint is already allocated. Instead,
// callers can use the AlreadyAllocated function from this package.
func (pa *allocator) Allocate(endpoint *api.Endpoint, spec *api.EndpointSpec) (Proposal, error) {
	// Ok, so Allocate is actually pretty tricky, because we do dynamic
	// port allocation. This means if the user gives us no published port, we
	// pick one for them. When we're updating ports, we have to figure out if
	// the user has *not* updated a dynamically assigned port, so we can use
	// the same port number after the update.
	//
	// The naive solution is to just check if the object has a published port
	// for a given target port, but the spec does not. Then we could just reuse
	// the port on the object, right? Except if the user goes from providing a
	// published port to dynamically assigning a published port. In that case,
	// the actual state of the object will have a published port, but the
	// user's spec wants a new (dynamically allocated) port, just like if we
	// had dynamically assigned the port.
	//
	// Luckily for use, we keep a copy of the spec on the Endpoint object,
	// which we own, which means we have the old endpoint spec around and can
	// compare the user's specs. Then, all we have to do is see if the
	// published port changed.
	if spec == nil {
		spec = &api.EndpointSpec{}
	}

	// So, basically, here's what we're going to do: we're going to create a
	// new list of PortConfigs. This will be the final list that gets put into
	// the object endpoint object
	finalPorts := make([]*api.PortConfig, len(spec.Ports))

	// first, we need to "recover" any dynamically assigned publish ports. to
	// do this:
	//   1. go through all of the ports in the new spec.
	//   2. if a port doesn't have a publish port assigned
	//     a. go through all of the ports in the old, checking if one matches
	//     b. if so, that port has a dynamic port assignment
	//        i. go through every port in the old object's ports. if every
	//           component of the port is the same EXCEPT the PublishedPort,
	//           copy that published port into the port assignments.
	for i, spec := range spec.Ports {
		// check if the published port or target port is off the end of the
		// allowed port range
		if spec.PublishedPort > masterPortEnd {
			return nil, errors.ErrInvalidSpec("published port %v isn't in the valid port range", spec.PublishedPort)
		}
		if spec.TargetPort > masterPortEnd {
			return nil, errors.ErrInvalidSpec("target port %v isn't in the valid port range", spec.TargetPort)
		}
		// copy the port from the spec into the final ports list
		finalPorts[i] = spec.Copy()
		// if the publish mode is host, we're done
		if spec.PublishMode == api.PublishModeHost {
			continue
		}
		// if the published port of the spec is given by the user, we're done
		if spec.PublishedPort != 0 {
			continue
		}
		// check if there is an old spec. If not, we don't have to go looking
		// for a previously assigned dynamic port
		if endpoint.Spec == nil {
			continue
		}
		// go through all of the ports in the old spec, checking if one matches
		for _, old := range endpoint.Spec.Ports {
			if portsEqual(old, spec) {
				// if the do match, find the port from the object
				for _, p := range endpoint.Ports {
					if PortsMostlyEqual(spec, p) {
						// and take it's PublishedPort assignment for the final
						// port assignment
						finalPorts[i].PublishedPort = p.PublishedPort
					}
				}
			}
		}
	}

	// to avoid altering the "real" state of the ports map, we use the proposal
	// object, which contains the proposed changes to the Allocator. this
	// means if there is a failure in the caller after calling Allocate, the
	// caller can discard the changes
	prop := &proposal{
		pa:         pa,
		allocate:   make(map[port]struct{}, len(finalPorts)),
		deallocate: make(map[port]struct{}, len(endpoint.Ports)),
	}

	// now, deallocate everything in the old object
	for _, p := range endpoint.Ports {
		// skip publish mode ports
		if p.PublishMode == api.PublishModeHost {
			continue
		}

		prop.deallocate[port{p.Protocol, p.PublishedPort}] = struct{}{}
	}

	// and then allocate everything in the new object

	// we'll do this in two steps. in the first step, allocate any ports that
	// have a published port assigned already. this first step prevents us from
	// choosing a published port for one port that another, later port wants to
	// use
	for _, p := range finalPorts {
		// Skip all Host ports, which we take no action on
		if p.PublishedPort == 0 || p.PublishMode == api.PublishModeHost {
			continue
		}
		portObj := port{p.Protocol, p.PublishedPort}
		if _, ok := pa.ports[portObj]; ok {
			// check if we're deallocating this port
			if _, ok := prop.deallocate[portObj]; !ok {
				return nil, errors.ErrResourceInUse("port", portObj.String())
			}
		}
		// also check that we haven't already tried to allocate this port for
		// this particular endpoint
		if _, ok := prop.allocate[portObj]; ok {
			return nil, errors.ErrInvalidSpec("published port %v is assigned to more than 1 port config", portObj)
		}

		// now, mark this port as "in use" in the newPorts map.
		prop.allocate[portObj] = struct{}{}
	}

	// now, this second go around, we'll choose all new publish ports to
	// dynamically allocate for any port that doesn't have a publish port yet
ports:
	for _, p := range finalPorts {
		// Again, skip all host ports
		if p.PublishedPort != 0 || p.PublishMode == api.PublishModeHost {
			continue
		}
		portObj := port{p.Protocol, DynamicPortStart}
		// loop through the whole range of dynamic ports and select the first
		// one available
		for i := DynamicPortStart; i <= DynamicPortEnd; i++ {
			portObj.port = i
			if _, ok := pa.ports[portObj]; !ok {
				// also check if the port has been assigned to some other
				// port on this service
				if _, ok := prop.allocate[portObj]; !ok {
					// if the port is not in use, mark it in use, it's our's now,
					// and continue to the next port
					p.PublishedPort = i
					prop.allocate[portObj] = struct{}{}
					continue ports
				}
			}
		}
		// we're out of dynamic ports. check to see if we've deallocated any
		// dynamic ports in this object
		for deallocated := range prop.deallocate {
			// is the protocol the same? is the deallocated port in the dynamic
			// port range?
			if deallocated.protocol == portObj.protocol &&
				DynamicPortStart <= deallocated.port &&
				deallocated.port <= DynamicPortEnd {
				// are not we already reallocating this port?
				if _, ok := prop.allocate[deallocated]; !ok {
					// if all of the above, we can use that published port for
					// this new port and move to the next port
					portObj.port = deallocated.port
					p.PublishedPort = deallocated.port
					prop.allocate[portObj] = struct{}{}
					continue ports
				}
			}
		}

		// if we've gotten all the way through the whole range of dynamic
		// ports, and there are no ports left, return an error
		return nil, errors.ErrResourceExhausted("dynamic port space", "protocol "+p.Protocol.String())
	}

	// add the final ports to the proposal, and we're done
	prop.ports = finalPorts
	return prop, nil
}

// portsEqual determines if the ports in some and other are equivalent.
func portsEqual(some, other *api.PortConfig) bool {
	return PortsMostlyEqual(some, other) && some.PublishedPort == other.PublishedPort
}

// PortsMostlyEqual determines if every component of the ports except the
// PublishedPort are equivalent
func PortsMostlyEqual(some, other *api.PortConfig) bool {
	// short circuit by comparing pointer values. this also handles the case
	// where both of the ports are actually nil
	if some == other {
		return true
	}
	// otherwise, let's check the whole setup.
	// these first two handle the case where either of some or other is nil
	return some != nil && other != nil &&
		// is the name the same?
		some.Name == other.Name &&
		// do the target ports match?
		some.TargetPort == other.TargetPort &&
		// are the protocols the same?
		some.Protocol == other.Protocol &&
		// are the publish modes?
		some.PublishMode == other.PublishMode
}

// AlreadyAllocated returns true if the endpoint's ports are already fully
// allocated
func AlreadyAllocated(endpoint *api.Endpoint, spec *api.EndpointSpec) bool {
	// handle some simple cases involving nil endpoints and spec
	if endpoint == nil && spec == nil {
		return true
	}
	// if the endpoint is nil but the spec is not, that means we haven't
	// allocated yet
	if endpoint == nil && spec != nil {
		return false
	}

	// if the endpoint's spec is nil but the spec is not, then that also means
	// we haven't allocated yet
	if endpoint.Spec == nil && spec != nil {
		return false
	}

	// if the spec is nil, but the endpoint's spec has ports, then that means
	// we have some deallocation to do
	if spec == nil && (endpoint.Spec != nil && len(endpoint.Spec.Ports) > 0) {
		return false
	}
	// that should clear up all of  the nil checks.

	// we're just going to compare equality of the specs. This relies on the
	// behavior that the service's endpoint will always match the embedded
	// EndpointSpec

	// if there are different numbers of ports, then obvious this endpoint
	// isn't fully allocated.
	if len(endpoint.Spec.Ports) != len(spec.Ports) {
		return false
	}

	// check every port in the endpoint's spec against the spec. if there are
	// any ports in the endpoint that aren't in the spec,  then we know this
	// isn't fully allocated
portsLoop:
	for _, port := range endpoint.Spec.Ports {
		for _, specPort := range spec.Ports {
			if portsEqual(port, specPort) {
				continue portsLoop
			}
		}
		// if we get through every spec port, then we have a mismatch and we're
		// not fully allocated
		return false
	}
	// finally if we get ALL the way through, and
	return true
}
