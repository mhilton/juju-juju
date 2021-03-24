// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package bootstrap

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/juju/collections/set"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/names/v4"
	"github.com/juju/utils/v2"
	"github.com/juju/utils/v2/arch"
	"github.com/juju/utils/v2/ssh"
	"github.com/juju/version"

	"github.com/juju/juju/api"
	"github.com/juju/juju/cloud"
	jujucloud "github.com/juju/juju/cloud"
	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/cloudconfig/podcfg"
	"github.com/juju/juju/controller"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/series"
	coreseries "github.com/juju/juju/core/series"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/context"
	"github.com/juju/juju/environs/gui"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/environs/storage"
	"github.com/juju/juju/environs/sync"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/mongo"
	"github.com/juju/juju/pki"
	coretools "github.com/juju/juju/tools"
	jujuversion "github.com/juju/juju/version"
)

const noToolsMessage = `Juju cannot bootstrap because no agent binaries are available for your model.
You may want to use the 'agent-metadata-url' configuration setting to specify the binaries' location.
`

var (
	logger = loggo.GetLogger("juju.environs.bootstrap")

	errCancelled = errors.New("cancelled")
)

// BootstrapParams holds the parameters for bootstrapping an environment.
type BootstrapParams struct {
	// ModelConstraints are merged with the bootstrap constraints
	// to choose the initial instance, and will be stored in the
	// initial models' states.
	ModelConstraints constraints.Value

	// BootstrapConstraints are used to choose the initial instance.
	// BootstrapConstraints does not affect the model constraints.
	BootstrapConstraints constraints.Value

	// ControllerName is the controller name.
	ControllerName string

	// BootstrapSeries, if specified, is the series to use for the
	// initial bootstrap machine.
	BootstrapSeries string

	// SupportedBootstrapSeries is a supported set of series to use for
	// validating against the bootstrap series.
	SupportedBootstrapSeries set.Strings

	// BootstrapImage, if specified, is the image ID to use for the
	// initial bootstrap machine.
	BootstrapImage string

	// Cloud contains the properties of the cloud that Juju will be
	// bootstrapped in.
	Cloud cloud.Cloud

	// CloudRegion is the name of the cloud region that Juju will be bootstrapped in.
	CloudRegion string

	// CloudCredentialName is the name of the cloud credential that Juju will be
	// bootstrapped with. This may be empty, for clouds that do not require
	// credentials.
	CloudCredentialName string

	// CloudCredential contains the cloud credential that Juju will be
	// bootstrapped with. This may be nil, for clouds that do not require
	// credentialis.
	CloudCredential *cloud.Credential

	// ControllerConfig is the set of config attributes relevant
	// to a controller.
	ControllerConfig controller.Config

	// ControllerInheritedConfig is the set of config attributes to be shared
	// across all models in the same controller.
	ControllerInheritedConfig map[string]interface{}

	// RegionInheritedConfig holds region specific configuration attributes to
	// be shared across all models in the same controller on a particular
	// cloud.
	RegionInheritedConfig cloud.RegionConfig

	// HostedModelConfig is the set of config attributes to be overlaid
	// on the controller config to construct the initial hosted model
	// config.
	HostedModelConfig map[string]interface{}

	// Placement, if non-empty, holds an environment-specific placement
	// directive used to choose the initial instance.
	Placement string

	// BuildAgent reports whether we should build and upload the local agent
	// binary and override the environment's specified agent-version.
	// It is an error to specify BuildAgent with a nil BuildAgentTarball.
	BuildAgent bool

	// BuildAgentTarball, if non-nil, is a function that may be used to
	// build tools to upload. If this is nil, tools uploading will never
	// take place.
	BuildAgentTarball sync.BuildAgentTarballFunc

	// MetadataDir is an optional path to a local directory containing
	// tools and/or image metadata.
	MetadataDir string

	// AgentVersion, if set, determines the exact tools version that
	// will be used to start the Juju agents.
	AgentVersion *version.Number

	// GUIDataSourceBaseURL holds the simplestreams data source base URL
	// used to retrieve the Juju GUI archive installed in the controller.
	// If not set, the Juju GUI is not installed from simplestreams.
	GUIDataSourceBaseURL string

	// AdminSecret contains the administrator password.
	AdminSecret string

	// CAPrivateKey is the controller's CA certificate private key.
	CAPrivateKey string

	// ControllerServiceType is the service type of a k8s controller.
	ControllerServiceType string

	// ControllerExternalName is the external name of a k8s controller.
	ControllerExternalName string

	// ControllerExternalIPs is the list of external ips for a k8s controller.
	ControllerExternalIPs []string

	// DialOpts contains the bootstrap dial options.
	DialOpts environs.BootstrapDialOpts

	// JujuDbSnapPath is the path to a local .snap file that will be used
	// to run the juju-db service.
	JujuDbSnapPath string

	// JujuDbSnapAssertionsPath is the path to a local .assertfile that
	// will be used to test the contents of the .snap at JujuDbSnap.
	JujuDbSnapAssertionsPath string

	// Force is used to allow a bootstrap to be run on unsupported series.
	Force bool

	// ExtraAgentValuesForTesting are testing only values written to the agent config file.
	ExtraAgentValuesForTesting map[string]string
}

// Validate validates the bootstrap parameters.
func (p BootstrapParams) Validate() error {
	if p.AdminSecret == "" {
		return errors.New("admin-secret is empty")
	}
	if p.ControllerConfig.ControllerUUID() == "" {
		return errors.New("controller configuration has no controller UUID")
	}
	if _, hasCACert := p.ControllerConfig.CACert(); !hasCACert {
		return errors.New("controller configuration has no ca-cert")
	}
	if p.CAPrivateKey == "" {
		return errors.New("empty ca-private-key")
	}
	if p.SupportedBootstrapSeries == nil || p.SupportedBootstrapSeries.Size() == 0 {
		return errors.NotValidf("supported bootstrap series")
	}
	// TODO(axw) validate other things.
	return nil
}

// withDefaultControllerConstraints returns the given constraints,
// updated to choose a default instance type appropriate for a
// controller machine. We use this only if the user does not specify
// any constraints that would otherwise control the instance type
// selection.
func withDefaultControllerConstraints(cons constraints.Value) constraints.Value {
	if !cons.HasInstanceType() && !cons.HasCpuCores() && !cons.HasCpuPower() && !cons.HasMem() {
		// A default of 3.5GiB will result in machines with up to 4GiB of memory, eg
		// - 3.75GiB on AWS, Google
		// - 3.5GiB on Azure
		// - 4GiB on Rackspace etc
		var mem uint64 = 3.5 * 1024
		cons.Mem = &mem
	}
	return cons
}

// withDefaultCAASControllerConstraints returns the given constraints,
// updated to choose a default instance type appropriate for a
// controller machine. We use this only if the user does not specify
// any constraints that would otherwise control the instance type
// selection.
func withDefaultCAASControllerConstraints(cons constraints.Value) constraints.Value {
	if !cons.HasInstanceType() && !cons.HasCpuCores() && !cons.HasCpuPower() && !cons.HasMem() {
		// TODO(caas): Set memory constraints for mongod and controller containers independently.
		var mem uint64 = 1.5 * 1024
		cons.Mem = &mem
	}
	return cons
}

func bootstrapCAAS(
	ctx environs.BootstrapContext,
	environ environs.BootstrapEnviron,
	callCtx context.ProviderCallContext,
	args BootstrapParams,
	bootstrapParams environs.BootstrapParams,
) error {
	if args.BuildAgent {
		return errors.NotSupportedf("--build-agent when bootstrapping a k8s controller")
	}
	if args.BootstrapImage != "" {
		return errors.NotSupportedf("--bootstrap-image when bootstrapping a k8s controller")
	}
	if args.BootstrapSeries != "" {
		return errors.NotSupportedf("--bootstrap-series when bootstrapping a k8s controller")
	}

	constraintsValidator, err := environ.ConstraintsValidator(callCtx)
	if err != nil {
		return err
	}
	bootstrapConstraints, err := constraintsValidator.Merge(
		args.ModelConstraints, args.BootstrapConstraints,
	)
	if err != nil {
		return errors.Trace(err)
	}
	bootstrapConstraints = withDefaultCAASControllerConstraints(bootstrapConstraints)
	bootstrapParams.BootstrapConstraints = bootstrapConstraints

	result, err := environ.Bootstrap(ctx, callCtx, bootstrapParams)
	if err != nil {
		return errors.Trace(err)
	}

	podConfig, err := podcfg.NewBootstrapControllerPodConfig(
		args.ControllerConfig,
		args.ControllerName,
		result.Series,
		bootstrapConstraints,
	)
	if err != nil {
		return errors.Trace(err)
	}

	jujuVersion := jujuversion.Current
	if args.AgentVersion != nil {
		jujuVersion = *args.AgentVersion
	}
	// set agent version before finalizing bootstrap config
	if err := setBootstrapAgentVersion(environ, jujuVersion); err != nil {
		return errors.Trace(err)
	}
	podConfig.JujuVersion = jujuVersion
	podConfig.OfficialBuild = jujuversion.OfficialBuild
	if err := finalizePodBootstrapConfig(ctx, podConfig, args, environ.Config()); err != nil {
		return errors.Annotate(err, "finalizing bootstrap instance config")
	}
	if err := result.CaasBootstrapFinalizer(ctx, podConfig, args.DialOpts); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func bootstrapIAAS(
	ctx environs.BootstrapContext,
	environ environs.BootstrapEnviron,
	callCtx context.ProviderCallContext,
	args BootstrapParams,
	bootstrapParams environs.BootstrapParams,
) error {
	cfg := environ.Config()
	if authKeys := ssh.SplitAuthorisedKeys(cfg.AuthorizedKeys()); len(authKeys) == 0 {
		// Apparently this can never happen, so it's not tested. But, one day,
		// Config will act differently (it's pretty crazy that, AFAICT, the
		// authorized-keys are optional config settings... but it's impossible
		// to actually *create* a config without them)... and when it does,
		// we'll be here to catch this problem early.
		return errors.Errorf("model configuration has no authorized-keys")
	}

	_, supportsNetworking := environs.SupportsNetworking(environ)
	logger.Debugf("model %q supports application/machine networks: %v", cfg.Name(), supportsNetworking)
	disableNetworkManagement, _ := cfg.DisableNetworkManagement()
	logger.Debugf("network management by juju enabled: %v", !disableNetworkManagement)

	// Set default tools metadata source, add image metadata source,
	// then verify constraints. Providers may rely on image metadata
	// for constraint validation.
	var customImageMetadata []*imagemetadata.ImageMetadata
	if args.MetadataDir != "" {
		var err error
		customImageMetadata, err = setPrivateMetadataSources(args.MetadataDir)
		if err != nil {
			return errors.Trace(err)
		}
	}

	// If the provider allows advance discovery of the series and hw
	// characteristics of the instance we are about to bootstrap, use this
	// information to backfill in any missing series and/or arch contstraints.
	if detector, supported := environ.(environs.HardwareCharacteristicsDetector); supported {
		detectedSeries, err := detector.DetectSeries()
		if err != nil {
			return errors.Trace(err)
		}
		detectedHW, err := detector.DetectHardware()
		if err != nil {
			return errors.Trace(err)
		}

		if args.BootstrapSeries == "" && detectedSeries != "" {
			args.BootstrapSeries = detectedSeries
			logger.Debugf("auto-selecting bootstrap series %q", args.BootstrapSeries)
		}
		if args.BootstrapConstraints.Arch == nil && args.ModelConstraints.Arch == nil && detectedHW.Arch != nil {
			arch := *detectedHW.Arch
			args.BootstrapConstraints.Arch = &arch
			logger.Debugf("auto-selecting bootstrap arch %q", arch)
		}
	}

	requestedBootstrapSeries, err := coreseries.ValidateSeries(
		args.SupportedBootstrapSeries,
		args.BootstrapSeries,
		config.PreferredSeries(cfg),
	)
	if !args.Force && err != nil {
		// If the series isn't valid at all, then don't prompt users to use
		// the --force flag.
		if _, err := series.UbuntuSeriesVersion(requestedBootstrapSeries); err != nil {
			return errors.NotValidf("series %q", requestedBootstrapSeries)
		}
		return errors.Annotatef(err, "use --force to override")
	}
	bootstrapSeries := &requestedBootstrapSeries

	var bootstrapArchForImageSearch string
	if args.BootstrapConstraints.Arch != nil {
		bootstrapArchForImageSearch = *args.BootstrapConstraints.Arch
	} else if args.ModelConstraints.Arch != nil {
		bootstrapArchForImageSearch = *args.ModelConstraints.Arch
	} else {
		bootstrapArchForImageSearch = arch.HostArch()
		// We no longer support i386.
		if bootstrapArchForImageSearch == arch.I386 {
			bootstrapArchForImageSearch = arch.AMD64
		}
	}

	ctx.Verbosef("Loading image metadata")
	imageMetadata, err := bootstrapImageMetadata(environ,
		bootstrapSeries,
		bootstrapArchForImageSearch,
		args.BootstrapImage,
		&customImageMetadata,
	)
	if err != nil {
		return errors.Trace(err)
	}

	// We want to determine a list of valid architectures for which to pick tools and images.
	// This includes architectures from custom and other available image metadata.
	architectures := set.NewStrings()
	if len(customImageMetadata) > 0 {
		for _, customMetadata := range customImageMetadata {
			architectures.Add(customMetadata.Arch)
		}
	}
	if len(imageMetadata) > 0 {
		for _, iMetadata := range imageMetadata {
			architectures.Add(iMetadata.Arch)
		}
	}
	bootstrapParams.ImageMetadata = imageMetadata

	constraintsValidator, err := environ.ConstraintsValidator(callCtx)
	if err != nil {
		return err
	}
	constraintsValidator.UpdateVocabulary(constraints.Arch, architectures.SortedValues())

	bootstrapConstraints, err := constraintsValidator.Merge(
		args.ModelConstraints, args.BootstrapConstraints,
	)
	if err != nil {
		return errors.Trace(err)
	}
	// The follow is used to determine if we should apply the default
	// constraints when we bootstrap. Generally speaking this should always be
	// applied, but there are exceptions to the rule e.g. local LXD
	if checker, ok := environ.(environs.DefaultConstraintsChecker); !ok || checker.ShouldApplyControllerConstraints() {
		bootstrapConstraints = withDefaultControllerConstraints(bootstrapConstraints)
	}
	bootstrapParams.BootstrapConstraints = bootstrapConstraints

	var bootstrapArch string
	if bootstrapConstraints.Arch != nil {
		bootstrapArch = *bootstrapConstraints.Arch
	} else {
		// If no arch is specified as a constraint and we couldn't
		// auto-discover the arch from the provider, we'll fall back
		// to bootstrapping on on the same arch as the CLI client.
		bootstrapArch = localToolsArch()

		// We no longer support controllers on i386. If we are
		// bootstrapping from an i386 client, we'll look for amd64
		// tools.
		if bootstrapArch == arch.I386 {
			bootstrapArch = arch.AMD64
		}
	}

	agentVersion := jujuversion.Current
	var availableTools coretools.List
	if !args.BuildAgent {
		latestPatchTxt := ""
		versionTxt := fmt.Sprintf("%v", args.AgentVersion)
		if args.AgentVersion == nil {
			latestPatchTxt = "latest patch of "
			versionTxt = fmt.Sprintf("%v.%v", agentVersion.Major, agentVersion.Minor)
		}
		ctx.Infof("Looking for %vpackaged Juju agent version %s for %s", latestPatchTxt, versionTxt, bootstrapArch)
		availableTools, err = findPackagedTools(environ, args.AgentVersion, &bootstrapArch, bootstrapSeries)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		if len(availableTools) != 0 && args.AgentVersion == nil {
			// If agent version was not specified in the arguments,
			// we always want the latest/newest available.
			agentVersion, availableTools = availableTools.Newest()
		}
	}
	// If there are no prepackaged tools and a specific version has not been
	// requested, look for or build a local binary.
	var builtTools *sync.BuiltAgent
	if len(availableTools) == 0 && (args.AgentVersion == nil || isCompatibleVersion(*args.AgentVersion, jujuversion.Current)) {
		if args.BuildAgentTarball == nil {
			return errors.New("cannot build agent binary to upload")
		}
		if err = validateUploadAllowed(environ, &bootstrapArch, bootstrapSeries, constraintsValidator); err != nil {
			return err
		}
		if args.BuildAgent {
			ctx.Infof("Building local Juju agent binary version %s for %s", args.AgentVersion, bootstrapArch)
		} else {
			ctx.Infof("No packaged binary found, preparing local Juju agent binary")
		}
		var forceVersion version.Number
		availableTools, forceVersion, err = locallyBuildableTools(bootstrapSeries)
		if err != nil {
			return errors.Annotate(err, "cannot package bootstrap agent binary")
		}
		builtTools, err = args.BuildAgentTarball(args.BuildAgent, &forceVersion, cfg.AgentStream())
		if err != nil {
			return errors.Annotate(err, "cannot package bootstrap agent binary")
		}
		defer os.RemoveAll(builtTools.Dir)
		// Combine the built agent information with the list of
		// available tools.
		for i, tool := range availableTools {
			if tool.URL != "" {
				continue
			}
			filename := filepath.Join(builtTools.Dir, builtTools.StorageName)
			tool.URL = fmt.Sprintf("file://%s", filename)
			tool.Size = builtTools.Size
			tool.SHA256 = builtTools.Sha256Hash

			// Use the version from the built tools but with the
			// corrected series and arch - this ensures the build
			// number is right if we found a valid official build.
			version := builtTools.Version
			version.Series = tool.Version.Series
			version.Arch = tool.Version.Arch
			// But if not an official build, use the forced version.
			if !builtTools.Official {
				version.Number = forceVersion
			}
			tool.Version = version
			availableTools[i] = tool
		}
	}
	if len(availableTools) == 0 {
		return errors.New(noToolsMessage)
	}
	bootstrapParams.AvailableTools = availableTools

	// TODO (anastasiamac 2018-02-02) By this stage, we will have a list
	// of available tools (agent binaries) but they should all be the same
	// version. Need to do check here, otherwise the provider.Bootstrap call
	// may fail. This also means that compatibility check, currently done
	// after provider.Bootstrap call in getBootstrapToolsVersion,
	// should be done here.

	// If we're uploading, we must override agent-version;
	// if we're not uploading, we want to ensure we have an
	// agent-version set anyway, to appease FinishInstanceConfig.
	// In the latter case, setBootstrapTools will later set
	// agent-version to the correct thing.
	if args.AgentVersion != nil {
		agentVersion = *args.AgentVersion
	}
	if cfg, err = cfg.Apply(map[string]interface{}{
		"agent-version": agentVersion.String(),
	}); err != nil {
		return errors.Trace(err)
	}
	if err = environ.SetConfig(cfg); err != nil {
		return errors.Trace(err)
	}

	ctx.Verbosef("Starting new instance for initial controller")

	result, err := environ.Bootstrap(ctx, callCtx, bootstrapParams)
	if err != nil {
		return errors.Trace(err)
	}

	publicKey, err := userPublicSigningKey()
	if err != nil {
		return errors.Trace(err)
	}
	instanceConfig, err := instancecfg.NewBootstrapInstanceConfig(
		args.ControllerConfig,
		bootstrapParams.BootstrapConstraints,
		args.ModelConstraints,
		result.Series,
		publicKey,
		args.ExtraAgentValuesForTesting,
	)
	if err != nil {
		return errors.Trace(err)
	}

	matchingTools, err := bootstrapParams.AvailableTools.Match(coretools.Filter{
		Arch:   result.Arch,
		Series: result.Series,
	})
	if err != nil {
		return errors.Annotatef(err, "expected tools for %q", result.Series)
	}
	selectedToolsList, err := getBootstrapToolsVersion(matchingTools)
	if err != nil {
		return errors.Trace(err)
	}
	// We set agent-version to the newest version, so the agent will immediately upgrade itself.
	// Note that this only is relevant if a specific agent version has not been requested, since
	// in that case the specific version will be the only version available.
	newestToolVersion, _ := matchingTools.Newest()
	// set agent version before finalizing bootstrap config
	if err := setBootstrapAgentVersion(environ, newestToolVersion); err != nil {
		return errors.Trace(err)
	}

	ctx.Infof("Installing Juju agent on bootstrap instance")
	if err := instanceConfig.SetTools(selectedToolsList); err != nil {
		return errors.Trace(err)
	}

	if err := instanceConfig.SetSnapSource(args.JujuDbSnapPath, args.JujuDbSnapAssertionsPath); err != nil {
		return errors.Trace(err)
	}

	var environVersion int
	if e, ok := environ.(environs.Environ); ok {
		environVersion = e.Provider().Version()
	}
	// Make sure we have the most recent environ config as the specified
	// tools version has been updated there.
	if err := finalizeInstanceBootstrapConfig(
		ctx, instanceConfig, args, environ.Config(), environVersion, customImageMetadata,
	); err != nil {
		return errors.Annotate(err, "finalizing bootstrap instance config")
	}
	if err := result.CloudBootstrapFinalizer(ctx, instanceConfig, args.DialOpts); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// Bootstrap bootstraps the given environment. The supplied constraints are
// used to provision the instance, and are also set within the bootstrapped
// environment.
func Bootstrap(
	ctx environs.BootstrapContext,
	environ environs.BootstrapEnviron,
	callCtx context.ProviderCallContext,
	args BootstrapParams,
) error {
	if err := args.Validate(); err != nil {
		return errors.Annotate(err, "validating bootstrap parameters")
	}
	bootstrapParams := environs.BootstrapParams{
		CloudName:                  args.Cloud.Name,
		CloudRegion:                args.CloudRegion,
		ControllerConfig:           args.ControllerConfig,
		ModelConstraints:           args.ModelConstraints,
		BootstrapSeries:            args.BootstrapSeries,
		SupportedBootstrapSeries:   args.SupportedBootstrapSeries,
		Placement:                  args.Placement,
		Force:                      args.Force,
		ExtraAgentValuesForTesting: args.ExtraAgentValuesForTesting,
	}
	doBootstrap := bootstrapIAAS
	if jujucloud.CloudIsCAAS(args.Cloud) {
		doBootstrap = bootstrapCAAS
	}

	if err := doBootstrap(ctx, environ, callCtx, args, bootstrapParams); err != nil {
		return errors.Trace(err)
	}
	ctx.Infof("Bootstrap agent now started")
	return nil
}

func finalizeInstanceBootstrapConfig(
	ctx environs.BootstrapContext,
	icfg *instancecfg.InstanceConfig,
	args BootstrapParams,
	cfg *config.Config,
	environVersion int,
	customImageMetadata []*imagemetadata.ImageMetadata,
) error {
	if icfg.APIInfo != nil || icfg.Controller.MongoInfo != nil {
		return errors.New("machine configuration already has api/state info")
	}
	controllerCfg := icfg.Controller.Config
	caCert, hasCACert := controllerCfg.CACert()
	if !hasCACert {
		return errors.New("controller configuration has no ca-cert")
	}
	icfg.APIInfo = &api.Info{
		Password: args.AdminSecret,
		CACert:   caCert,
		ModelTag: names.NewModelTag(cfg.UUID()),
	}
	icfg.Controller.MongoInfo = &mongo.MongoInfo{
		Password: args.AdminSecret,
		Info:     mongo.Info{CACert: caCert},
	}

	authority, err := pki.NewDefaultAuthorityPemCAKey(
		[]byte(caCert), []byte(args.CAPrivateKey))
	if err != nil {
		return errors.Annotate(err, "loading juju certificate authority")
	}

	leaf, err := authority.LeafRequestForGroup(pki.DefaultLeafGroup).
		AddDNSNames(controller.DefaultDNSNames...).
		Commit()

	if err != nil {
		return errors.Annotate(err, "make juju default controller cert")
	}

	cert, key, err := leaf.ToPemParts()
	if err != nil {
		return errors.Annotate(err, "encoding default controller cert to pem")
	}

	icfg.Bootstrap.StateServingInfo = controller.StateServingInfo{
		StatePort:    controllerCfg.StatePort(),
		APIPort:      controllerCfg.APIPort(),
		Cert:         string(cert),
		PrivateKey:   string(key),
		CAPrivateKey: args.CAPrivateKey,
	}
	vers, ok := cfg.AgentVersion()
	if !ok {
		return errors.New("controller model configuration has no agent-version")
	}

	icfg.Bootstrap.ControllerModelConfig = cfg
	icfg.Bootstrap.ControllerModelEnvironVersion = environVersion
	icfg.Bootstrap.CustomImageMetadata = customImageMetadata
	icfg.Bootstrap.ControllerCloud = args.Cloud
	icfg.Bootstrap.ControllerCloudRegion = args.CloudRegion
	icfg.Bootstrap.ControllerCloudCredential = args.CloudCredential
	icfg.Bootstrap.ControllerCloudCredentialName = args.CloudCredentialName
	icfg.Bootstrap.ControllerConfig = args.ControllerConfig
	icfg.Bootstrap.ControllerInheritedConfig = args.ControllerInheritedConfig
	icfg.Bootstrap.RegionInheritedConfig = args.Cloud.RegionConfig
	icfg.Bootstrap.HostedModelConfig = args.HostedModelConfig
	icfg.Bootstrap.Timeout = args.DialOpts.Timeout
	icfg.Bootstrap.GUI = guiArchive(args.GUIDataSourceBaseURL, cfg.GUIStream(), vers.Major, vers.Minor, true, func(msg string) {
		ctx.Infof(msg)
	})
	icfg.Bootstrap.JujuDbSnapPath = args.JujuDbSnapPath
	icfg.Bootstrap.JujuDbSnapAssertionsPath = args.JujuDbSnapAssertionsPath
	return nil
}

func finalizePodBootstrapConfig(
	ctx environs.BootstrapContext,
	pcfg *podcfg.ControllerPodConfig,
	args BootstrapParams,
	cfg *config.Config,
) error {
	if pcfg.APIInfo != nil || pcfg.Controller.MongoInfo != nil {
		return errors.New("machine configuration already has api/state info")
	}

	controllerCfg := pcfg.Controller.Config
	caCert, hasCACert := controllerCfg.CACert()
	if !hasCACert {
		return errors.New("controller configuration has no ca-cert")
	}
	pcfg.APIInfo = &api.Info{
		Password: args.AdminSecret,
		CACert:   caCert,
		ModelTag: names.NewModelTag(cfg.UUID()),
	}
	pcfg.Controller.MongoInfo = &mongo.MongoInfo{
		Password: args.AdminSecret,
		Info:     mongo.Info{CACert: caCert},
	}

	authority, err := pki.NewDefaultAuthorityPemCAKey(
		[]byte(caCert), []byte(args.CAPrivateKey))
	if err != nil {
		return errors.Annotate(err, "loading juju certificate authority")
	}

	// We generate a controller certificate with a set of well known static dns
	// names. IP addresses are left for other workers to make subsequent
	// certificates.
	leaf, err := authority.LeafRequestForGroup(pki.DefaultLeafGroup).
		AddDNSNames(controller.DefaultDNSNames...).
		Commit()

	if err != nil {
		return errors.Annotate(err, "make juju default controller cert")
	}

	cert, key, err := leaf.ToPemParts()
	if err != nil {
		return errors.Annotate(err, "encoding default controller cert to pem")
	}

	pcfg.Bootstrap.StateServingInfo = controller.StateServingInfo{
		StatePort:    controllerCfg.StatePort(),
		APIPort:      controllerCfg.APIPort(),
		Cert:         string(cert),
		PrivateKey:   string(key),
		CAPrivateKey: args.CAPrivateKey,
	}
	if _, ok := cfg.AgentVersion(); !ok {
		return errors.New("controller model configuration has no agent-version")
	}

	pcfg.AgentEnvironment = make(map[string]string)
	for k, v := range args.ExtraAgentValuesForTesting {
		pcfg.AgentEnvironment[k] = v
	}

	vers, ok := cfg.AgentVersion()
	if !ok {
		return errors.New("controller model configuration has no agent-version")
	}

	pcfg.Bootstrap.ControllerModelConfig = cfg
	pcfg.Bootstrap.ControllerCloud = args.Cloud
	pcfg.Bootstrap.ControllerCloudRegion = args.CloudRegion
	pcfg.Bootstrap.ControllerCloudCredential = args.CloudCredential
	pcfg.Bootstrap.ControllerCloudCredentialName = args.CloudCredentialName
	pcfg.Bootstrap.ControllerConfig = args.ControllerConfig
	pcfg.Bootstrap.ControllerInheritedConfig = args.ControllerInheritedConfig
	pcfg.Bootstrap.HostedModelConfig = args.HostedModelConfig
	pcfg.Bootstrap.Timeout = args.DialOpts.Timeout
	pcfg.Bootstrap.ControllerServiceType = args.ControllerServiceType
	pcfg.Bootstrap.ControllerExternalName = args.ControllerExternalName
	pcfg.Bootstrap.ControllerExternalIPs = append([]string(nil), args.ControllerExternalIPs...)
	pcfg.Bootstrap.GUI = guiArchive(args.GUIDataSourceBaseURL, cfg.GUIStream(), vers.Major, vers.Minor, false, func(msg string) {
		ctx.Infof(msg)
	})
	return nil
}

func userPublicSigningKey() (string, error) {
	signingKeyFile := os.Getenv("JUJU_STREAMS_PUBLICKEY_FILE")
	signingKey := ""
	if signingKeyFile != "" {
		path, err := utils.NormalizePath(signingKeyFile)
		if err != nil {
			return "", errors.Annotatef(err, "cannot expand key file path: %s", signingKeyFile)
		}
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return "", errors.Annotatef(err, "invalid public key file: %s", path)
		}
		signingKey = string(b)
	}
	return signingKey, nil
}

// bootstrapImageMetadata returns the image metadata to use for bootstrapping
// the given environment. If the environment provider does not make use of
// simplestreams, no metadata will be returned.
//
// If a bootstrap image ID is specified, image metadata will be synthesised
// using that image ID, and the architecture and series specified by the
// initiator. In addition, the custom image metadata that is saved into the
// state database will have the synthesised image metadata added to it.
func bootstrapImageMetadata(
	environ environs.BootstrapEnviron,
	bootstrapSeries *string,
	bootstrapArch string,
	bootstrapImageId string,
	customImageMetadata *[]*imagemetadata.ImageMetadata,
) ([]*imagemetadata.ImageMetadata, error) {

	hasRegion, ok := environ.(simplestreams.HasRegion)
	if !ok {
		if bootstrapImageId != "" {
			// We only support specifying image IDs for providers
			// that use simplestreams for now.
			return nil, errors.NotSupportedf(
				"specifying bootstrap image for %q provider",
				environ.Config().Type(),
			)
		}
		// No region, no metadata.
		return nil, nil
	}
	region, err := hasRegion.Region()
	if err != nil {
		return nil, errors.Trace(err)
	}

	if bootstrapImageId != "" {
		if bootstrapSeries == nil {
			return nil, errors.NotValidf("no series specified with bootstrap image")
		}
		seriesVersion, err := series.SeriesVersion(*bootstrapSeries)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// The returned metadata does not have information about the
		// storage or virtualisation type. Any provider that wants to
		// filter on those properties should allow for empty values.
		meta := &imagemetadata.ImageMetadata{
			Id:         bootstrapImageId,
			Arch:       bootstrapArch,
			Version:    seriesVersion,
			RegionName: region.Region,
			Endpoint:   region.Endpoint,
			Stream:     environ.Config().ImageStream(),
		}
		*customImageMetadata = append(*customImageMetadata, meta)
		return []*imagemetadata.ImageMetadata{meta}, nil
	}

	// For providers that support making use of simplestreams
	// image metadata, search public image metadata. We need
	// to pass this onto Bootstrap for selecting images.
	sources, err := environs.ImageMetadataSources(environ)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// This constraint will search image metadata for all supported architectures and series.
	imageConstraint, err := imagemetadata.NewImageConstraint(simplestreams.LookupParams{
		CloudSpec: region,
		Stream:    environ.Config().ImageStream(),
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	logger.Debugf("constraints for image metadata lookup %v", imageConstraint)

	// Get image metadata from all data sources.
	// Since order of data source matters, order of image metadata matters too. Append is important here.
	var publicImageMetadata []*imagemetadata.ImageMetadata
	for _, source := range sources {
		sourceMetadata, _, err := imagemetadata.Fetch([]simplestreams.DataSource{source}, imageConstraint)
		if err != nil {
			logger.Debugf("ignoring image metadata in %s: %v", source.Description(), err)
			// Just keep looking...
			continue
		}
		logger.Debugf("found %d image metadata in %s", len(sourceMetadata), source.Description())
		publicImageMetadata = append(publicImageMetadata, sourceMetadata...)
	}

	logger.Debugf("found %d image metadata from all image data sources", len(publicImageMetadata))
	return publicImageMetadata, nil
}

// getBootstrapToolsVersion returns the newest tools from the given tools list.
func getBootstrapToolsVersion(possibleTools coretools.List) (coretools.List, error) {
	if len(possibleTools) == 0 {
		return nil, errors.New("no bootstrap agent binaries available")
	}
	var newVersion version.Number
	newVersion, toolsList := possibleTools.Newest()
	logger.Infof("newest version: %s", newVersion)
	bootstrapVersion := newVersion
	// We should only ever bootstrap the exact same version as the client,
	// or we risk bootstrap incompatibility.
	if !isCompatibleVersion(newVersion, jujuversion.Current) {
		compatibleVersion, compatibleTools := findCompatibleTools(possibleTools, jujuversion.Current)
		if len(compatibleTools) == 0 {
			logger.Infof(
				"failed to find %s agent binaries, will attempt to use %s",
				jujuversion.Current, newVersion,
			)
		} else {
			bootstrapVersion, toolsList = compatibleVersion, compatibleTools
		}
	}
	logger.Infof("picked bootstrap agent binary version: %s", bootstrapVersion)
	return toolsList, nil
}

// setBootstrapAgentVersion updates the agent-version configuration attribute.
func setBootstrapAgentVersion(environ environs.Configer, toolsVersion version.Number) error {
	cfg := environ.Config()
	if agentVersion, _ := cfg.AgentVersion(); agentVersion != toolsVersion {
		cfg, err := cfg.Apply(map[string]interface{}{
			"agent-version": toolsVersion.String(),
		})
		if err == nil {
			err = environ.SetConfig(cfg)
		}
		if err != nil {
			return errors.Errorf("failed to update model configuration: %v", err)
		}
	}
	return nil
}

// findCompatibleTools finds tools in the list that have the same major, minor
// and patch level as jujuversion.Current.
//
// Build number is not important to match; uploaded tools will have
// incremented build number, and we want to match them.
func findCompatibleTools(possibleTools coretools.List, version version.Number) (version.Number, coretools.List) {
	var compatibleTools coretools.List
	for _, tools := range possibleTools {
		if isCompatibleVersion(tools.Version.Number, version) {
			compatibleTools = append(compatibleTools, tools)
		}
	}
	return compatibleTools.Newest()
}

func isCompatibleVersion(v1, v2 version.Number) bool {
	x := v1.ToPatch()
	y := v2.ToPatch()
	return x.Compare(y) == 0
}

// setPrivateMetadataSources verifies the specified metadataDir exists,
// uses it to set the default agent metadata source for agent binaries,
// and adds an image metadata source after verifying the contents. If the
// directory ends in tools, only the default tools metadata source will be
// set. Same for images.
func setPrivateMetadataSources(metadataDir string) ([]*imagemetadata.ImageMetadata, error) {
	if _, err := os.Stat(metadataDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Annotate(err, "cannot access simplestreams metadata directory")
		}
		return nil, errors.NotFoundf("simplestreams metadata source: %s", metadataDir)
	}

	agentBinaryMetadataDir := metadataDir
	ending := filepath.Base(agentBinaryMetadataDir)
	if ending != storage.BaseToolsPath {
		agentBinaryMetadataDir = filepath.Join(metadataDir, storage.BaseToolsPath)
	}
	if _, err := os.Stat(agentBinaryMetadataDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Annotate(err, "cannot access agent metadata")
		}
		logger.Debugf("no agent directory found, using default agent metadata source: %s", tools.DefaultBaseURL)
	} else {
		if ending == storage.BaseToolsPath {
			// As the specified metadataDir ended in 'tools'
			// assume that is the only metadata to find and return.
			tools.DefaultBaseURL = filepath.Dir(metadataDir)
			logger.Debugf("setting default agent metadata source: %s", tools.DefaultBaseURL)
			return nil, nil
		} else {
			tools.DefaultBaseURL = metadataDir
			logger.Debugf("setting default agent metadata source: %s", tools.DefaultBaseURL)
		}
	}

	imageMetadataDir := metadataDir
	ending = filepath.Base(imageMetadataDir)
	if ending != storage.BaseImagesPath {
		imageMetadataDir = filepath.Join(metadataDir, storage.BaseImagesPath)
	}
	if _, err := os.Stat(imageMetadataDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Annotate(err, "cannot access image metadata")
		}
		return nil, nil
	} else {
		logger.Debugf("setting default image metadata source: %s", imageMetadataDir)
	}

	baseURL := fmt.Sprintf("file://%s", filepath.ToSlash(imageMetadataDir))
	publicKey, err := simplestreams.UserPublicSigningKey()
	if err != nil {
		return nil, errors.Trace(err)
	}
	// TODO: (hml) 2020-01-08
	// Why ignore the the model-config "ssl-hostname-verification" value in
	// the config here? Its default value is true.
	dataSourceConfig := simplestreams.Config{
		Description:          "bootstrap metadata",
		BaseURL:              baseURL,
		PublicSigningKey:     publicKey,
		HostnameVerification: utils.NoVerifySSLHostnames,
		Priority:             simplestreams.CUSTOM_CLOUD_DATA,
	}
	if err := dataSourceConfig.Validate(); err != nil {
		return nil, errors.Annotate(err, "simplestreams config validation failed")
	}
	dataSource := simplestreams.NewDataSource(dataSourceConfig)

	// Read the image metadata, as we'll want to upload it to the environment.
	imageConstraint, err := imagemetadata.NewImageConstraint(simplestreams.LookupParams{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	existingMetadata, _, err := imagemetadata.Fetch([]simplestreams.DataSource{dataSource}, imageConstraint)
	if err != nil && !errors.IsNotFound(err) {
		return nil, errors.Annotate(err, "cannot read image metadata")
	}

	// Add an image metadata datasource for constraint validation, etc.
	environs.RegisterUserImageDataSourceFunc("bootstrap metadata", func(environs.Environ) (simplestreams.DataSource, error) {
		return dataSource, nil
	})
	logger.Infof("custom image metadata added to search path")
	return existingMetadata, nil
}

// guiArchive returns information on the GUI archive that will be uploaded
// to the controller. Possible errors in retrieving the GUI archive information
// do not prevent the model to be bootstrapped. If dataSourceBaseURL is
// non-empty, remote GUI archive info is retrieved from simplestreams using it
// as the base URL. The given logProgress function is used to inform users
// about errors or progress in setting up the Juju GUI.
func guiArchive(dataSourceBaseURL, stream string, major, minor int, allowLocal bool, logProgress func(string)) *coretools.GUIArchive {
	// The environment variable is only used for development purposes.
	path := os.Getenv("JUJU_GUI")
	if path != "" && !allowLocal {
		// TODO(wallyworld) - support local archive on k8s controllers at bootstrap
		// It can't be passed the same way as on IAAS as it's too large.
		logProgress("Dashboard from local archive on bootstrap not supported")
	} else if path != "" {
		vers, err := guiVersion(path)
		if err != nil {
			logProgress(fmt.Sprintf("Cannot use Juju Dashboard at %q: %s", path, err))
			return nil
		}
		hash, size, err := hashAndSize(path)
		if err != nil {
			logProgress(fmt.Sprintf("Cannot use Juju Dashboard at %q: %s", path, err))
			return nil
		}
		logProgress(fmt.Sprintf("Fetching Juju Dashboard %s from local archive", vers))
		return &coretools.GUIArchive{
			Version: vers,
			URL:     "file://" + filepath.ToSlash(path),
			SHA256:  hash,
			Size:    size,
		}
	}
	// Check if the user requested to bootstrap with no GUI.
	if dataSourceBaseURL == "" {
		logProgress("Juju Dashboard installation has been disabled")
		return nil
	}
	// Fetch GUI archives info from simplestreams.
	source := gui.NewDataSource(dataSourceBaseURL)
	allMeta, err := guiFetchMetadata(stream, major, minor, source)
	if err != nil {
		logProgress(fmt.Sprintf("Unable to fetch Juju Dashboard info: %s", err))
		return nil
	}
	if len(allMeta) == 0 {
		logProgress("No available Juju Dashboard archives found")
		return nil
	}
	// Metadata info are returned in descending version order.
	logProgress(fmt.Sprintf("Fetching Juju Dashboard %s", allMeta[0].Version))
	return &coretools.GUIArchive{
		Version: allMeta[0].Version,
		URL:     allMeta[0].FullPath,
		SHA256:  allMeta[0].SHA256,
		Size:    allMeta[0].Size,
	}
}

// guiFetchMetadata is defined for testing purposes.
var guiFetchMetadata = gui.FetchMetadata

// guiVersion retrieves the GUI version from the juju-gui-* directory included
// in the bz2 archive at the given path.
func guiVersion(path string) (version.Number, error) {
	var number version.Number
	f, err := os.Open(path)
	if err != nil {
		return number, errors.Annotate(err, "cannot open Juju Dashboard archive")
	}
	defer f.Close()
	return gui.DashboardArchiveVersion(f)
}

// hashAndSize calculates and returns the SHA256 hash and the size of the file
// located at the given path.
func hashAndSize(path string) (hash string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, errors.Mask(err)
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, errors.Mask(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), size, nil
}

// Cancelled returns an error that satisfies IsCancelled.
func Cancelled() error {
	return errCancelled
}

// IsCancelled reports whether err is a "bootstrap cancelled" error.
func IsCancelled(err error) bool {
	return errors.Cause(err) == errCancelled
}
