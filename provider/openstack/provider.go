// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// Stub provider for OpenStack, using goose will be implemented here

package openstack

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/rand"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/clock"
	"github.com/juju/errors"
	"github.com/juju/jsonschema"
	"github.com/juju/loggo"
	"github.com/juju/retry"
	"github.com/juju/utils"
	"github.com/juju/version"
	"gopkg.in/goose.v2/cinder"
	"gopkg.in/goose.v2/client"
	gooseerrors "gopkg.in/goose.v2/errors"
	"gopkg.in/goose.v2/identity"
	gooselogging "gopkg.in/goose.v2/logging"
	"gopkg.in/goose.v2/neutron"
	"gopkg.in/goose.v2/nova"
	"gopkg.in/juju/names.v3"

	"github.com/juju/juju/cloud"
	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/cloudconfig/providerinit"
	"github.com/juju/juju/cmd/juju/interact"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/instance"
	corenetwork "github.com/juju/juju/core/network"
	"github.com/juju/juju/core/status"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/context"
	"github.com/juju/juju/environs/instances"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/environs/tags"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/tools"
)

var logger = loggo.GetLogger("juju.provider.openstack")

type EnvironProvider struct {
	environs.ProviderCredentials
	Configurator      ProviderConfigurator
	FirewallerFactory FirewallerFactory
	FlavorFilter      FlavorFilter

	// NetworkingDecorator, if non-nil, will be used to
	// decorate the default networking implementation.
	// This can be used to override behaviour.
	NetworkingDecorator NetworkingDecorator

	// ClientFromEndpoint returns an Openstack client for the given endpoint.
	ClientFromEndpoint func(endpoint string) client.AuthenticatingClient
}

var (
	_ environs.CloudEnvironProvider = (*EnvironProvider)(nil)
	_ environs.ProviderSchema       = (*EnvironProvider)(nil)
)

var providerInstance = &EnvironProvider{
	ProviderCredentials: OpenstackCredentials{},
	Configurator:        &defaultConfigurator{},
	FirewallerFactory:   &firewallerFactory{},
	FlavorFilter:        FlavorFilterFunc(AcceptAllFlavors),
	NetworkingDecorator: nil,
	ClientFromEndpoint:  newGooseClient,
}

var cloudSchema = &jsonschema.Schema{
	Type:     []jsonschema.Type{jsonschema.ObjectType},
	Required: []string{cloud.EndpointKey, cloud.AuthTypesKey, cloud.RegionsKey},
	Order:    []string{cloud.EndpointKey, cloud.CertFilenameKey, cloud.AuthTypesKey, cloud.RegionsKey},
	Properties: map[string]*jsonschema.Schema{
		cloud.EndpointKey: {
			Singular: "the API endpoint url for the cloud",
			Type:     []jsonschema.Type{jsonschema.StringType},
			Format:   jsonschema.FormatURI,
			Default:  "",
			EnvVars:  []string{"OS_AUTH_URL"},
		},
		cloud.CertFilenameKey: {
			Singular:      "a path to the CA certificate for your cloud if one is required to access it. (optional)",
			Type:          []jsonschema.Type{jsonschema.StringType},
			Format:        interact.FormatCertFilename,
			Default:       "",
			PromptDefault: "none",
			EnvVars:       []string{"OS_CACERT"},
		},
		cloud.AuthTypesKey: {
			Singular:    "auth type",
			Plural:      "auth types",
			Type:        []jsonschema.Type{jsonschema.ArrayType},
			UniqueItems: jsonschema.Bool(true),
			Items: &jsonschema.ItemSpec{
				Schemas: []*jsonschema.Schema{{
					Type: []jsonschema.Type{jsonschema.StringType},
					Enum: []interface{}{
						string(cloud.AccessKeyAuthType),
						string(cloud.UserPassAuthType),
					},
				}},
			},
		},
		cloud.RegionsKey: {
			Type:     []jsonschema.Type{jsonschema.ObjectType},
			Singular: "region",
			Plural:   "regions",
			Default:  "",
			EnvVars:  []string{"OS_REGION_NAME"},
			AdditionalProperties: &jsonschema.Schema{
				Type:          []jsonschema.Type{jsonschema.ObjectType},
				Required:      []string{cloud.EndpointKey},
				MaxProperties: jsonschema.Int(1),
				Properties: map[string]*jsonschema.Schema{
					cloud.EndpointKey: {
						Singular:      "the API endpoint url for the region",
						Type:          []jsonschema.Type{jsonschema.StringType},
						Format:        jsonschema.FormatURI,
						Default:       "",
						PromptDefault: "use cloud api url",
					},
				},
			},
		},
	},
}

var makeServiceURL = client.AuthenticatingClient.MakeServiceURL

// TODO: shortAttempt was kept to a long timeout because Nova needs
// more time than EC2.  Storage delays are handled separately now, and
// perhaps other polling attempts can time out faster.

// shortAttempt is used when polling for short-term events in tests.
var shortAttempt = utils.AttemptStrategy{
	Total: 15 * time.Second,
	Delay: 200 * time.Millisecond,
}

// Version is part of the EnvironProvider interface.
func (EnvironProvider) Version() int {
	return 0
}

func (p EnvironProvider) Open(args environs.OpenParams) (environs.Environ, error) {
	logger.Infof("opening model %q", args.Config.Name())
	uuid := args.Config.UUID()
	namespace, err := instance.NewNamespace(uuid)
	if err != nil {
		return nil, errors.Annotate(err, "creating instance namespace")
	}

	e := &Environ{
		name:         args.Config.Name(),
		uuid:         uuid,
		namespace:    namespace,
		clock:        clock.WallClock,
		configurator: p.Configurator,
		flavorFilter: p.FlavorFilter,
	}

	if err := e.SetConfig(args.Config); err != nil {
		return nil, errors.Trace(err)
	}
	if err := e.SetCloudSpec(args.Cloud); err != nil {
		return nil, errors.Trace(err)
	}

	e.networking, e.firewaller, err = p.getEnvironNetworkingFirewaller(e)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return e, nil
}

// getEnvironNetworkingFirewaller returns Networking and Firewaller for the
// new Environ.  Both require Neutron to be support by the OpenStack cloud,
// so create together.
func (p EnvironProvider) getEnvironNetworkingFirewaller(e *Environ) (Networking, Firewaller, error) {
	// TODO (hml) 2019-12-05
	// We want to ensure a failure if an old nova networking OpenStack is
	// added as a new model to a multi-cloud controller.  However the
	// current OpenStack testservice does not implement EndpointsForRegions(),
	// thus causing failures and panics in the setup of the majority of
	// provider unit tests.  Or a rewrite of code and/or tests.
	// See LP:1855343
	if err := authenticateClient(e.client()); err != nil {
		return nil, nil, errors.Trace(err)
	}
	if !e.supportsNeutron() {
		// This should turn into a failure, left as an Error message for now to help
		// provide context for failing networking calls by this environ.  Previously
		// this was covered by switchingNetworking{} and switchingFirewaller{}.
		logger.Errorf("Using unsupported OpenStack APIs. Neutron networking " +
			"is not supported by this OpenStack cloud.\n  Please use OpenStack Queens or " +
			"newer to maintain compatibility.")
	}
	networking := newNetworking(e)
	if p.NetworkingDecorator != nil {
		var err error
		// The NetworkingDecorator is used by the rackspace provider, which
		// uses a majority of this provider's code.
		networking, err = p.NetworkingDecorator.DecorateNetworking(networking)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
	}
	return networking, p.FirewallerFactory.GetFirewaller(e), nil
}

// DetectRegions implements environs.CloudRegionDetector.
func (EnvironProvider) DetectRegions() ([]cloud.Region, error) {
	// If OS_REGION_NAME and OS_AUTH_URL are both set,
	// return return a region using them.
	creds, err := identity.CredentialsFromEnv()
	if err != nil {
		return nil, errors.Errorf("failed to retrieve credential from env : %v", err)
	}
	if creds.Region == "" {
		return nil, errors.NewNotFound(nil, "OS_REGION_NAME environment variable not set")
	}
	if creds.URL == "" {
		return nil, errors.NewNotFound(nil, "OS_AUTH_URL environment variable not set")
	}
	return []cloud.Region{{
		Name:     creds.Region,
		Endpoint: creds.URL,
	}}, nil
}

// CloudSchema returns the schema for adding new clouds of this type.
func (p EnvironProvider) CloudSchema() *jsonschema.Schema {
	return cloudSchema
}

// Ping tests the connection to the cloud, to verify the endpoint is valid.
func (p EnvironProvider) Ping(ctx context.ProviderCallContext, endpoint string) error {
	c := p.ClientFromEndpoint(endpoint)
	if _, err := c.IdentityAuthOptions(); err != nil {
		handleCredentialError(err, ctx)
		return errors.Wrap(err, errors.Errorf("No Openstack server running at %s", endpoint))
	}
	return nil
}

// newGooseClient is the default function in EnvironProvider.ClientFromEndpoint.
func newGooseClient(endpoint string) client.AuthenticatingClient {
	// Use NonValidatingClient, in case the endpoint is behind a cert
	return client.NewNonValidatingClient(&identity.Credentials{URL: endpoint}, 0, nil)
}

// PrepareConfig is specified in the EnvironProvider interface.
func (p EnvironProvider) PrepareConfig(args environs.PrepareConfigParams) (*config.Config, error) {
	if err := validateCloudSpec(args.Cloud); err != nil {
		return nil, errors.Annotate(err, "validating cloud spec")
	}

	// Set the default block-storage source.
	attrs := make(map[string]interface{})
	if _, ok := args.Config.StorageDefaultBlockSource(); !ok {
		attrs[config.StorageDefaultBlockSourceKey] = CinderProviderType
	}

	cfg, err := args.Config.Apply(attrs)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return cfg, nil
}

// MetadataLookupParams returns parameters which are used to query image metadata to
// find matching image information.
func (p EnvironProvider) MetadataLookupParams(region string) (*simplestreams.MetadataLookupParams, error) {
	if region == "" {
		return nil, errors.Errorf("region must be specified")
	}
	return &simplestreams.MetadataLookupParams{
		Region: region,
	}, nil
}

func (p EnvironProvider) newConfig(cfg *config.Config) (*environConfig, error) {
	valid, err := p.Validate(cfg, nil)
	if err != nil {
		return nil, err
	}
	return &environConfig{valid, valid.UnknownAttrs()}, nil
}

type Environ struct {
	name      string
	uuid      string
	namespace instance.Namespace

	ecfgMutex       sync.Mutex
	ecfgUnlocked    *environConfig
	cloudUnlocked   environs.CloudSpec
	clientUnlocked  client.AuthenticatingClient
	novaUnlocked    *nova.Client
	neutronUnlocked *neutron.Client
	volumeURL       *url.URL

	// keystoneImageDataSource caches the result of getKeystoneImageSource.
	keystoneImageDataSourceMutex sync.Mutex
	keystoneImageDataSource      simplestreams.DataSource

	// keystoneToolsDataSource caches the result of getKeystoneToolsSource.
	keystoneToolsDataSourceMutex sync.Mutex
	keystoneToolsDataSource      simplestreams.DataSource

	availabilityZonesMutex sync.Mutex
	availabilityZones      []common.AvailabilityZone
	firewaller             Firewaller
	networking             Networking
	configurator           ProviderConfigurator
	flavorFilter           FlavorFilter

	// Clock is defined so it can be replaced for testing
	clock clock.Clock

	publicIPMutex sync.Mutex
}

var _ environs.Environ = (*Environ)(nil)
var _ environs.NetworkingEnviron = (*Environ)(nil)
var _ simplestreams.HasRegion = (*Environ)(nil)
var _ context.Distributor = (*Environ)(nil)
var _ environs.InstanceTagger = (*Environ)(nil)

type openstackInstance struct {
	e        *Environ
	instType *instances.InstanceType
	arch     *string

	mu           sync.Mutex
	serverDetail *nova.ServerDetail
	// floatingIP is non-nil iff use-floating-ip is true.
	floatingIP *string

	// runOpts is only set in the response from StartInstance.
	runOpts *nova.RunServerOpts
}

// NovaInstanceStartedWithOpts exposes run options used to start an instance.
// Used by unit testing.
func (inst *openstackInstance) NovaInstanceStartedWithOpts() *nova.RunServerOpts {
	return inst.runOpts
}

func (inst *openstackInstance) String() string {
	return string(inst.Id())
}

var _ instances.Instance = (*openstackInstance)(nil)

func (inst *openstackInstance) Refresh(ctx context.ProviderCallContext) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	server, err := inst.e.nova().GetServer(inst.serverDetail.Id)
	if err != nil {
		handleCredentialError(err, ctx)
		return err
	}
	inst.serverDetail = server
	return nil
}

func (inst *openstackInstance) getServerDetail() *nova.ServerDetail {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.serverDetail
}

func (inst *openstackInstance) Id() instance.Id {
	return instance.Id(inst.getServerDetail().Id)
}

func (inst *openstackInstance) Status(ctx context.ProviderCallContext) instance.Status {
	instStatus := inst.getServerDetail().Status
	var jujuStatus status.Status
	switch instStatus {
	case nova.StatusActive:
		jujuStatus = status.Running
	case nova.StatusError:
		jujuStatus = status.ProvisioningError
	case nova.StatusBuild, nova.StatusBuildSpawning,
		nova.StatusDeleted, nova.StatusHardReboot,
		nova.StatusPassword, nova.StatusReboot,
		nova.StatusRebuild, nova.StatusRescue,
		nova.StatusResize, nova.StatusShutoff,
		nova.StatusSuspended, nova.StatusVerifyResize:
		jujuStatus = status.Empty
	case nova.StatusUnknown:
		jujuStatus = status.Unknown
	default:
		jujuStatus = status.Empty
	}
	return instance.Status{
		Status:  jujuStatus,
		Message: instStatus,
	}
}

func (inst *openstackInstance) hardwareCharacteristics() *instance.HardwareCharacteristics {
	hc := &instance.HardwareCharacteristics{Arch: inst.arch}
	if inst.instType != nil {
		hc.Mem = &inst.instType.Mem
		// openstack is special in that a 0-size root disk means that
		// the root disk will result in an instance with a root disk
		// the same size as the image that created it, so we just set
		// the HardwareCharacteristics to nil to signal that we don't
		// know what the correct size is.
		if inst.instType.RootDisk == 0 {
			hc.RootDisk = nil
		} else {
			hc.RootDisk = &inst.instType.RootDisk
		}
		hc.CpuCores = &inst.instType.CpuCores
		hc.CpuPower = inst.instType.CpuPower
		// tags not currently supported on openstack
	}
	hc.AvailabilityZone = &inst.serverDetail.AvailabilityZone
	// If the instance was started with a volume block device mapping, select the first
	// boot disk as the reported RootDisk size.
	if inst.runOpts != nil {
		for _, blockDevice := range inst.runOpts.BlockDeviceMappings {
			if blockDevice.BootIndex == 0 &&
				blockDevice.DestinationType == rootDiskSourceVolume {
				rootDiskSize := uint64(blockDevice.VolumeSize * 1024)
				hc.RootDisk = &rootDiskSize
				break
			}
		}
	}
	return hc
}

// getAddresses returns the existing server information on addresses,
// but fetches the details over the api again if no addresses exist.
func (inst *openstackInstance) getAddresses(ctx context.ProviderCallContext) (map[string][]nova.IPAddress, error) {
	addrs := inst.getServerDetail().Addresses
	if len(addrs) == 0 {
		server, err := inst.e.nova().GetServer(string(inst.Id()))
		if err != nil {
			handleCredentialError(err, ctx)
			return nil, err
		}
		addrs = server.Addresses
	}
	return addrs, nil
}

// Addresses implements network.Addresses() returning generic address
// details for the instances, and calling the openstack api if needed.
func (inst *openstackInstance) Addresses(ctx context.ProviderCallContext) (corenetwork.ProviderAddresses, error) {
	addresses, err := inst.getAddresses(ctx)
	if err != nil {
		return nil, err
	}
	var floatingIP string
	if inst.floatingIP != nil {
		floatingIP = *inst.floatingIP
		logger.Debugf("instance %v has floating IP address: %v", inst.Id(), floatingIP)
	}
	return convertNovaAddresses(floatingIP, addresses), nil
}

// convertNovaAddresses returns nova addresses in generic format
func convertNovaAddresses(publicIP string, addresses map[string][]nova.IPAddress) corenetwork.ProviderAddresses {
	var machineAddresses []corenetwork.ProviderAddress
	if publicIP != "" {
		publicAddr := corenetwork.NewScopedProviderAddress(publicIP, corenetwork.ScopePublic)
		machineAddresses = append(machineAddresses, publicAddr)
	}
	// TODO(gz) Network ordering may be significant but is not preserved by
	// the map, see lp:1188126 for example. That could potentially be fixed
	// in goose, or left to be derived by other means.
	for netName, ips := range addresses {
		networkScope := corenetwork.ScopeUnknown
		if netName == "public" {
			networkScope = corenetwork.ScopePublic
		}
		for _, address := range ips {
			// If this address has already been added as a floating IP, skip it.
			if publicIP == address.Address {
				continue
			}
			// Assume IPv4 unless specified otherwise
			addrType := corenetwork.IPv4Address
			if address.Version == 6 {
				addrType = corenetwork.IPv6Address
			}
			machineAddr := corenetwork.NewScopedProviderAddress(address.Address, networkScope)
			if machineAddr.Type != addrType {
				logger.Warningf("derived address type %v, nova reports %v", machineAddr.Type, addrType)
			}
			machineAddresses = append(machineAddresses, machineAddr)
		}
	}
	return machineAddresses
}

func (inst *openstackInstance) OpenPorts(ctx context.ProviderCallContext, machineId string, rules []network.IngressRule) error {
	return inst.e.firewaller.OpenInstancePorts(ctx, inst, machineId, rules)
}

func (inst *openstackInstance) ClosePorts(ctx context.ProviderCallContext, machineId string, rules []network.IngressRule) error {
	return inst.e.firewaller.CloseInstancePorts(ctx, inst, machineId, rules)
}

func (inst *openstackInstance) IngressRules(ctx context.ProviderCallContext, machineId string) ([]network.IngressRule, error) {
	return inst.e.firewaller.InstanceIngressRules(ctx, inst, machineId)
}

func (e *Environ) ecfg() *environConfig {
	e.ecfgMutex.Lock()
	ecfg := e.ecfgUnlocked
	e.ecfgMutex.Unlock()
	return ecfg
}

func (e *Environ) cloud() environs.CloudSpec {
	e.ecfgMutex.Lock()
	cloud := e.cloudUnlocked
	e.ecfgMutex.Unlock()
	return cloud
}

func (e *Environ) client() client.AuthenticatingClient {
	e.ecfgMutex.Lock()
	client := e.clientUnlocked
	e.ecfgMutex.Unlock()
	return client
}

func (e *Environ) nova() *nova.Client {
	e.ecfgMutex.Lock()
	nova := e.novaUnlocked
	e.ecfgMutex.Unlock()
	return nova
}

func (e *Environ) neutron() *neutron.Client {
	e.ecfgMutex.Lock()
	neutron := e.neutronUnlocked
	e.ecfgMutex.Unlock()
	return neutron
}

var unsupportedConstraints = []string{
	constraints.Tags,
	constraints.CpuPower,
}

// ConstraintsValidator is defined on the Environs interface.
func (e *Environ) ConstraintsValidator(ctx context.ProviderCallContext) (constraints.Validator, error) {
	validator := constraints.NewValidator()
	validator.RegisterConflicts(
		[]string{constraints.InstanceType},
		[]string{constraints.Mem, constraints.Cores})
	// NOTE: RootDiskSource and RootDisk constraints are validated in PrecheckInstance.
	validator.RegisterUnsupported(unsupportedConstraints)
	novaClient := e.nova()
	flavors, err := novaClient.ListFlavorsDetail()
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, err
	}
	instTypeNames := make([]string, len(flavors))
	for i, flavor := range flavors {
		instTypeNames[i] = flavor.Name
	}
	sort.Strings(instTypeNames)
	validator.RegisterVocabulary(constraints.InstanceType, instTypeNames)
	validator.RegisterVocabulary(constraints.VirtType, []string{"kvm", "lxd"})
	validator.RegisterVocabulary(constraints.RootDiskSource, []string{rootDiskSourceVolume})
	return validator, nil
}

var novaListAvailabilityZones = (*nova.Client).ListAvailabilityZones

type openstackAvailabilityZone struct {
	nova.AvailabilityZone
}

func (z *openstackAvailabilityZone) Name() string {
	return z.AvailabilityZone.Name
}

func (z *openstackAvailabilityZone) Available() bool {
	return z.AvailabilityZone.State.Available
}

// AvailabilityZones returns a slice of availability zones.
func (e *Environ) AvailabilityZones(ctx context.ProviderCallContext) ([]common.AvailabilityZone, error) {
	e.availabilityZonesMutex.Lock()
	defer e.availabilityZonesMutex.Unlock()
	if e.availabilityZones == nil {
		zones, err := novaListAvailabilityZones(e.nova())
		if gooseerrors.IsNotImplemented(err) {
			return nil, errors.NotImplementedf("availability zones")
		}
		if err != nil {
			handleCredentialError(err, ctx)
			return nil, err
		}
		e.availabilityZones = make([]common.AvailabilityZone, len(zones))
		for i, z := range zones {
			e.availabilityZones[i] = &openstackAvailabilityZone{z}
		}
	}
	return e.availabilityZones, nil
}

// InstanceAvailabilityZoneNames returns the availability zone names for each
// of the specified instances.
func (e *Environ) InstanceAvailabilityZoneNames(ctx context.ProviderCallContext, ids []instance.Id) ([]string, error) {
	instances, err := e.Instances(ctx, ids)
	if err != nil && err != environs.ErrPartialInstances {
		handleCredentialError(err, ctx)
		return nil, err
	}
	zones := make([]string, len(instances))
	for i, inst := range instances {
		if inst == nil {
			continue
		}
		zones[i] = inst.(*openstackInstance).serverDetail.AvailabilityZone
	}
	return zones, err
}

type openstackPlacement struct {
	zoneName string
}

// DeriveAvailabilityZones is part of the common.ZonedEnviron interface.
func (e *Environ) DeriveAvailabilityZones(ctx context.ProviderCallContext, args environs.StartInstanceParams) ([]string, error) {
	availabilityZone, err := e.deriveAvailabilityZone(ctx, args.Placement, args.VolumeAttachments)
	if err != nil && !errors.IsNotImplemented(err) {
		handleCredentialError(err, ctx)
		return nil, errors.Trace(err)
	}
	if availabilityZone != "" {
		return []string{availabilityZone}, nil
	}
	return nil, nil
}

func (e *Environ) parsePlacement(ctx context.ProviderCallContext, placement string) (*openstackPlacement, error) {
	pos := strings.IndexRune(placement, '=')
	if pos == -1 {
		return nil, errors.Errorf("unknown placement directive: %v", placement)
	}
	switch key, value := placement[:pos], placement[pos+1:]; key {
	case "zone":
		availabilityZone := value
		err := common.ValidateAvailabilityZone(e, ctx, availabilityZone)
		if err != nil {
			handleCredentialError(err, ctx)
			return nil, err
		}
		return &openstackPlacement{zoneName: availabilityZone}, nil
	}
	return nil, errors.Errorf("unknown placement directive: %v", placement)
}

// PrecheckInstance is defined on the environs.InstancePrechecker interface.
func (e *Environ) PrecheckInstance(ctx context.ProviderCallContext, args environs.PrecheckInstanceParams) error {
	if _, err := e.deriveAvailabilityZone(ctx, args.Placement, args.VolumeAttachments); err != nil {
		return errors.Trace(err)
	}
	usingVolumeRootDisk := false
	if args.Constraints.HasRootDiskSource() && args.Constraints.HasRootDisk() &&
		*args.Constraints.RootDiskSource == rootDiskSourceVolume {
		usingVolumeRootDisk = true
	}
	if args.Constraints.HasRootDisk() && args.Constraints.HasInstanceType() && !usingVolumeRootDisk {
		return errors.Errorf("constraint %s cannot be specified with %s unless constraint %s=%s",
			constraints.RootDisk, constraints.InstanceType,
			constraints.RootDiskSource, rootDiskSourceVolume)
	}
	if args.Constraints.HasInstanceType() {
		// Constraint has an instance-type constraint so let's see if it is valid.
		novaClient := e.nova()
		flavors, err := novaClient.ListFlavorsDetail()
		if err != nil {
			handleCredentialError(err, ctx)
			return err
		}
		flavorFound := false
		for _, flavor := range flavors {
			if flavor.Name == *args.Constraints.InstanceType {
				flavorFound = true
				break
			}
		}
		if !flavorFound {
			return errors.Errorf("invalid Openstack flavour %q specified", *args.Constraints.InstanceType)
		}
	}

	return nil
}

// PrepareForBootstrap is part of the Environ interface.
func (e *Environ) PrepareForBootstrap(ctx environs.BootstrapContext, controllerName string) error {
	// Verify credentials.
	if err := authenticateClient(e.client()); err != nil {
		return err
	}
	if !e.supportsNeutron() {
		logger.Errorf(`Using unsupported OpenStack APIs.

  Neutron networking is not supported by this OpenStack cloud.

  Please use OpenStack Queens or newer to maintain compatibility.

`,
		)
		return errors.NewNotFound(nil, "OpenStack Neutron service")
	}
	return nil
}

// Create is part of the Environ interface.
func (e *Environ) Create(ctx context.ProviderCallContext, args environs.CreateParams) error {
	// Verify credentials.
	if err := authenticateClient(e.client()); err != nil {
		handleCredentialError(err, ctx)
		return err
	}
	// TODO(axw) 2016-08-04 #1609643
	// Create global security group(s) here.
	return nil
}

func (e *Environ) Bootstrap(ctx environs.BootstrapContext, callCtx context.ProviderCallContext, args environs.BootstrapParams) (*environs.BootstrapResult, error) {
	// The client's authentication may have been reset when finding tools if the agent-version
	// attribute was updated so we need to re-authenticate. This will be a no-op if already authenticated.
	// An authenticated client is needed for the URL() call below.
	if err := authenticateClient(e.client()); err != nil {
		handleCredentialError(err, callCtx)
		return nil, err
	}
	result, err := common.Bootstrap(ctx, e, callCtx, args)
	if err != nil {
		handleCredentialError(err, callCtx)
		return nil, err
	}
	return result, nil
}

func (e *Environ) supportsNeutron() bool {
	client := e.client()
	endpointMap := client.EndpointsForRegion(e.cloud().Region)
	_, ok := endpointMap["network"]
	return ok
}

func (e *Environ) ControllerInstances(ctx context.ProviderCallContext, controllerUUID string) ([]instance.Id, error) {
	// Find all instances tagged with tags.JujuIsController.
	instances, err := e.allControllerManagedInstances(ctx, controllerUUID, e.ecfg().useFloatingIP())
	if err != nil {
		return nil, errors.Trace(err)
	}
	ids := make([]instance.Id, 0, 1)
	for _, instance := range instances {
		detail := instance.(*openstackInstance).getServerDetail()
		if detail.Metadata[tags.JujuIsController] == "true" {
			ids = append(ids, instance.Id())
		}
	}
	if len(ids) == 0 {
		return nil, environs.ErrNoInstances
	}
	return ids, nil
}

func (e *Environ) Config() *config.Config {
	return e.ecfg().Config
}

func newCredentials(spec environs.CloudSpec) (identity.Credentials, identity.AuthMode, error) {
	credAttrs := spec.Credential.Attributes()
	cred := identity.Credentials{
		Region:     spec.Region,
		URL:        spec.Endpoint,
		TenantName: credAttrs[CredAttrTenantName],
		TenantID:   credAttrs[CredAttrTenantID],
	}

	// AuthType is validated when the environment is opened, so it's known
	// to be one of these values.
	var authMode identity.AuthMode
	switch spec.Credential.AuthType() {
	case cloud.UserPassAuthType:
		// TODO(axw) we need a way of saying to use legacy auth.
		cred.User = credAttrs[CredAttrUserName]
		cred.Secrets = credAttrs[CredAttrPassword]
		cred.ProjectDomain = credAttrs[CredAttrProjectDomainName]
		cred.UserDomain = credAttrs[CredAttrUserDomainName]
		cred.Domain = credAttrs[CredAttrDomainName]
		if credAttrs[CredAttrVersion] != "" {
			version, err := strconv.Atoi(credAttrs[CredAttrVersion])
			if err != nil {
				return identity.Credentials{}, 0,
					errors.Errorf("cred.Version is not a valid integer type : %v", err)
			}
			if version < 3 {
				authMode = identity.AuthUserPass
			} else {
				authMode = identity.AuthUserPassV3
			}
			cred.Version = version
		} else if cred.Domain != "" || cred.UserDomain != "" || cred.ProjectDomain != "" {
			authMode = identity.AuthUserPassV3
		} else {
			authMode = identity.AuthUserPass
		}
	case cloud.AccessKeyAuthType:
		cred.User = credAttrs[CredAttrAccessKey]
		cred.Secrets = credAttrs[CredAttrSecretKey]
		authMode = identity.AuthKeyPair
	}
	return cred, authMode, nil
}

func authClient(spec environs.CloudSpec, ecfg *environConfig) (client.AuthenticatingClient, error) {
	identityClientVersion, err := identityClientVersion(spec.Endpoint)
	if err != nil {
		return nil, errors.Annotate(err, "cannot create a client")
	}
	cred, authMode, err := newCredentials(spec)
	if err != nil {
		return nil, errors.Annotate(err, "cannot create credential")
	}
	gooseLogger := gooselogging.LoggoLogger{loggo.GetLogger("goose")}

	cl, _ := newClientByType(cred, authMode, gooseLogger, ecfg.SSLHostnameVerification(), spec.CACertificates)

	// before returning, lets make sure that we want to have AuthMode
	// AuthUserPass instead of its V3 counterpart.
	if authMode == identity.AuthUserPass && (identityClientVersion == -1 || identityClientVersion == 3) {
		options, err := cl.IdentityAuthOptions()
		if err != nil {
			logger.Errorf("cannot determine available auth versions %v", err)
		}
		for _, option := range options {
			if option.Mode != identity.AuthUserPassV3 {
				continue
			}
			cred.URL = option.Endpoint
			v3Cl, err := newClientByType(cred, identity.AuthUserPassV3, gooseLogger, ecfg.SSLHostnameVerification(), spec.CACertificates)
			if err != nil {
				return nil, err
			}
			// if the v3 client can authenticate, use it, otherwise fallback to the v2 client.
			if err = v3Cl.Authenticate(); err == nil {
				cl = v3Cl
				break
			}
		}
	}

	// Juju requires "compute" at a minimum. We'll use "network" if it's
	// available in preference to the Neutron network APIs; and "volume" or
	// "volume2" for storage if either one is available.
	cl.SetRequiredServiceTypes([]string{"compute"})
	return cl, nil
}

// newClientByType returns an authenticating client to talk to the
// OpenStack cloud.  CACertificate and SSLHostnameVerification == false
// config options are mutually exclusive here.
func newClientByType(
	cred identity.Credentials,
	authMode identity.AuthMode,
	gooseLogger gooselogging.CompatLogger,
	sslHostnameVerification bool,
	certs []string,
) (client.AuthenticatingClient, error) {
	switch {
	case len(certs) > 0:
		tlsConfig := tlsConfig(certs)
		logger.Tracef("using NewClientTLSConfig")
		return client.NewClientTLSConfig(&cred, authMode, gooseLogger, tlsConfig), nil
	case sslHostnameVerification == false:
		logger.Tracef("using NewNonValidatingClient")
		return client.NewNonValidatingClient(&cred, authMode, gooseLogger), nil
	default:
		logger.Tracef("using NewClient")
		return client.NewClient(&cred, authMode, gooseLogger), nil
	}
}

func tlsConfig(certStrs []string) *tls.Config {
	pool := x509.NewCertPool()
	for _, cert := range certStrs {
		pool.AppendCertsFromPEM([]byte(cert))
	}
	tlsConfig := utils.SecureTLSConfig()
	tlsConfig.RootCAs = pool
	return tlsConfig
}

type authenticator interface {
	Authenticate() error
}

var authenticateClient = func(auth authenticator) error {
	err := auth.Authenticate()
	if err != nil {
		// Log the error in case there are any useful hints,
		// but provide a readable and helpful error message
		// to the user.
		logger.Debugf("Authenticate() failed: %v", err)
		if gooseerrors.IsUnauthorised(err) {
			return errors.Errorf("authentication failed : %v\n"+
				"Please ensure the credentials are correct. A common mistake is\n"+
				"to specify the wrong tenant. Use the OpenStack project name\n"+
				"for tenant-name in your model configuration. \n", err)
		} else {
			return errors.Annotate(err, "authentication failed.")
		}
	}
	return nil
}

func (e *Environ) SetConfig(cfg *config.Config) error {
	ecfg, err := providerInstance.newConfig(cfg)
	if err != nil {
		return err
	}
	// At this point, the authentication method config value has been validated so we extract it's value here
	// to avoid having to validate again each time when creating the OpenStack client.
	e.ecfgMutex.Lock()
	defer e.ecfgMutex.Unlock()
	e.ecfgUnlocked = ecfg

	return nil
}

// SetCloudSpec is specified in the environs.Environ interface.
func (e *Environ) SetCloudSpec(spec environs.CloudSpec) error {
	e.ecfgMutex.Lock()
	defer e.ecfgMutex.Unlock()

	if err := validateCloudSpec(spec); err != nil {
		return errors.Annotate(err, "validating cloud spec")
	}
	e.cloudUnlocked = spec
	client, err := authClient(e.cloudUnlocked, e.ecfgUnlocked)
	if err != nil {
		return errors.Annotate(err, "cannot set config")
	}
	e.clientUnlocked = client
	e.novaUnlocked = nova.New(e.clientUnlocked)
	e.neutronUnlocked = neutron.New(e.clientUnlocked)
	return nil
}

func identityClientVersion(authURL string) (int, error) {
	url, err := url.Parse(authURL)
	if err != nil {
		// Return 0 as this is the lowest invalid number according to openstack codebase:
		// -1 is reserved and has special handling; 1, 2, 3, etc are valid identity client versions.
		return 0, err
	}
	if url.Path == authURL {
		// This means we could not parse URL into url structure
		// with protocols, domain, port, etc.
		// For example, specifying "keystone.foo" instead of "https://keystone.foo:443/v3/"
		// falls into this category.
		return 0, errors.Errorf("url %s is malformed", authURL)
	}
	if url.Path == "" || url.Path == "/" {
		// User explicitly did not provide any version, it is empty.
		return -1, nil
	}
	// The last part of the path should be the version #, prefixed with a 'v' or 'V'
	// Example: https://keystone.foo:443/v3/
	// Example: https://sharedhost.foo:443/identity/v3/
	var tail string
	urlpath := strings.ToLower(url.Path)
	urlpath, tail = path.Split(urlpath)
	if len(tail) == 0 && len(urlpath) > 2 {
		// trailing /, remove it and split again
		_, tail = path.Split(strings.TrimRight(urlpath, "/"))
	}
	versionNumStr := strings.TrimPrefix(tail, "v")
	logger.Tracef("authURL: %s", authURL)
	major, _, err := version.ParseMajorMinor(versionNumStr)
	if len(tail) < 2 || tail[0] != 'v' || err != nil {
		// There must be a '/v' in the URL path.
		// At this stage only '/Vxxx' and '/vxxx' are valid where xxx is major.minor version.
		// Return 0 as this is the lowest invalid number according to openstack codebase:
		// -1 is reserved and has special handling; 1, 2, 3, etc are valid identity client versions.
		return 0, errors.NotValidf("version part of identity url %s", authURL)
	}
	return major, err
}

// getKeystoneImageSource is an imagemetadata.ImageDataSourceFunc that
// returns a DataSource using the "product-streams" keystone URL.
func getKeystoneImageSource(env environs.Environ) (simplestreams.DataSource, error) {
	e, ok := env.(*Environ)
	if !ok {
		return nil, errors.NotSupportedf("non-openstack model")
	}
	return e.getKeystoneDataSource(&e.keystoneImageDataSourceMutex, &e.keystoneImageDataSource, "product-streams")
}

// getKeystoneToolsSource is a tools.ToolsDataSourceFunc that
// returns a DataSource using the "juju-tools" keystone URL.
func getKeystoneToolsSource(env environs.Environ) (simplestreams.DataSource, error) {
	e, ok := env.(*Environ)
	if !ok {
		return nil, errors.NotSupportedf("non-openstack model")
	}
	return e.getKeystoneDataSource(&e.keystoneToolsDataSourceMutex, &e.keystoneToolsDataSource, "juju-tools")
}

func (e *Environ) getKeystoneDataSource(mu *sync.Mutex, datasource *simplestreams.DataSource, keystoneName string) (simplestreams.DataSource, error) {
	mu.Lock()
	defer mu.Unlock()
	if *datasource != nil {
		return *datasource, nil
	}

	cl := e.client()
	if !cl.IsAuthenticated() {
		if err := authenticateClient(cl); err != nil {
			return nil, err
		}
	}

	serviceURL, err := makeServiceURL(cl, keystoneName, "", nil)
	if err != nil {
		return nil, errors.NewNotSupported(err, fmt.Sprintf("cannot make service URL: %v", err))
	}
	verify := utils.VerifySSLHostnames
	if !e.Config().SSLHostnameVerification() {
		verify = utils.NoVerifySSLHostnames
	}
	cfg := simplestreams.Config{
		Description:          "keystone catalog",
		BaseURL:              serviceURL,
		HostnameVerification: verify,
		Priority:             simplestreams.SPECIFIC_CLOUD_DATA,
		CACertificates:       e.cloudUnlocked.CACertificates,
	}
	if err := cfg.Validate(); err != nil {
		return nil, errors.Annotate(err, "simplestreams config validation failed")
	}
	*datasource = simplestreams.NewDataSource(cfg)
	return *datasource, nil
}

// assignPublicIP tries to assign the given floating IP address to the
// specified server, or returns an error.
func (e *Environ) assignPublicIP(fip *string, serverId string) (err error) {
	if *fip == "" {
		return errors.Errorf("cannot assign a nil public IP to %q", serverId)
	}
	// At startup nw_info is not yet cached so this may fail
	// temporarily while the server is being built
	for a := common.LongAttempt.Start(); a.Next(); {
		err = e.nova().AddServerFloatingIP(serverId, *fip)
		if err == nil {
			return nil
		}
	}
	return err
}

// DistributeInstances implements the state.InstanceDistributor policy.
func (e *Environ) DistributeInstances(
	ctx context.ProviderCallContext, candidates, distributionGroup []instance.Id, limitZones []string,
) ([]instance.Id, error) {
	valid, err := common.DistributeInstances(e, ctx, candidates, distributionGroup, limitZones)
	if err != nil {
		handleCredentialError(err, ctx)
		return valid, err
	}
	return valid, nil
}

// MaintainInstance is specified in the InstanceBroker interface.
func (*Environ) MaintainInstance(ctx context.ProviderCallContext, args environs.StartInstanceParams) error {
	return nil
}

// StartInstance is specified in the InstanceBroker interface.
func (e *Environ) StartInstance(
	ctx context.ProviderCallContext, args environs.StartInstanceParams,
) (*environs.StartInstanceResult, error) {
	res, err := e.startInstance(ctx, args)
	handleCredentialError(err, ctx)
	return res, errors.Trace(err)
}

func (e *Environ) startInstance(
	ctx context.ProviderCallContext, args environs.StartInstanceParams,
) (_ *environs.StartInstanceResult, err error) {
	if err := e.validateAvailabilityZone(ctx, args); err != nil {
		return nil, errors.Trace(err)
	}

	series := args.Tools.OneSeries()
	arches := args.Tools.Arches()
	spec, err := findInstanceSpec(e, instances.InstanceConstraint{
		Region:      e.cloud().Region,
		Series:      series,
		Arches:      arches,
		Constraints: args.Constraints,
	}, args.ImageMetadata)
	if err != nil {
		return nil, common.ZoneIndependentError(err)
	}
	tools, err := args.Tools.Match(tools.Filter{Arch: spec.Image.Arch})
	if err != nil {
		return nil, common.ZoneIndependentError(
			errors.Errorf("chosen architecture %v not present in %v", spec.Image.Arch, arches),
		)
	}

	if err := args.InstanceConfig.SetTools(tools); err != nil {
		return nil, common.ZoneIndependentError(err)
	}

	if err := instancecfg.FinishInstanceConfig(args.InstanceConfig, e.Config()); err != nil {
		return nil, common.ZoneIndependentError(err)
	}

	cloudCfg, err := e.configurator.GetCloudConfig(args)
	if err != nil {
		return nil, common.ZoneIndependentError(err)
	}
	userData, err := providerinit.ComposeUserData(args.InstanceConfig, cloudCfg, OpenstackRenderer{})
	if err != nil {
		return nil, common.ZoneIndependentError(errors.Annotate(err, "cannot make user data"))
	}
	logger.Debugf("openstack user data; %d bytes", len(userData))

	networks, err := e.networksForInstance(args)
	if err != nil {
		return nil, common.ZoneIndependentError(err)
	}

	machineName := resourceName(
		e.namespace,
		e.name,
		args.InstanceConfig.MachineId,
	)

	if e.ecfg().useOpenstackGBP() {
		client := e.neutron()
		ptArg := neutron.PolicyTargetV2{
			Name:                fmt.Sprintf("juju-policytarget-%s", machineName),
			PolicyTargetGroupId: e.ecfg().policyTargetGroup(),
		}
		pt, err := client.CreatePolicyTargetV2(ptArg)
		if err != nil {
			return nil, errors.Trace(err)
		}
		networks = append(networks, nova.ServerNetworks{PortId: pt.PortId})
	}

	// For BUG 1680787: openstack: add support for neutron networks where port
	// security is disabled.
	// If any network specified for instance boot has PortSecurityEnabled equals
	// false, don't create security groups, instance boot will fail.
	createSecurityGroups := true
	if len(networks) > 0 && e.supportsNeutron() {
		client := e.neutron()
		for _, n := range networks {
			if n.NetworkId == "" {
				// It's a GBP network.
				continue
			}
			net, err := client.GetNetworkV2(n.NetworkId)
			if err != nil {
				return nil, common.ZoneIndependentError(err)
			}
			if net.PortSecurityEnabled != nil &&
				*net.PortSecurityEnabled == false {
				createSecurityGroups = *net.PortSecurityEnabled
				logger.Infof("network %q has port_security_enabled set to false. Not using security groups.", net.Id)
				break
			}
		}
	}

	var novaGroupNames = []nova.SecurityGroupName{}
	if createSecurityGroups {
		var apiPort int
		if args.InstanceConfig.Controller != nil {
			apiPort = args.InstanceConfig.Controller.Config.APIPort()
		} else {
			// All ports are the same so pick the first.
			apiPort = args.InstanceConfig.APIInfo.Ports()[0]
		}
		groupNames, err := e.firewaller.SetUpGroups(ctx, args.ControllerUUID, args.InstanceConfig.MachineId, apiPort)
		if err != nil {
			return nil, common.ZoneIndependentError(errors.Annotate(err, "cannot set up groups"))
		}
		novaGroupNames = make([]nova.SecurityGroupName, len(groupNames))
		for i, name := range groupNames {
			novaGroupNames[i].Name = name
		}
	}

	waitForActiveServerDetails := func(
		client *nova.Client,
		id string,
		timeout time.Duration,
	) (server *nova.ServerDetail, err error) {

		var errStillBuilding = errors.Errorf("instance %q has status BUILD", id)
		err = retry.Call(retry.CallArgs{
			Clock:       e.clock,
			Delay:       10 * time.Second,
			MaxDuration: timeout,
			Func: func() error {
				server, err = client.GetServer(id)
				if err != nil {
					return err
				}
				if server.Status == nova.StatusBuild {
					return errStillBuilding
				}
				return nil
			},
			NotifyFunc: func(lastError error, attempt int) {
				_ = args.StatusCallback(status.Provisioning,
					fmt.Sprintf("%s, wait 10 seconds before retry, attempt %d", lastError, attempt), nil)
			},
			IsFatalError: func(err error) bool {
				return err != errStillBuilding
			},
		})
		if err != nil {
			return nil, err
		}
		return server, nil
	}

	tryStartNovaInstance := func(
		attempts utils.AttemptStrategy,
		client *nova.Client,
		instanceOpts nova.RunServerOpts,
	) (server *nova.Entity, err error) {
		for a := attempts.Start(); a.Next(); {
			server, err = client.RunServer(instanceOpts)
			if err != nil {
				break
			}
			if server == nil {
				logger.Warningf("may have lost contact with nova api while creating instances, some stray instances may be around and need to be deleted")
				break
			}
			var serverDetail *nova.ServerDetail
			serverDetail, err = waitForActiveServerDetails(client, server.Id, 5*time.Minute)
			if err != nil || serverDetail == nil {
				server = nil
				break
			} else if serverDetail.Status == nova.StatusActive {
				break
			} else if serverDetail.Status == nova.StatusError {
				// Perhaps there is an error case where a retry in the same AZ
				// is a good idea.
				faultMsg := " unable to determine fault details"
				if serverDetail.Fault != nil {
					faultMsg = fmt.Sprintf(" with fault %q", serverDetail.Fault.Message)
				} else {
					logger.Debugf("getting active server details from nova failed without fault details")
				}
				logger.Infof("Deleting instance %q in ERROR state%v", server.Id, faultMsg)
				if err = e.terminateInstances(ctx, []instance.Id{instance.Id(server.Id)}); err != nil {
					logger.Debugf("Failed to delete instance in ERROR state, %q", err)
				}
				server = nil
				err = errors.New(faultMsg)
				break
			}
		}
		return server, err
	}

	var opts = nova.RunServerOpts{
		Name:               machineName,
		FlavorId:           spec.InstanceType.Id,
		UserData:           userData,
		SecurityGroupNames: novaGroupNames,
		Networks:           networks,
		Metadata:           args.InstanceConfig.Tags,
		AvailabilityZone:   args.AvailabilityZone,
	}
	err = e.configureRootDisk(ctx, args, spec, &opts)
	if err != nil {
		return nil, common.ZoneIndependentError(err)
	}
	e.configurator.ModifyRunServerOptions(&opts)

	server, err := tryStartNovaInstance(shortAttempt, e.nova(), opts)
	if err != nil || server == nil {
		// 'No valid host available' is typically a resource error,
		// let the provisioner know it is a good idea to try another
		// AZ if available.
		err := errors.Annotate(err, "cannot run instance")
		zoneSpecific := isNoValidHostsError(err)
		if !zoneSpecific {
			err = common.ZoneIndependentError(err)
		}
		return nil, err
	}

	detail, err := e.nova().GetServer(server.Id)
	if err != nil {
		return nil, common.ZoneIndependentError(errors.Annotate(err, "cannot get started instance"))
	}

	inst := &openstackInstance{
		e:            e,
		serverDetail: detail,
		arch:         &spec.Image.Arch,
		instType:     &spec.InstanceType,
		runOpts:      &opts,
	}
	logger.Infof("started instance %q", inst.Id())
	withPublicIP := e.ecfg().useFloatingIP()
	if withPublicIP {
		// If we don't lock here, AllocatePublicIP() can return the same
		// public IP for 2 different instances.  Only one will successfully
		// be assigned the public IP, the other will not have one.
		e.publicIPMutex.Lock()
		defer e.publicIPMutex.Unlock()
		var publicIP *string
		logger.Debugf("allocating public IP address for openstack node")
		if fip, err := e.networking.AllocatePublicIP(inst.Id()); err != nil {
			return nil, common.ZoneIndependentError(errors.Annotate(err, "cannot allocate a public IP as needed"))
		} else {
			publicIP = fip
			logger.Infof("allocated public IP %s", *publicIP)
		}
		if err := e.assignPublicIP(publicIP, string(inst.Id())); err != nil {
			if err := e.terminateInstances(ctx, []instance.Id{inst.Id()}); err != nil {
				// ignore the failure at this stage, just log it
				logger.Debugf("failed to terminate instance %q: %v", inst.Id(), err)
			}
			return nil, common.ZoneIndependentError(errors.Annotatef(err,
				"cannot assign public address %s to instance %q",
				*publicIP, inst.Id(),
			))
		}
		inst.floatingIP = publicIP
	}

	return &environs.StartInstanceResult{
		Instance: inst,
		Hardware: inst.hardwareCharacteristics(),
	}, nil
}

// validateAvailabilityZone validates AZs supplied in StartInstanceParams.
// args.AvailabilityZone should only be set if this OpenStack supports zones.
// We need to validate it if supplied.
func (e *Environ) validateAvailabilityZone(ctx context.ProviderCallContext, args environs.StartInstanceParams) error {
	if args.AvailabilityZone == "" {
		return nil
	}

	volumeAttachmentsZone, err := e.volumeAttachmentsZone(args.VolumeAttachments)
	if err != nil {
		return common.ZoneIndependentError(err)
	}
	if err := validateAvailabilityZoneConsistency(args.AvailabilityZone, volumeAttachmentsZone); err != nil {
		return common.ZoneIndependentError(err)
	}

	return errors.Trace(common.ValidateAvailabilityZone(e, ctx, args.AvailabilityZone))
}

// networksForInstance returns networks that will be attached
// to a new Openstack instance.
func (e *Environ) networksForInstance(args environs.StartInstanceParams) ([]nova.ServerNetworks, error) {
	networks, err := e.networking.DefaultNetworks()
	if err != nil {
		return nil, errors.Annotate(err, "getting initial networks")
	}

	usingNetwork := e.ecfg().network()
	networkID, err := e.networking.ResolveNetwork(usingNetwork, false)
	if err != nil {
		if usingNetwork == "" {
			// If there is no network configured, we only throw out when the
			// error reports multiple Openstack networks.
			// If there are no Openstack networks at all (such as Canonistack),
			// having no network config is not an error condition.
			if strings.HasPrefix(err.Error(), "multiple networks") {
				return nil, errors.New(noNetConfigMsg(err))
			}
		} else {
			return nil, errors.Trace(err)
		}
	} else {
		logger.Debugf("using network id %q", networkID)
		networks = append(networks, nova.ServerNetworks{
			NetworkId: networkID,
		})
	}

	// Attempt to locate any subnet IDs for a passed in AZ.
	availabilityZone := args.AvailabilityZone

	var subnetIDsForZone []string
	if args.Constraints.HasSpaces() {
		var err error
		subnetIDsForZone, err = corenetwork.FindSubnetIDsForAvailabilityZone(availabilityZone, args.SubnetsToZones)

		switch {
		case errors.IsNotFound(err):
			return nil, errors.Trace(err)
		case err != nil:
			return nil, errors.Annotatef(err, "getting subnets for zone %q", availabilityZone)
		}
	}

	// FixedIp takes a CIDR, but we've only got a network ID, so we need to
	// hoover all the subnets and select the ones we care about.
	networkIDs := make([]corenetwork.Id, len(subnetIDsForZone))
	for k, v := range subnetIDsForZone {
		networkIDs[k] = corenetwork.Id(v)
	}
	subnetInfo, err := e.networking.Subnets(instance.UnknownId, networkIDs)
	if err != nil {
		return nil, errors.Annotate(err, "getting subnet info")
	}

	// If there are some subnet IDs from an AZ, attempt to put them on the
	// networks.
	// TODO (stickupkid): Currently this logic attempts to handle multiple
	// networks, in fact in reality it actually handles only one server network.
	// DefaultNetworks currently returns an empty slice and ResolveNetwork only
	// returns one network. bug #1733266
	var subnetCIDR string
	if num := len(subnetInfo); num > 1 {
		// Randomize the subnetInfo in order to gain a better distributed
		// spread over the zones.
		// Note: this won't perform an even distribution unless a new random
		// seed has been given for every initialization.
		subnetCIDR = subnetInfo[rand.Intn(len(subnetInfo))].CIDR
		logger.Debugf("selected random subnet cidr %q from all matching in zone %q", subnetCIDR, availabilityZone)
	} else if num == 1 {
		subnetCIDR = subnetInfo[0].CIDR
		logger.Debugf("selected subnet cidr %q in zone %q", subnetCIDR, availabilityZone)
	}

	// Set the subnetID on the network for all networks.
	subnetNetworks := make([]nova.ServerNetworks, len(networks))
	for k, network := range networks {
		// If no networkId is found we need to skip them as we can't do anything
		// with them.
		if network.NetworkId == "" {
			continue
		}
		network.FixedIp = subnetCIDR
		subnetNetworks[k] = network
	}

	return subnetNetworks, nil
}

func (e *Environ) configureRootDisk(ctx context.ProviderCallContext, args environs.StartInstanceParams,
	spec *instances.InstanceSpec, runOpts *nova.RunServerOpts) error {
	rootDiskSource := rootDiskSourceLocal
	if args.Constraints.HasRootDiskSource() {
		rootDiskSource = *args.Constraints.RootDiskSource
	}
	rootDiskMapping := nova.BlockDeviceMapping{
		BootIndex:  0,
		UUID:       spec.Image.Id,
		SourceType: "image",
		// NB constraints.RootDiskSource in the case of OpenStack represents
		// the type of block device to use. Either "local" to represent a local
		// block device or "volume" to represent a block device from the cinder
		// block storage service.
		DestinationType:     rootDiskSource,
		DeleteOnTermination: true,
	}
	switch rootDiskSource {
	case rootDiskSourceLocal:
		runOpts.ImageId = spec.Image.Id
	case rootDiskSourceVolume:
		size := uint64(0)
		if args.Constraints.HasRootDisk() {
			size = *args.Constraints.RootDisk
		}
		if size <= 0 {
			size = defaultRootDiskSize
		}
		sizeGB := common.MiBToGiB(size)
		rootDiskMapping.VolumeSize = int(sizeGB)
	default:
		return errors.Errorf("invalid %s %s", constraints.RootDiskSource, rootDiskSource)
	}
	runOpts.BlockDeviceMappings = []nova.BlockDeviceMapping{rootDiskMapping}
	return nil
}

func (e *Environ) deriveAvailabilityZone(
	ctx context.ProviderCallContext,
	placement string,
	volumeAttachments []storage.VolumeAttachmentParams,
) (string, error) {
	volumeAttachmentsZone, err := e.volumeAttachmentsZone(volumeAttachments)
	if err != nil {
		handleCredentialError(err, ctx)
		return "", errors.Trace(err)
	}
	if placement == "" {
		return volumeAttachmentsZone, nil
	}
	instPlacement, err := e.parsePlacement(ctx, placement)
	if err != nil {
		return "", err
	}
	if err := validateAvailabilityZoneConsistency(instPlacement.zoneName, volumeAttachmentsZone); err != nil {
		return "", errors.Annotatef(err, "cannot create instance with placement %q", placement)
	}
	return instPlacement.zoneName, nil
}

func validateAvailabilityZoneConsistency(instanceZone, volumeAttachmentsZone string) error {
	if volumeAttachmentsZone != "" && instanceZone != volumeAttachmentsZone {
		return errors.Errorf(
			"cannot create instance in zone %q, as this will prevent attaching the requested disks in zone %q",
			instanceZone, volumeAttachmentsZone,
		)
	}
	return nil
}

// volumeAttachmentsZone determines the availability zone for each volume
// identified in the volume attachment parameters, checking that they are
// all the same, and returns the availability zone name.
func (e *Environ) volumeAttachmentsZone(volumeAttachments []storage.VolumeAttachmentParams) (string, error) {
	if len(volumeAttachments) == 0 {
		return "", nil
	}
	cinderProvider, err := e.cinderProvider()
	if err != nil {
		return "", errors.Trace(err)
	}
	volumes, err := modelCinderVolumes(cinderProvider.storageAdapter, cinderProvider.modelUUID)
	if err != nil {
		return "", errors.Trace(err)
	}
	var zone string
	for i, a := range volumeAttachments {
		var v *cinder.Volume
		for i := range volumes {
			if volumes[i].ID == a.VolumeId {
				v = &volumes[i]
				break
			}
		}
		if v == nil {
			return "", errors.Errorf("cannot find volume %q to attach to new instance", a.VolumeId)
		}
		if zone == "" {
			zone = v.AvailabilityZone
		} else if v.AvailabilityZone != zone {
			return "", errors.Errorf(
				"cannot attach volumes from multiple availability zones: %s is in %s, %s is in %s",
				volumeAttachments[i-1].VolumeId, zone, a.VolumeId, v.AvailabilityZone,
			)
		}
	}
	return zone, nil
}

func isNoValidHostsError(err error) bool {
	if cause := errors.Cause(err); cause != nil {
		return strings.Contains(cause.Error(), "No valid host was found")
	}
	return false
}

func (e *Environ) StopInstances(ctx context.ProviderCallContext, ids ...instance.Id) error {
	// If in instance firewall mode, gather the security group names.
	securityGroupNames, err := e.firewaller.GetSecurityGroups(ctx, ids...)
	if err == environs.ErrNoInstances {
		return nil
	}
	if err != nil {
		handleCredentialError(err, ctx)
		return err
	}
	logger.Debugf("terminating instances %v", ids)
	if err := e.terminateInstances(ctx, ids); err != nil {
		handleCredentialError(err, ctx)
		return err
	}
	if securityGroupNames != nil {
		if err := e.firewaller.DeleteGroups(ctx, securityGroupNames...); err != nil {
			handleCredentialError(err, ctx)
			return err
		}
	}
	return nil
}

func (e *Environ) isAliveServer(server nova.ServerDetail) bool {
	switch server.Status {
	case nova.StatusActive, nova.StatusBuild, nova.StatusBuildSpawning, nova.StatusShutoff, nova.StatusSuspended:
		return true
	}
	return false
}

func (e *Environ) listServers(ctx context.ProviderCallContext, ids []instance.Id) ([]nova.ServerDetail, error) {
	wantedServers := make([]nova.ServerDetail, 0, len(ids))
	if len(ids) == 1 {
		// Common case, single instance, may return NotFound
		var maybeServer *nova.ServerDetail
		maybeServer, err := e.nova().GetServer(string(ids[0]))
		if err != nil {
			handleCredentialError(err, ctx)
			return nil, err
		}
		// Only return server details if it is currently alive
		if maybeServer != nil && e.isAliveServer(*maybeServer) {
			wantedServers = append(wantedServers, *maybeServer)
		}
		return wantedServers, nil
	}
	// List all instances in the environment.
	instances, err := e.AllRunningInstances(ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, err
	}
	// Return only servers with the wanted ids that are currently alive
	for _, inst := range instances {
		inst := inst.(*openstackInstance)
		serverDetail := *inst.serverDetail
		if !e.isAliveServer(serverDetail) {
			continue
		}
		for _, id := range ids {
			if inst.Id() != id {
				continue
			}
			wantedServers = append(wantedServers, serverDetail)
			break
		}
	}
	return wantedServers, nil
}

// updateFloatingIPAddresses updates the instances with any floating IP address
// that have been assigned to those instances.
func (e *Environ) updateFloatingIPAddresses(ctx context.ProviderCallContext, instances map[string]instances.Instance) error {
	servers, err := e.nova().ListServersDetail(jujuMachineFilter())
	if err != nil {
		handleCredentialError(err, ctx)
		return err
	}
	for _, server := range servers {
		// server.Addresses is a map with entries containing []nova.IPAddress
		for _, net := range server.Addresses {
			for _, addr := range net {
				if addr.Type == "floating" {
					instId := server.Id
					if inst, ok := instances[instId]; ok {
						instFip := &addr.Address
						inst.(*openstackInstance).floatingIP = instFip
					}
				}
			}
		}
	}
	return nil
}

func (e *Environ) Instances(ctx context.ProviderCallContext, ids []instance.Id) ([]instances.Instance, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Make a series of requests to cope with eventual consistency.
	// Each request will attempt to add more instances to the requested
	// set.
	var foundServers []nova.ServerDetail
	for a := shortAttempt.Start(); a.Next(); {
		var err error
		foundServers, err = e.listServers(ctx, ids)
		if err != nil {
			logger.Debugf("error listing servers: %v", err)
			if !IsNotFoundError(err) {
				handleCredentialError(err, ctx)
				return nil, err
			}
		}
		if len(foundServers) == len(ids) {
			break
		}
	}
	logger.Tracef("%d/%d live servers found", len(foundServers), len(ids))
	if len(foundServers) == 0 {
		return nil, environs.ErrNoInstances
	}

	instsById := make(map[string]instances.Instance, len(foundServers))
	for i, server := range foundServers {
		// TODO(wallyworld): lookup the flavor details to fill in the
		// instance type data
		instsById[server.Id] = &openstackInstance{
			e:            e,
			serverDetail: &foundServers[i],
		}
	}

	// Update the instance structs with any floating IP address that has been assigned to the instance.
	if e.ecfg().useFloatingIP() {
		if err := e.updateFloatingIPAddresses(ctx, instsById); err != nil {
			return nil, err
		}
	}

	insts := make([]instances.Instance, len(ids))
	var err error
	for i, id := range ids {
		if inst := instsById[string(id)]; inst != nil {
			insts[i] = inst
		} else {
			err = environs.ErrPartialInstances
		}
	}
	return insts, err
}

// AdoptResources is part of the Environ interface.
func (e *Environ) AdoptResources(ctx context.ProviderCallContext, controllerUUID string, fromVersion version.Number) error {
	var failed []string
	controllerTag := map[string]string{tags.JujuController: controllerUUID}

	instances, err := e.AllInstances(ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	for _, instance := range instances {
		err := e.TagInstance(ctx, instance.Id(), controllerTag)
		if err != nil {
			logger.Errorf("error updating controller tag for instance %s: %v", instance.Id(), err)
			failed = append(failed, string(instance.Id()))
			if denied := common.MaybeHandleCredentialError(IsAuthorisationFailure, err, ctx); denied {
				// If we have an invvalid credential, there is no need to proceed: we'll fail 100%.
				break
			}
		}
	}

	failedVolumes, err := e.adoptVolumes(controllerTag, ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	failed = append(failed, failedVolumes...)

	err = e.firewaller.UpdateGroupController(ctx, controllerUUID)
	if err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	if len(failed) != 0 {
		return errors.Errorf("error updating controller tag for some resources: %v", failed)
	}
	return nil
}

func (e *Environ) adoptVolumes(controllerTag map[string]string, ctx context.ProviderCallContext) ([]string, error) {
	cinder, err := e.cinderProvider()
	if errors.IsNotSupported(err) {
		logger.Debugf("volumes not supported: not transferring ownership for volumes")
		return nil, nil
	}
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, errors.Trace(err)
	}
	// TODO(axw): fix the storage API.
	storageConfig, err := storage.NewConfig("cinder", CinderProviderType, nil)
	if err != nil {
		return nil, errors.Trace(err)
	}
	volumeSource, err := cinder.VolumeSource(storageConfig)
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, errors.Trace(err)
	}
	volumeIds, err := volumeSource.ListVolumes(ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, errors.Trace(err)
	}

	var failed []string
	for _, volumeId := range volumeIds {
		_, err := cinder.storageAdapter.SetVolumeMetadata(volumeId, controllerTag)
		if err != nil {
			logger.Errorf("error updating controller tag for volume %s: %v", volumeId, err)
			failed = append(failed, volumeId)
			if denied := common.MaybeHandleCredentialError(IsAuthorisationFailure, err, ctx); denied {
				// If we have an invvalid credential, there is no need to proceed: we'll fail 100%.
				break
			}
		}
	}
	return failed, nil
}

// AllInstances returns all instances in this environment.
func (e *Environ) AllInstances(ctx context.ProviderCallContext) ([]instances.Instance, error) {
	tagFilter := tagValue{tags.JujuModel, e.ecfg().UUID()}
	instances, err := e.allInstances(ctx, tagFilter, e.ecfg().useFloatingIP())
	if err != nil {
		handleCredentialError(err, ctx)
		return instances, err
	}
	return instances, nil
}

// AllRunningInstances returns all running, available instances in this environment.
func (e *Environ) AllRunningInstances(ctx context.ProviderCallContext) ([]instances.Instance, error) {
	// e.allInstances(...) already handles all instances irrespective of the state, so
	// here 'all' is also 'all running'.
	return e.AllInstances(ctx)
}

// allControllerManagedInstances returns all instances managed by this
// environment's controller, matching the optionally specified filter.
func (e *Environ) allControllerManagedInstances(ctx context.ProviderCallContext, controllerUUID string, updateFloatingIPAddresses bool) ([]instances.Instance, error) {
	tagFilter := tagValue{tags.JujuController, controllerUUID}
	instances, err := e.allInstances(ctx, tagFilter, updateFloatingIPAddresses)
	if err != nil {
		handleCredentialError(err, ctx)
		return instances, err
	}
	return instances, nil
}

type tagValue struct {
	tag, value string
}

// allControllerManagedInstances returns all instances managed by this
// environment's controller, matching the optionally specified filter.
func (e *Environ) allInstances(ctx context.ProviderCallContext, tagFilter tagValue, updateFloatingIPAddresses bool) ([]instances.Instance, error) {
	servers, err := e.nova().ListServersDetail(jujuMachineFilter())
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, err
	}
	instsById := make(map[string]instances.Instance)
	for _, server := range servers {
		if server.Metadata[tagFilter.tag] != tagFilter.value {
			continue
		}
		if e.isAliveServer(server) {
			var s = server
			// TODO(wallyworld): lookup the flavor details to fill in the instance type data
			instsById[s.Id] = &openstackInstance{e: e, serverDetail: &s}
		}
	}
	if updateFloatingIPAddresses {
		if err := e.updateFloatingIPAddresses(ctx, instsById); err != nil {
			handleCredentialError(err, ctx)
			return nil, err
		}
	}
	insts := make([]instances.Instance, 0, len(instsById))
	for _, inst := range instsById {
		insts = append(insts, inst)
	}
	return insts, nil
}

func (e *Environ) Destroy(ctx context.ProviderCallContext) error {
	err := common.Destroy(e, ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	// Delete all security groups remaining in the model.
	if err := e.firewaller.DeleteAllModelGroups(ctx); err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	return nil
}

// DestroyController implements the Environ interface.
func (e *Environ) DestroyController(ctx context.ProviderCallContext, controllerUUID string) error {
	if err := e.Destroy(ctx); err != nil {
		handleCredentialError(err, ctx)
		return errors.Annotate(err, "destroying controller model")
	}
	// In case any hosted environment hasn't been cleaned up yet,
	// we also attempt to delete their resources when the controller
	// environment is destroyed.
	if err := e.destroyControllerManagedEnvirons(ctx, controllerUUID); err != nil {
		handleCredentialError(err, ctx)
		return errors.Annotate(err, "destroying managed models")
	}
	if err := e.firewaller.DeleteAllControllerGroups(ctx, controllerUUID); err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	return nil
}

// destroyControllerManagedEnvirons destroys all environments managed by this
// models's controller.
func (e *Environ) destroyControllerManagedEnvirons(ctx context.ProviderCallContext, controllerUUID string) error {
	// Terminate all instances managed by the controller.
	insts, err := e.allControllerManagedInstances(ctx, controllerUUID, false)
	if err != nil {
		return errors.Annotate(err, "listing instances")
	}
	instIds := make([]instance.Id, len(insts))
	for i, inst := range insts {
		instIds[i] = inst.Id()
	}
	if err := e.terminateInstances(ctx, instIds); err != nil {
		handleCredentialError(err, ctx)
		return errors.Annotate(err, "terminating instances")
	}

	// Delete all volumes managed by the controller.
	cinder, err := e.cinderProvider()
	if err == nil {
		volumes, err := controllerCinderVolumes(cinder.storageAdapter, controllerUUID)
		if err != nil {
			handleCredentialError(err, ctx)
			return errors.Annotate(err, "listing volumes")
		}
		volIds := volumeInfoToVolumeIds(cinderToJujuVolumeInfos(volumes))
		errs := foreachVolume(ctx, cinder.storageAdapter, volIds, destroyVolume)
		for i, err := range errs {
			if err == nil {
				continue
			}
			handleCredentialError(err, ctx)
			return errors.Annotatef(err, "destroying volume %q", volIds[i])
		}
	} else if !errors.IsNotSupported(err) {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}

	// Security groups for hosted models are destroyed by the
	// DeleteAllControllerGroups method call from Destroy().
	return nil
}

func resourceName(namespace instance.Namespace, envName, resourceId string) string {
	return namespace.Value(envName + "-" + resourceId)
}

// jujuMachineFilter returns a nova.Filter matching machines created by Juju.
// The machines are not filtered to any particular environment. To do that,
// instance tags must be compared.
func jujuMachineFilter() *nova.Filter {
	filter := nova.NewFilter()
	filter.Set(nova.FilterServer, "juju-.*")
	return filter
}

// rulesToRuleInfo maps ingress rules to nova rules
func rulesToRuleInfo(groupId string, rules []network.IngressRule) []neutron.RuleInfoV2 {
	var result []neutron.RuleInfoV2
	for _, r := range rules {
		ruleInfo := neutron.RuleInfoV2{
			Direction:     "ingress",
			ParentGroupId: groupId,
			PortRangeMin:  r.FromPort,
			PortRangeMax:  r.ToPort,
			IPProtocol:    r.Protocol,
		}
		sourceCIDRs := r.SourceCIDRs
		if len(sourceCIDRs) == 0 {
			sourceCIDRs = []string{"0.0.0.0/0"}
		}
		for _, sr := range sourceCIDRs {
			ruleInfo.RemoteIPPrefix = sr
			result = append(result, ruleInfo)
		}
	}
	return result
}

func (e *Environ) OpenPorts(ctx context.ProviderCallContext, rules []network.IngressRule) error {
	if err := e.firewaller.OpenPorts(ctx, rules); err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	return nil
}

func (e *Environ) ClosePorts(ctx context.ProviderCallContext, rules []network.IngressRule) error {
	if err := e.firewaller.ClosePorts(ctx, rules); err != nil {
		handleCredentialError(err, ctx)
		return errors.Trace(err)
	}
	return nil
}

func (e *Environ) IngressRules(ctx context.ProviderCallContext) ([]network.IngressRule, error) {
	rules, err := e.firewaller.IngressRules(ctx)
	if err != nil {
		handleCredentialError(err, ctx)
		return rules, errors.Trace(err)
	}
	return rules, nil
}

func (e *Environ) Provider() environs.EnvironProvider {
	return providerInstance
}

func (e *Environ) terminateInstances(ctx context.ProviderCallContext, ids []instance.Id) error {
	if len(ids) == 0 {
		return nil
	}
	var firstErr error
	novaClient := e.nova()
	for _, id := range ids {
		err := novaClient.DeleteServer(string(id))
		if IsNotFoundError(err) {
			err = nil
		}
		if err != nil && firstErr == nil {
			logger.Debugf("error terminating instance %q: %v", id, err)
			firstErr = err
			if denied := common.MaybeHandleCredentialError(IsAuthorisationFailure, err, ctx); denied {
				// We'll 100% fail all subsequent calls if we have an invalid credential.
				break
			}
		}
	}
	return firstErr
}

// MetadataLookupParams returns parameters which are used to query simplestreams metadata.
func (e *Environ) MetadataLookupParams(region string) (*simplestreams.MetadataLookupParams, error) {
	if region == "" {
		region = e.cloud().Region
	}
	cloudSpec, err := e.cloudSpec(region)
	if err != nil {
		return nil, err
	}
	return &simplestreams.MetadataLookupParams{
		Series:   config.PreferredSeries(e.ecfg()),
		Region:   cloudSpec.Region,
		Endpoint: cloudSpec.Endpoint,
	}, nil
}

// Region is specified in the HasRegion interface.
func (e *Environ) Region() (simplestreams.CloudSpec, error) {
	return e.cloudSpec(e.cloud().Region)
}

func (e *Environ) cloudSpec(region string) (simplestreams.CloudSpec, error) {
	return simplestreams.CloudSpec{
		Region:   region,
		Endpoint: e.cloud().Endpoint,
	}, nil
}

// TagInstance implements environs.InstanceTagger.
func (e *Environ) TagInstance(ctx context.ProviderCallContext, id instance.Id, tags map[string]string) error {
	if err := e.nova().SetServerMetadata(string(id), tags); err != nil {
		handleCredentialError(err, ctx)
		return errors.Annotate(err, "setting server metadata")
	}
	return nil
}

func (e *Environ) SetClock(clock clock.Clock) {
	e.clock = clock
}

func validateCloudSpec(spec environs.CloudSpec) error {
	if err := spec.Validate(); err != nil {
		return errors.Trace(err)
	}
	if err := validateAuthURL(spec.Endpoint); err != nil {
		return errors.Annotate(err, "validating auth-url")
	}
	if spec.Credential == nil {
		return errors.NotValidf("missing credential")
	}
	switch authType := spec.Credential.AuthType(); authType {
	case cloud.UserPassAuthType:
	case cloud.AccessKeyAuthType:
	default:
		return errors.NotSupportedf("%q auth-type", authType)
	}
	return nil
}

func validateAuthURL(authURL string) error {
	parts, err := url.Parse(authURL)
	if err != nil || parts.Host == "" || parts.Scheme == "" {
		return errors.NotValidf("auth-url %q", authURL)
	}
	return nil
}

// Subnets is specified on environs.Networking.
func (e *Environ) Subnets(
	ctx context.ProviderCallContext, instId instance.Id, subnetIds []corenetwork.Id,
) ([]corenetwork.SubnetInfo, error) {
	subnets, err := e.networking.Subnets(instId, subnetIds)
	if err != nil {
		handleCredentialError(err, ctx)
		return subnets, errors.Trace(err)
	}
	return subnets, nil
}

// NetworkInterfaces is specified on environs.Networking.
func (e *Environ) NetworkInterfaces(ctx context.ProviderCallContext, ids []instance.Id) ([][]corenetwork.InterfaceInfo, error) {
	infos, err := e.networking.NetworkInterfaces(ids)
	if err != nil {
		handleCredentialError(err, ctx)
		return infos, errors.Trace(err)
	}

	return infos, nil
}

// SupportsSpaces is specified on environs.Networking.
func (e *Environ) SupportsSpaces(ctx context.ProviderCallContext) (bool, error) {
	return false, nil
}

// SupportsSpaces is specified on environs.Networking.
func (e *Environ) SupportsProviderSpaces(ctx context.ProviderCallContext) (bool, error) {
	return false, nil
}

// SupportsSpaceDiscovery is specified on environs.Networking.
func (e *Environ) SupportsSpaceDiscovery(ctx context.ProviderCallContext) (bool, error) {
	return false, nil
}

// Spaces is specified on environs.Networking.
func (e *Environ) Spaces(ctx context.ProviderCallContext) ([]corenetwork.SpaceInfo, error) {
	return nil, errors.NotSupportedf("spaces")
}

// SupportsContainerAddresses is specified on environs.Networking.
func (e *Environ) SupportsContainerAddresses(ctx context.ProviderCallContext) (bool, error) {
	return false, errors.NotSupportedf("container address")
}

// SuperSubnets is specified on environs.Networking
func (e *Environ) SuperSubnets(ctx context.ProviderCallContext) ([]string, error) {
	subnets, err := e.networking.Subnets("", nil)
	if err != nil {
		handleCredentialError(err, ctx)
		return nil, err
	}
	cidrs := make([]string, len(subnets))
	for i, subnet := range subnets {
		cidrs[i] = subnet.CIDR
	}
	return cidrs, nil
}

// AllocateContainerAddresses is specified on environs.Networking.
func (e *Environ) AllocateContainerAddresses(ctx context.ProviderCallContext, hostInstanceID instance.Id, containerTag names.MachineTag, preparedInfo []corenetwork.InterfaceInfo) ([]corenetwork.InterfaceInfo, error) {
	return nil, errors.NotSupportedf("allocate container address")
}

// ReleaseContainerAddresses is specified on environs.Networking.
func (e *Environ) ReleaseContainerAddresses(ctx context.ProviderCallContext, interfaces []network.ProviderInterfaceInfo) error {
	return errors.NotSupportedf("release container address")
}

// ProviderSpaceInfo is specified on environs.NetworkingEnviron.
func (*Environ) ProviderSpaceInfo(
	ctx context.ProviderCallContext, space *corenetwork.SpaceInfo,
) (*environs.ProviderSpaceInfo, error) {
	return nil, errors.NotSupportedf("provider space info")
}

// AreSpacesRoutable is specified on environs.NetworkingEnviron.
func (*Environ) AreSpacesRoutable(ctx context.ProviderCallContext, space1, space2 *environs.ProviderSpaceInfo) (bool, error) {
	return false, nil
}

// SSHAddresses is specified on environs.SSHAddresses.
func (*Environ) SSHAddresses(ctx context.ProviderCallContext, addresses corenetwork.SpaceAddresses) (corenetwork.SpaceAddresses, error) {
	return addresses, nil
}
