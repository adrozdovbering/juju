// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package provisioner_test

import (
	"fmt"
	"strings"
	"time"

	"github.com/juju/errors"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	"github.com/juju/utils/arch"
	"github.com/juju/utils/series"
	"github.com/juju/utils/set"
	"github.com/juju/version"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v2"
	worker "gopkg.in/juju/worker.v1"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/api"
	apiprovisioner "github.com/juju/juju/api/provisioner"
	apiserverprovisioner "github.com/juju/juju/apiserver/facades/agent/provisioner"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/controller/authentication"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/filestorage"
	"github.com/juju/juju/environs/imagemetadata"
	imagetesting "github.com/juju/juju/environs/imagemetadata/testing"
	envtesting "github.com/juju/juju/environs/testing"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/juju/testing"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/dummy"
	"github.com/juju/juju/state"
	"github.com/juju/juju/state/cloudimagemetadata"
	"github.com/juju/juju/state/multiwatcher"
	"github.com/juju/juju/status"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/storage/poolmanager"
	coretesting "github.com/juju/juju/testing"
	coretools "github.com/juju/juju/tools"
	jujuversion "github.com/juju/juju/version"
	"github.com/juju/juju/worker/provisioner"
)

type CommonProvisionerSuite struct {
	testing.JujuConnSuite
	op  <-chan dummy.Operation
	cfg *config.Config
	// defaultConstraints are used when adding a machine and then later in test assertions.
	defaultConstraints constraints.Value

	st          api.Connection
	provisioner *apiprovisioner.State
}

func (s *CommonProvisionerSuite) assertProvisionerObservesConfigChanges(c *gc.C, p provisioner.Provisioner) {
	// Inject our observer into the provisioner
	cfgObserver := make(chan *config.Config, 1)
	provisioner.SetObserver(p, cfgObserver)

	// Switch to reaping on All machines.
	attrs := map[string]interface{}{
		config.ProvisionerHarvestModeKey: config.HarvestAll.String(),
	}
	err := s.State.UpdateModelConfig(attrs, nil)
	c.Assert(err, jc.ErrorIsNil)

	s.BackingState.StartSync()

	// Wait for the PA to load the new configuration. We wait for the change we expect
	// like this because sometimes we pick up the initial harvest config (destroyed)
	// rather than the one we change to (all).
	received := []string{}
	timeout := time.After(coretesting.LongWait)
	for {
		select {
		case newCfg := <-cfgObserver:
			if newCfg.ProvisionerHarvestMode().String() == config.HarvestAll.String() {
				return
			}
			received = append(received, newCfg.ProvisionerHarvestMode().String())
		case <-time.After(coretesting.ShortWait):
			s.BackingState.StartSync()
		case <-timeout:
			if len(received) == 0 {
				c.Fatalf("PA did not action config change")
			} else {
				c.Fatalf("timed out waiting for config to change to '%s', received %+v",
					config.HarvestAll.String(), received)
			}
		}
	}
}

type ProvisionerSuite struct {
	CommonProvisionerSuite
}

var _ = gc.Suite(&ProvisionerSuite{})

func (s *CommonProvisionerSuite) SetUpSuite(c *gc.C) {
	s.JujuConnSuite.SetUpSuite(c)
	s.defaultConstraints = constraints.MustParse("arch=amd64 mem=4G cores=1 root-disk=8G")
}

func (s *CommonProvisionerSuite) SetUpTest(c *gc.C) {
	s.JujuConnSuite.SetUpTest(c)

	// We do not want to pull published image metadata for tests...
	imagetesting.PatchOfficialDataSources(&s.CleanupSuite, "")
	// We want an image to start test instances
	err := s.State.CloudImageMetadataStorage.SaveMetadata([]cloudimagemetadata.Metadata{{
		MetadataAttributes: cloudimagemetadata.MetadataAttributes{
			Region:          "region",
			Series:          "trusty",
			Arch:            "amd64",
			VirtType:        "",
			RootStorageType: "",
			Source:          "test",
			Stream:          "released",
		},
		Priority: 10,
		ImageId:  "-999",
	}})
	c.Assert(err, jc.ErrorIsNil)

	// Create the operations channel with more than enough space
	// for those tests that don't listen on it.
	op := make(chan dummy.Operation, 500)
	dummy.Listen(op)
	s.op = op

	cfg, err := s.State.ModelConfig()
	c.Assert(err, jc.ErrorIsNil)
	s.cfg = cfg

	// Create a machine for the dummy bootstrap instance,
	// so the provisioner doesn't destroy it.
	insts, err := s.Environ.Instances([]instance.Id{dummy.BootstrapInstanceId})
	c.Assert(err, jc.ErrorIsNil)
	addrs, err := insts[0].Addresses()
	c.Assert(err, jc.ErrorIsNil)
	machine, err := s.State.AddOneMachine(state.MachineTemplate{
		Addresses:  addrs,
		Series:     "quantal",
		Nonce:      agent.BootstrapNonce,
		InstanceId: dummy.BootstrapInstanceId,
		Jobs:       []state.MachineJob{state.JobManageModel},
	})
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(machine.Id(), gc.Equals, "0")

	current := version.Binary{
		Number: jujuversion.Current,
		Arch:   arch.HostArch(),
		Series: series.MustHostSeries(),
	}
	err = machine.SetAgentVersion(current)
	c.Assert(err, jc.ErrorIsNil)

	password, err := utils.RandomPassword()
	c.Assert(err, jc.ErrorIsNil)
	err = machine.SetPassword(password)
	c.Assert(err, jc.ErrorIsNil)

	s.st = s.OpenAPIAsMachine(c, machine.Tag(), password, agent.BootstrapNonce)
	c.Assert(s.st, gc.NotNil)
	c.Logf("API: login as %q successful", machine.Tag())
	s.provisioner = apiprovisioner.NewState(s.st)
	c.Assert(s.provisioner, gc.NotNil)
}

// stop stops a Worker.
func stop(c *gc.C, w worker.Worker) {
	c.Assert(worker.Stop(w), jc.ErrorIsNil)
}

func (s *CommonProvisionerSuite) startUnknownInstance(c *gc.C, id string) instance.Instance {
	instance, _ := testing.AssertStartInstance(c, s.Environ, s.ControllerConfig.ControllerUUID(), id)
	select {
	case o := <-s.op:
		switch o := o.(type) {
		case dummy.OpStartInstance:
		default:
			c.Fatalf("unexpected operation %#v", o)
		}
	case <-time.After(coretesting.LongWait):
		c.Fatalf("timed out waiting for startinstance operation")
	}
	return instance
}

func (s *CommonProvisionerSuite) checkStartInstance(c *gc.C, m *state.Machine) instance.Instance {
	return s.checkStartInstanceCustom(c, m, "pork", s.defaultConstraints, nil, nil, nil, nil, nil, true)
}

func (s *CommonProvisionerSuite) checkStartInstanceCustom(
	c *gc.C, m *state.Machine,
	secret string, cons constraints.Value,
	networkInfo []network.InterfaceInfo,
	subnetsToZones map[network.Id][]string,
	volumes []storage.Volume,
	volumeAttachments []storage.VolumeAttachment,
	checkPossibleTools coretools.List,
	waitInstanceId bool,
) (
	inst instance.Instance,
) {
	s.BackingState.StartSync()
	for {
		select {
		case o := <-s.op:
			switch o := o.(type) {
			case dummy.OpStartInstance:
				inst = o.Instance
				if waitInstanceId {
					s.waitInstanceId(c, m, inst.Id())
				}

				// Check the instance was started with the expected params.
				c.Assert(o.MachineId, gc.Equals, m.Id())
				nonceParts := strings.SplitN(o.MachineNonce, ":", 2)
				c.Assert(nonceParts, gc.HasLen, 2)
				c.Assert(nonceParts[0], gc.Equals, names.NewMachineTag("0").String())
				c.Assert(nonceParts[1], jc.Satisfies, utils.IsValidUUIDString)
				c.Assert(o.Secret, gc.Equals, secret)
				c.Assert(o.SubnetsToZones, jc.DeepEquals, subnetsToZones)
				c.Assert(o.NetworkInfo, jc.DeepEquals, networkInfo)
				c.Assert(o.Volumes, jc.DeepEquals, volumes)
				c.Assert(o.VolumeAttachments, jc.DeepEquals, volumeAttachments)

				var jobs []multiwatcher.MachineJob
				for _, job := range m.Jobs() {
					jobs = append(jobs, job.ToParams())
				}
				c.Assert(o.Jobs, jc.SameContents, jobs)

				if checkPossibleTools != nil {
					for _, t := range o.PossibleTools {
						url := fmt.Sprintf("https://%s/model/%s/tools/%s",
							s.st.Addr(), coretesting.ModelTag.Id(), t.Version)
						c.Check(t.URL, gc.Equals, url)
						t.URL = ""
					}
					for _, t := range checkPossibleTools {
						t.URL = ""
					}
					c.Assert(o.PossibleTools, gc.DeepEquals, checkPossibleTools)
				}

				// All provisioned machines in this test suite have
				// their hardware characteristics attributes set to
				// the same values as the constraints due to the dummy
				// environment being used.
				if !constraints.IsEmpty(&cons) {
					c.Assert(o.Constraints, gc.DeepEquals, cons)
					hc, err := m.HardwareCharacteristics()
					c.Assert(err, jc.ErrorIsNil)
					c.Assert(*hc, gc.DeepEquals, instance.HardwareCharacteristics{
						Arch:     cons.Arch,
						Mem:      cons.Mem,
						RootDisk: cons.RootDisk,
						CpuCores: cons.CpuCores,
						CpuPower: cons.CpuPower,
						Tags:     cons.Tags,
					})
				}
				return
			default:
				c.Logf("ignoring unexpected operation %#v", o)
			}
		case <-time.After(2 * time.Second):
			c.Fatalf("provisioner did not start an instance")
			return
		}
	}
}

// checkNoOperations checks that the environ was not operated upon.
func (s *CommonProvisionerSuite) checkNoOperations(c *gc.C) {
	s.BackingState.StartSync()
	select {
	case o := <-s.op:
		c.Fatalf("unexpected operation %+v", o)
	case <-time.After(coretesting.ShortWait):
		return
	}
}

// checkStopInstances checks that an instance has been stopped.
func (s *CommonProvisionerSuite) checkStopInstances(c *gc.C, instances ...instance.Instance) {
	s.checkStopSomeInstances(c, instances, nil)
}

// checkStopSomeInstances checks that instancesToStop are stopped while instancesToKeep are not.
func (s *CommonProvisionerSuite) checkStopSomeInstances(c *gc.C,
	instancesToStop []instance.Instance, instancesToKeep []instance.Instance) {

	s.BackingState.StartSync()
	instanceIdsToStop := set.NewStrings()
	for _, instance := range instancesToStop {
		instanceIdsToStop.Add(string(instance.Id()))
	}
	instanceIdsToKeep := set.NewStrings()
	for _, instance := range instancesToKeep {
		instanceIdsToKeep.Add(string(instance.Id()))
	}
	// Continue checking for stop instance calls until all the instances we
	// are waiting on to finish, actually finish, or we time out.
	for !instanceIdsToStop.IsEmpty() {
		select {
		case o := <-s.op:
			switch o := o.(type) {
			case dummy.OpStopInstances:
				for _, id := range o.Ids {
					instId := string(id)
					instanceIdsToStop.Remove(instId)
					if instanceIdsToKeep.Contains(instId) {
						c.Errorf("provisioner unexpectedly stopped instance %s", instId)
					}
				}
			default:
				c.Fatalf("unexpected operation %#v", o)
				return
			}
		case <-time.After(2 * time.Second):
			c.Fatalf("provisioner did not stop an instance")
			return
		}
	}
}

func (s *CommonProvisionerSuite) waitForWatcher(c *gc.C, w state.NotifyWatcher, name string, check func() bool) {
	// TODO(jam): We need to grow a new method on NotifyWatcherC
	// that calls StartSync while waiting for changes, then
	// waitMachine and waitHardwareCharacteristics can use that
	// instead
	defer stop(c, w)
	timeout := time.After(coretesting.LongWait)
	resync := time.After(0)
	for {
		select {
		case <-w.Changes():
			if check() {
				return
			}
		case <-resync:
			resync = time.After(coretesting.ShortWait)
			s.BackingState.StartSync()
		case <-timeout:
			c.Fatalf("%v wait timed out", name)
		}
	}
}

func (s *CommonProvisionerSuite) waitHardwareCharacteristics(c *gc.C, m *state.Machine, check func() bool) {
	w := m.WatchHardwareCharacteristics()
	name := fmt.Sprintf("hardware characteristics for machine %v", m)
	s.waitForWatcher(c, w, name, check)
}

// waitForRemovalMark waits for the supplied machine to be marked for removal.
func (s *CommonProvisionerSuite) waitForRemovalMark(c *gc.C, m *state.Machine) {
	w := s.BackingState.WatchMachineRemovals()
	name := fmt.Sprintf("machine %v marked for removal", m)
	s.waitForWatcher(c, w, name, func() bool {
		removals, err := s.BackingState.AllMachineRemovals()
		c.Assert(err, jc.ErrorIsNil)
		for _, removal := range removals {
			if removal == m.Id() {
				return true
			}
		}
		return false
	})
}

// waitInstanceId waits until the supplied machine has an instance id, then
// asserts it is as expected.
func (s *CommonProvisionerSuite) waitInstanceId(c *gc.C, m *state.Machine, expect instance.Id) {
	s.waitHardwareCharacteristics(c, m, func() bool {
		if actual, err := m.InstanceId(); err == nil {
			c.Assert(actual, gc.Equals, expect)
			return true
		} else if !errors.IsNotProvisioned(err) {
			// We don't expect any errors.
			panic(err)
		}
		c.Logf("machine %v is still unprovisioned", m)
		return false
	})
}

func (s *CommonProvisionerSuite) newEnvironProvisioner(c *gc.C) provisioner.Provisioner {
	machineTag := names.NewMachineTag("0")
	agentConfig := s.AgentConfigForTag(c, machineTag)
	apiState := apiprovisioner.NewState(s.st)
	w, err := provisioner.NewEnvironProvisioner(apiState, agentConfig, s.Environ)
	c.Assert(err, jc.ErrorIsNil)
	return w
}

func (s *CommonProvisionerSuite) addMachine() (*state.Machine, error) {
	return s.addMachineWithConstraints(s.defaultConstraints)
}

func (s *CommonProvisionerSuite) addMachineWithConstraints(cons constraints.Value) (*state.Machine, error) {
	return s.BackingState.AddOneMachine(state.MachineTemplate{
		Series:      series.LatestLts(),
		Jobs:        []state.MachineJob{state.JobHostUnits},
		Constraints: cons,
	})
}

func (s *CommonProvisionerSuite) enableHA(c *gc.C, n int) []*state.Machine {
	changes, err := s.BackingState.EnableHA(n, s.defaultConstraints, series.LatestLts(), nil)
	c.Assert(err, jc.ErrorIsNil)
	added := make([]*state.Machine, len(changes.Added))
	for i, mid := range changes.Added {
		m, err := s.BackingState.Machine(mid)
		c.Assert(err, jc.ErrorIsNil)
		added[i] = m
	}
	return added
}

func (s *ProvisionerSuite) TestProvisionerStartStop(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	stop(c, p)
}

func (s *ProvisionerSuite) TestSimple(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// Check that an instance is provisioned when the machine is created...
	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	instance := s.checkStartInstance(c, m)

	// ...and removed, along with the machine, when the machine is Dead.
	c.Assert(m.EnsureDead(), gc.IsNil)
	s.checkStopInstances(c, instance)
	s.waitForRemovalMark(c, m)
}

func (s *ProvisionerSuite) TestConstraints(c *gc.C) {
	// Create a machine with non-standard constraints.
	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	cons := constraints.MustParse("mem=8G arch=amd64 cores=2 root-disk=10G")
	err = m.SetConstraints(cons)
	c.Assert(err, jc.ErrorIsNil)

	// Start a provisioner and check those constraints are used.
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)
	s.checkStartInstanceCustom(c, m, "pork", cons, nil, nil, nil, nil, nil, true)
}

func (s *ProvisionerSuite) TestPossibleTools(c *gc.C) {

	storageDir := c.MkDir()
	s.PatchValue(&tools.DefaultBaseURL, storageDir)
	stor, err := filestorage.NewFileStorageWriter(storageDir)
	c.Assert(err, jc.ErrorIsNil)

	// Set a current version that does not match the
	// agent-version in the environ config.
	currentVersion := version.MustParseBinary("1.2.3-quantal-arm64")
	s.PatchValue(&arch.HostArch, func() string { return currentVersion.Arch })
	s.PatchValue(&series.MustHostSeries, func() string { return currentVersion.Series })
	s.PatchValue(&jujuversion.Current, currentVersion.Number)

	// Upload some plausible matches, and some that should be filtered out.
	compatibleVersion := version.MustParseBinary("1.2.3-quantal-amd64")
	ignoreVersion1 := version.MustParseBinary("1.2.4-quantal-arm64")
	ignoreVersion2 := version.MustParseBinary("1.2.3-precise-arm64")
	availableVersions := []version.Binary{
		currentVersion, compatibleVersion, ignoreVersion1, ignoreVersion2,
	}
	envtesting.AssertUploadFakeToolsVersions(c, stor, s.cfg.AgentStream(), s.cfg.AgentStream(), availableVersions...)

	// Extract the tools that we expect to actually match.
	expectedList, err := tools.FindTools(s.Environ, -1, -1, s.cfg.AgentStream(), coretools.Filter{
		Number: currentVersion.Number,
		Series: currentVersion.Series,
	})
	c.Assert(err, jc.ErrorIsNil)

	// Create the machine and check the tools that get passed into StartInstance.
	machine, err := s.BackingState.AddOneMachine(state.MachineTemplate{
		Series: "quantal",
		Jobs:   []state.MachineJob{state.JobHostUnits},
	})
	c.Assert(err, jc.ErrorIsNil)

	provisioner := s.newEnvironProvisioner(c)
	defer stop(c, provisioner)
	s.checkStartInstanceCustom(
		c, machine, "pork", constraints.Value{},
		nil, nil, nil, nil, expectedList, true,
	)
}

func (s *ProvisionerSuite) TestProvisionerSetsErrorStatusWhenNoToolsAreAvailable(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// Check that an instance is not provisioned when the machine is created...
	m, err := s.BackingState.AddOneMachine(state.MachineTemplate{
		// We need a valid series that has no tools uploaded
		Series:      "raring",
		Jobs:        []state.MachineJob{state.JobHostUnits},
		Constraints: s.defaultConstraints,
	})
	c.Assert(err, jc.ErrorIsNil)
	s.checkNoOperations(c)

	// Ensure machine error status was set, and the error matches
	agentStatus, instanceStatus := s.waitUntilMachineNotPending(c, m)
	c.Check(agentStatus.Status, gc.Equals, status.Error)
	c.Check(agentStatus.Message, gc.Equals, "no matching agent binaries available")
	c.Check(instanceStatus.Status, gc.Equals, status.ProvisioningError)
	c.Check(instanceStatus.Message, gc.Equals, "no matching agent binaries available")

	// Restart the PA to make sure the machine is skipped again.
	stop(c, p)
	p = s.newEnvironProvisioner(c)
	defer stop(c, p)
	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) waitUntilMachineNotPending(c *gc.C, m *state.Machine) (status.StatusInfo, status.StatusInfo) {
	t0 := time.Now()
	for time.Since(t0) < coretesting.LongWait {
		agentStatusInfo, err := m.Status()
		c.Assert(err, jc.ErrorIsNil)
		if agentStatusInfo.Status == status.Pending {
			time.Sleep(coretesting.ShortWait)
			continue
		}
		instanceStatusInfo, err := m.InstanceStatus()
		c.Assert(err, jc.ErrorIsNil)
		// officially InstanceStatus is only supposed to be Provisioning, but
		// all current Providers have their unknown state as Pending.
		if instanceStatusInfo.Status == status.Provisioning ||
			instanceStatusInfo.Status == status.Pending {
			time.Sleep(coretesting.ShortWait)
			continue
		}
		return agentStatusInfo, instanceStatusInfo
	}
	c.Fatalf("machine %q stayed in pending", m.Id())
	// Satisfy Go, Fatal should be a panic anyway
	return status.StatusInfo{}, status.StatusInfo{}
}

func (s *ProvisionerSuite) TestProvisionerFailedStartInstanceWithInjectedCreationError(c *gc.C) {
	// Set the retry delay to 0, and retry count to 2 to keep tests short
	s.PatchValue(provisioner.RetryStrategyDelay, 0*time.Second)
	s.PatchValue(provisioner.RetryStrategyCount, 2)

	// create the error injection channel
	errorInjectionChannel := make(chan error, 3)

	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// patch the dummy provider error injection channel
	cleanup := dummy.PatchTransientErrorInjectionChannel(errorInjectionChannel)
	defer cleanup()

	retryableError := errors.New("container failed to start and was destroyed")
	destroyError := errors.New("container failed to start and failed to destroy: manual cleanup of containers needed")
	// send the error message three times, because the provisioner will retry twice as patched above.
	errorInjectionChannel <- retryableError
	errorInjectionChannel <- retryableError
	errorInjectionChannel <- destroyError

	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkNoOperations(c)

	agentStatus, instanceStatus := s.waitUntilMachineNotPending(c, m)
	// check that the status matches the error message
	c.Check(agentStatus.Status, gc.Equals, status.Error)
	c.Check(agentStatus.Message, gc.Equals, destroyError.Error())
	c.Check(instanceStatus.Status, gc.Equals, status.ProvisioningError)
	c.Check(instanceStatus.Message, gc.Equals, destroyError.Error())
}

func (s *ProvisionerSuite) TestProvisionerSucceedStartInstanceWithInjectedRetryableCreationError(c *gc.C) {
	// Set the retry delay to 0, and retry count to 2 to keep tests short
	s.PatchValue(provisioner.RetryStrategyDelay, 0*time.Second)
	s.PatchValue(provisioner.RetryStrategyCount, 2)

	// create the error injection channel
	errorInjectionChannel := make(chan error, 1)
	c.Assert(errorInjectionChannel, gc.NotNil)

	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// patch the dummy provider error injection channel
	cleanup := dummy.PatchTransientErrorInjectionChannel(errorInjectionChannel)
	defer cleanup()

	// send the error message once
	// - instance creation should succeed
	retryableError := errors.New("container failed to start and was destroyed")
	errorInjectionChannel <- retryableError

	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m)
}

func (s *ProvisionerSuite) TestProvisionerStopRetryingIfDying(c *gc.C) {
	// Create the error injection channel and inject
	// a retryable error
	errorInjectionChannel := make(chan error, 1)

	p := s.newEnvironProvisioner(c)
	// Don't refer the stop.  We will manually stop and verify the result.

	// patch the dummy provider error injection channel
	cleanup := dummy.PatchTransientErrorInjectionChannel(errorInjectionChannel)
	defer cleanup()

	retryableError := errors.New("container failed to start and was destroyed")
	errorInjectionChannel <- retryableError

	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)

	time.Sleep(coretesting.ShortWait)

	stop(c, p)
	statusInfo, err := m.Status()
	c.Assert(err, jc.ErrorIsNil)
	c.Check(statusInfo.Status, gc.Equals, status.Pending)
	statusInfo, err = m.InstanceStatus()
	c.Assert(err, jc.ErrorIsNil)
	if statusInfo.Status != status.Pending && statusInfo.Status != status.Provisioning {
		c.Errorf("statusInfo.Status was %q not one of %q or %q",
			statusInfo.Status, status.Pending, status.Provisioning)
	}
	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) TestProvisioningDoesNotOccurForLXD(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// create a machine to host the container.
	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	inst := s.checkStartInstance(c, m)

	// make a container on the machine we just created
	template := state.MachineTemplate{
		Series: series.LatestLts(),
		Jobs:   []state.MachineJob{state.JobHostUnits},
	}
	container, err := s.State.AddMachineInsideMachine(template, m.Id(), instance.LXD)
	c.Assert(err, jc.ErrorIsNil)

	// the PA should not attempt to create it
	s.checkNoOperations(c)

	// cleanup
	c.Assert(container.EnsureDead(), gc.IsNil)
	c.Assert(container.Remove(), gc.IsNil)
	c.Assert(m.EnsureDead(), gc.IsNil)
	s.checkStopInstances(c, inst)
	s.waitForRemovalMark(c, m)
}

func (s *ProvisionerSuite) TestProvisioningDoesNotOccurForKVM(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// create a machine to host the container.
	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	inst := s.checkStartInstance(c, m)

	// make a container on the machine we just created
	template := state.MachineTemplate{
		Series: series.LatestLts(),
		Jobs:   []state.MachineJob{state.JobHostUnits},
	}
	container, err := s.State.AddMachineInsideMachine(template, m.Id(), instance.KVM)
	c.Assert(err, jc.ErrorIsNil)

	// the PA should not attempt to create it
	s.checkNoOperations(c)

	// cleanup
	c.Assert(container.EnsureDead(), gc.IsNil)
	c.Assert(container.Remove(), gc.IsNil)
	c.Assert(m.EnsureDead(), gc.IsNil)
	s.checkStopInstances(c, inst)
	s.waitForRemovalMark(c, m)
}

type MachineClassifySuite struct {
}

var _ = gc.Suite(&MachineClassifySuite{})

type MockMachine struct {
	life          params.Life
	status        status.Status
	id            string
	idErr         error
	ensureDeadErr error
	statusErr     error
}

func (m *MockMachine) Life() params.Life {
	return m.life
}

func (m *MockMachine) InstanceId() (instance.Id, error) {
	return instance.Id(m.id), m.idErr
}

func (m *MockMachine) EnsureDead() error {
	return m.ensureDeadErr
}

func (m *MockMachine) Status() (status.Status, string, error) {
	return m.status, "", m.statusErr
}

func (m *MockMachine) InstanceStatus() (status.Status, string, error) {
	return m.status, "", m.statusErr
}

func (m *MockMachine) Id() string {
	return m.id
}

type machineClassificationTest struct {
	description    string
	life           params.Life
	status         status.Status
	idErr          string
	ensureDeadErr  string
	expectErrCode  string
	expectErrFmt   string
	statusErr      string
	classification provisioner.MachineClassification
}

var machineClassificationTests = []machineClassificationTest{{
	description:    "Dead machine is dead",
	life:           params.Dead,
	status:         status.Started,
	classification: provisioner.Dead,
}, {
	description:    "Dying machine can carry on dying",
	life:           params.Dying,
	status:         status.Started,
	classification: provisioner.None,
}, {
	description:    "Dying unprovisioned machine is ensured dead",
	life:           params.Dying,
	status:         status.Started,
	classification: provisioner.Dead,
	idErr:          params.CodeNotProvisioned,
}, {
	description:    "Can't load provisioned dying machine",
	life:           params.Dying,
	status:         status.Started,
	classification: provisioner.None,
	idErr:          params.CodeNotFound,
	expectErrCode:  params.CodeNotFound,
	expectErrFmt:   "failed to load dying machine id:%s.*",
}, {
	description:    "Alive machine is not provisioned - pending",
	life:           params.Alive,
	status:         status.Pending,
	classification: provisioner.Pending,
	idErr:          params.CodeNotProvisioned,
	expectErrFmt:   "found machine pending provisioning id:%s.*",
}, {
	description:    "Alive, pending machine not found",
	life:           params.Alive,
	status:         status.Pending,
	classification: provisioner.None,
	idErr:          params.CodeNotFound,
	expectErrCode:  params.CodeNotFound,
	expectErrFmt:   "failed to load machine id:%s.*",
}, {
	description:    "Cannot get unprovisioned machine status",
	life:           params.Alive,
	classification: provisioner.None,
	statusErr:      params.CodeNotFound,
	idErr:          params.CodeNotProvisioned,
}, {
	description:    "Dying machine fails to ensure dead",
	life:           params.Dying,
	status:         status.Started,
	classification: provisioner.None,
	idErr:          params.CodeNotProvisioned,
	expectErrCode:  params.CodeNotFound,
	ensureDeadErr:  params.CodeNotFound,
	expectErrFmt:   "failed to ensure machine dead id:%s.*",
}}

var machineClassificationTestsRequireMaintenance = machineClassificationTest{
	description:    "Machine needs maintaining",
	life:           params.Alive,
	status:         status.Started,
	classification: provisioner.Maintain,
}

var machineClassificationTestsNoMaintenance = machineClassificationTest{
	description:    "Machine doesn't need maintaining",
	life:           params.Alive,
	status:         status.Started,
	classification: provisioner.None,
}

func (s *MachineClassifySuite) TestMachineClassification(c *gc.C) {
	test := func(t machineClassificationTest, id string) {
		// Run a sub-test from the test table
		s2e := func(s string) error {
			// Little helper to turn a non-empty string into a useful error for "ErrorMaches"
			if s != "" {
				return &params.Error{Code: s}
			}
			return nil
		}

		c.Logf("%s: %s", id, t.description)
		machine := MockMachine{t.life, t.status, id, s2e(t.idErr), s2e(t.ensureDeadErr), s2e(t.statusErr)}
		classification, err := provisioner.ClassifyMachine(&machine)
		if err != nil {
			c.Assert(err, gc.ErrorMatches, fmt.Sprintf(t.expectErrFmt, machine.Id()))
		} else {
			c.Assert(err, gc.Equals, s2e(t.expectErrCode))
		}
		c.Assert(classification, gc.Equals, t.classification)
	}

	machineIds := []string{"0/kvm/0", "0"}
	for _, id := range machineIds {
		tests := machineClassificationTests
		if id == "0" {
			tests = append(tests, machineClassificationTestsNoMaintenance)
		} else {
			tests = append(tests, machineClassificationTestsRequireMaintenance)
		}
		for _, t := range tests {
			test(t, id)
		}
	}
}

func (s *ProvisionerSuite) TestProvisioningMachinesWithSpacesSuccess(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// Add the spaces used in constraints.
	_, err := s.State.AddSpace("space1", "", nil, false)
	c.Assert(err, jc.ErrorIsNil)
	_, err = s.State.AddSpace("space2", "", nil, false)
	c.Assert(err, jc.ErrorIsNil)

	// Add 1 subnet into space1, and 2 into space2.
	// Each subnet is in a matching zone (e.g "subnet-#" in "zone#").
	testing.AddSubnetsWithTemplate(c, s.State, 3, state.SubnetInfo{
		CIDR:             "10.10.{{.}}.0/24",
		ProviderId:       "subnet-{{.}}",
		AvailabilityZone: "zone{{.}}",
		SpaceName:        "{{if (eq . 0)}}space1{{else}}space2{{end}}",
		VLANTag:          42,
	})

	// Add and provision a machine with spaces specified.
	cons := constraints.MustParse(
		s.defaultConstraints.String(), "spaces=space2,^space1",
	)
	// The dummy provider simulates 2 subnets per included space.
	expectedSubnetsToZones := map[network.Id][]string{
		"subnet-0": []string{"zone0"},
		"subnet-1": []string{"zone1"},
	}
	m, err := s.addMachineWithConstraints(cons)
	c.Assert(err, jc.ErrorIsNil)
	inst := s.checkStartInstanceCustom(
		c, m, "pork", cons,
		nil,
		expectedSubnetsToZones,
		nil, nil, nil, true,
	)

	// Cleanup.
	c.Assert(m.EnsureDead(), gc.IsNil)
	s.checkStopInstances(c, inst)
	s.waitForRemovalMark(c, m)
}

func (s *ProvisionerSuite) testProvisioningFailsAndSetsErrorStatusForConstraints(
	c *gc.C,
	cons constraints.Value,
	expectedErrorStatus string,
) {
	machine, err := s.addMachineWithConstraints(cons)
	c.Assert(err, jc.ErrorIsNil)

	// Start the PA.
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// Expect StartInstance to fail.
	s.checkNoOperations(c)

	// Ensure machine error status was set, and the error matches
	agentStatus, instanceStatus := s.waitUntilMachineNotPending(c, machine)
	c.Check(agentStatus.Status, gc.Equals, status.Error)
	c.Check(agentStatus.Message, gc.Equals, expectedErrorStatus)
	c.Check(instanceStatus.Status, gc.Equals, status.ProvisioningError)
	c.Check(instanceStatus.Message, gc.Equals, expectedErrorStatus)

	// Make sure the task didn't stop with an error
	died := make(chan error)
	go func() {
		died <- p.Wait()
	}()
	select {
	case <-time.After(coretesting.ShortWait):
	case err := <-died:
		c.Fatalf("provisioner task died unexpectedly with err: %v", err)
	}

	// Restart the PA to make sure the machine is not retried.
	stop(c, p)
	p = s.newEnvironProvisioner(c)
	defer stop(c, p)

	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) TestProvisioningMachinesFailsWithUnknownSpaces(c *gc.C) {
	cons := constraints.MustParse(
		s.defaultConstraints.String(), "spaces=missing,ignored,^ignored-too",
	)
	expectedErrorStatus := `cannot match subnets to zones: space "missing" not found`
	s.testProvisioningFailsAndSetsErrorStatusForConstraints(c, cons, expectedErrorStatus)
}

func (s *ProvisionerSuite) TestProvisioningMachinesFailsWithEmptySpaces(c *gc.C) {
	_, err := s.State.AddSpace("empty", "", nil, false)
	c.Assert(err, jc.ErrorIsNil)
	cons := constraints.MustParse(
		s.defaultConstraints.String(), "spaces=empty",
	)
	expectedErrorStatus := `cannot match subnets to zones: ` +
		`cannot use space "empty" as deployment target: no subnets`
	s.testProvisioningFailsAndSetsErrorStatusForConstraints(c, cons, expectedErrorStatus)
}

func (s *CommonProvisionerSuite) addMachineWithRequestedVolumes(volumes []state.MachineVolumeParams, cons constraints.Value) (*state.Machine, error) {
	return s.BackingState.AddOneMachine(state.MachineTemplate{
		Series:      series.LatestLts(),
		Jobs:        []state.MachineJob{state.JobHostUnits},
		Constraints: cons,
		Volumes:     volumes,
	})
}

func (s *ProvisionerSuite) TestProvisioningMachinesWithRequestedVolumes(c *gc.C) {
	// Set up a persistent pool.
	poolManager := poolmanager.New(state.NewStateSettings(s.State), s.Environ)
	_, err := poolManager.Create("persistent-pool", "static", map[string]interface{}{"persistent": true})
	c.Assert(err, jc.ErrorIsNil)

	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// Add a machine with volumes to state.
	requestedVolumes := []state.MachineVolumeParams{{
		Volume:     state.VolumeParams{Pool: "static", Size: 1024},
		Attachment: state.VolumeAttachmentParams{},
	}, {
		Volume:     state.VolumeParams{Pool: "persistent-pool", Size: 2048},
		Attachment: state.VolumeAttachmentParams{},
	}, {
		Volume:     state.VolumeParams{Pool: "persistent-pool", Size: 4096},
		Attachment: state.VolumeAttachmentParams{},
	}}
	m, err := s.addMachineWithRequestedVolumes(requestedVolumes, s.defaultConstraints)
	c.Assert(err, jc.ErrorIsNil)

	// Provision volume-2, so that it is attached rather than created.
	err = s.IAASModel.SetVolumeInfo(names.NewVolumeTag("2"), state.VolumeInfo{
		Pool:     "persistent-pool",
		VolumeId: "vol-ume",
		Size:     4096,
	})
	c.Assert(err, jc.ErrorIsNil)

	// Provision the machine, checking the volume and volume attachment arguments.
	expectedVolumes := []storage.Volume{{
		names.NewVolumeTag("0"),
		storage.VolumeInfo{
			Size: 1024,
		},
	}, {
		names.NewVolumeTag("1"),
		storage.VolumeInfo{
			Size:       2048,
			Persistent: true,
		},
	}}
	expectedVolumeAttachments := []storage.VolumeAttachment{{
		Volume:  names.NewVolumeTag("2"),
		Machine: m.MachineTag(),
		VolumeAttachmentInfo: storage.VolumeAttachmentInfo{
			DeviceName: "sdb",
		},
	}}
	inst := s.checkStartInstanceCustom(
		c, m, "pork", s.defaultConstraints,
		nil, nil,
		expectedVolumes,
		expectedVolumeAttachments,
		nil, true,
	)

	// Cleanup.
	c.Assert(m.EnsureDead(), gc.IsNil)
	s.checkStopInstances(c, inst)
	s.waitForRemovalMark(c, m)
}

func (s *ProvisionerSuite) TestProvisioningDoesNotProvisionTheSameMachineAfterRestart(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// create a machine
	m, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m)

	// restart the PA
	stop(c, p)
	p = s.newEnvironProvisioner(c)
	defer stop(c, p)

	// check that there is only one machine provisioned.
	machines, err := s.State.AllMachines()
	c.Assert(err, jc.ErrorIsNil)
	c.Check(len(machines), gc.Equals, 2)
	c.Check(machines[0].Id(), gc.Equals, "0")
	c.Check(machines[1].CheckProvisioned("fake_nonce"), jc.IsFalse)

	// the PA should not create it a second time
	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) TestDyingMachines(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)

	// provision a machine
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m0)

	// stop the provisioner and make the machine dying
	stop(c, p)
	err = m0.Destroy()
	c.Assert(err, jc.ErrorIsNil)

	// add a new, dying, unprovisioned machine
	m1, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	err = m1.Destroy()
	c.Assert(err, jc.ErrorIsNil)

	// start the provisioner and wait for it to reap the useless machine
	p = s.newEnvironProvisioner(c)
	defer stop(c, p)
	s.checkNoOperations(c)
	s.waitForRemovalMark(c, m1)

	// verify the other one's still fine
	err = m0.Refresh()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(m0.Life(), gc.Equals, state.Dying)
}

type mockMachineGetter struct{}

func (*mockMachineGetter) Machine(names.MachineTag) (*apiprovisioner.Machine, error) {
	return nil, fmt.Errorf("error")
}

func (*mockMachineGetter) MachinesWithTransientErrors() ([]*apiprovisioner.Machine, []params.StatusResult, error) {
	return nil, nil, fmt.Errorf("error")
}

func (s *ProvisionerSuite) TestMachineErrorsRetainInstances(c *gc.C) {
	task := s.newProvisionerTask(c, config.HarvestAll, s.Environ, s.provisioner, mockToolsFinder{})
	defer stop(c, task)

	// create a machine
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m0)

	// create an instance out of band
	s.startUnknownInstance(c, "999")

	// start the provisioner and ensure it doesn't kill any instances if there are error getting machines
	task = s.newProvisionerTask(
		c,
		config.HarvestAll,
		s.Environ,
		&mockMachineGetter{},
		&mockToolsFinder{},
	)
	defer func() {
		err := worker.Stop(task)
		c.Assert(err, gc.ErrorMatches, ".*failed to get machine.*")
	}()
	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) TestEnvironProvisionerObservesConfigChanges(c *gc.C) {
	p := s.newEnvironProvisioner(c)
	defer stop(c, p)
	s.assertProvisionerObservesConfigChanges(c, p)
}

func (s *ProvisionerSuite) newProvisionerTask(
	c *gc.C,
	harvestingMethod config.HarvestMode,
	broker environs.InstanceBroker,
	machineGetter provisioner.MachineGetter,
	toolsFinder provisioner.ToolsFinder,
) provisioner.ProvisionerTask {

	machineWatcher, err := s.provisioner.WatchModelMachines()
	c.Assert(err, jc.ErrorIsNil)
	retryWatcher, err := s.provisioner.WatchMachineErrorRetry()
	c.Assert(err, jc.ErrorIsNil)
	auth, err := authentication.NewAPIAuthenticator(s.provisioner)
	c.Assert(err, jc.ErrorIsNil)

	retryStrategy := provisioner.NewRetryStrategy(0*time.Second, 0)

	w, err := provisioner.NewProvisionerTask(
		s.ControllerConfig.ControllerUUID(),
		names.NewMachineTag("0"),
		harvestingMethod,
		machineGetter,
		toolsFinder,
		machineWatcher,
		retryWatcher,
		broker,
		auth,
		imagemetadata.ReleasedStream,
		retryStrategy,
	)
	c.Assert(err, jc.ErrorIsNil)
	return w
}

func (s *ProvisionerSuite) TestHarvestNoneReapsNothing(c *gc.C) {

	task := s.newProvisionerTask(c, config.HarvestDestroyed, s.Environ, s.provisioner, mockToolsFinder{})
	defer stop(c, task)
	task.SetHarvestMode(config.HarvestNone)

	// Create a machine and an unknown instance.
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m0)
	s.startUnknownInstance(c, "999")

	// Mark the first machine as dead.
	c.Assert(m0.EnsureDead(), gc.IsNil)

	// Ensure we're doing nothing.
	s.checkNoOperations(c)
}

func (s *ProvisionerSuite) TestHarvestUnknownReapsOnlyUnknown(c *gc.C) {

	task := s.newProvisionerTask(c,
		config.HarvestDestroyed,
		s.Environ,
		s.provisioner,
		mockToolsFinder{},
	)
	defer stop(c, task)
	task.SetHarvestMode(config.HarvestUnknown)

	// Create a machine and an unknown instance.
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	i0 := s.checkStartInstance(c, m0)
	i1 := s.startUnknownInstance(c, "999")

	// Mark the first machine as dead.
	c.Assert(m0.EnsureDead(), gc.IsNil)

	// When only harvesting unknown machines, only one of the machines
	// is stopped.
	s.checkStopSomeInstances(c, []instance.Instance{i1}, []instance.Instance{i0})
	s.waitForRemovalMark(c, m0)
}

func (s *ProvisionerSuite) TestHarvestDestroyedReapsOnlyDestroyed(c *gc.C) {

	task := s.newProvisionerTask(
		c,
		config.HarvestDestroyed,
		s.Environ,
		s.provisioner,
		mockToolsFinder{},
	)
	defer stop(c, task)

	// Create a machine and an unknown instance.
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	i0 := s.checkStartInstance(c, m0)
	i1 := s.startUnknownInstance(c, "999")

	// Mark the first machine as dead.
	c.Assert(m0.EnsureDead(), gc.IsNil)

	// When only harvesting destroyed machines, only one of the
	// machines is stopped.
	s.checkStopSomeInstances(c, []instance.Instance{i0}, []instance.Instance{i1})
	s.waitForRemovalMark(c, m0)
}

func (s *ProvisionerSuite) TestHarvestAllReapsAllTheThings(c *gc.C) {

	task := s.newProvisionerTask(c,
		config.HarvestDestroyed,
		s.Environ,
		s.provisioner,
		mockToolsFinder{},
	)
	defer stop(c, task)
	task.SetHarvestMode(config.HarvestAll)

	// Create a machine and an unknown instance.
	m0, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	i0 := s.checkStartInstance(c, m0)
	i1 := s.startUnknownInstance(c, "999")

	// Mark the first machine as dead.
	c.Assert(m0.EnsureDead(), gc.IsNil)

	// Everything must die!
	s.checkStopSomeInstances(c, []instance.Instance{i0, i1}, []instance.Instance{})
	s.waitForRemovalMark(c, m0)
}

func (s *ProvisionerSuite) TestProvisionerRetriesTransientErrors(c *gc.C) {
	s.PatchValue(&apiserverprovisioner.ErrorRetryWaitDelay, 5*time.Millisecond)
	e := &mockBroker{Environ: s.Environ, retryCount: make(map[string]int)}
	task := s.newProvisionerTask(c, config.HarvestAll, e, s.provisioner, mockToolsFinder{})
	defer stop(c, task)

	// Provision some machines, some will be started first time,
	// another will require retries.
	m1, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m1)
	m2, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	s.checkStartInstance(c, m2)
	m3, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)
	m4, err := s.addMachine()
	c.Assert(err, jc.ErrorIsNil)

	// mockBroker will fail to start machine-3 several times;
	// keep setting the transient flag to retry until the
	// instance has started.
	thatsAllFolks := make(chan struct{})
	go func() {
		for {
			select {
			case <-thatsAllFolks:
				return
			case <-time.After(coretesting.ShortWait):
				now := time.Now()
				sInfo := status.StatusInfo{
					Status:  status.ProvisioningError,
					Message: "info",
					Data:    map[string]interface{}{"transient": true},
					Since:   &now,
				}
				err := m3.SetInstanceStatus(sInfo)
				c.Assert(err, jc.ErrorIsNil)
			}
		}
	}()
	s.checkStartInstance(c, m3)
	close(thatsAllFolks)

	// Machine 4 is never provisioned.
	statusInfo, err := m4.InstanceStatus()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(statusInfo.Status, gc.Equals, status.ProvisioningError)
	_, err = m4.InstanceId()
	c.Assert(err, jc.Satisfies, errors.IsNotProvisioned)
}

func (s *ProvisionerSuite) TestProvisionerObservesMachineJobs(c *gc.C) {
	s.PatchValue(&apiserverprovisioner.ErrorRetryWaitDelay, 5*time.Millisecond)
	broker := &mockBroker{Environ: s.Environ, retryCount: make(map[string]int)}
	task := s.newProvisionerTask(c, config.HarvestAll, broker, s.provisioner, mockToolsFinder{})
	defer stop(c, task)

	added := s.enableHA(c, 3)
	c.Assert(added, gc.HasLen, 2)
	byId := make(map[string]*state.Machine)
	for _, m := range added {
		byId[m.Id()] = m
	}
	for _, id := range broker.ids {
		s.checkStartInstance(c, byId[id])
	}
}

type mockBroker struct {
	environs.Environ
	retryCount map[string]int
	ids        []string
}

func (b *mockBroker) StartInstance(args environs.StartInstanceParams) (*environs.StartInstanceResult, error) {
	// All machines except machines 3, 4 are provisioned successfully the first time.
	// Machines 3 is provisioned after some attempts have been made.
	// Machine 4 is never provisioned.
	id := args.InstanceConfig.MachineId
	// record ids so we can call checkStartInstance in the appropriate order.
	b.ids = append(b.ids, id)
	retries := b.retryCount[id]
	if (id != "3" && id != "4") || retries > 2 {
		return b.Environ.StartInstance(args)
	} else {
		b.retryCount[id] = retries + 1
	}
	return nil, fmt.Errorf("error: some error")
}

type mockToolsFinder struct {
}

func (f mockToolsFinder) FindTools(number version.Number, series string, a string) (coretools.List, error) {
	v, err := version.ParseBinary(fmt.Sprintf("%s-%s-%s", number, series, arch.HostArch()))
	if err != nil {
		return nil, err
	}
	if a != "" {
		v.Arch = a
	}
	return coretools.List{&coretools.Tools{Version: v}}, nil
}

type mockAgent struct {
	agent.Agent
	config agent.Config
}

func (mock mockAgent) CurrentConfig() agent.Config {
	return mock.config
}
