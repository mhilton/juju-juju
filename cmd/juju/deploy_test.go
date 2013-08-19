// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	gc "launchpad.net/gocheck"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/errors"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/state"
	coretesting "launchpad.net/juju-core/testing"
	"launchpad.net/juju-core/testing/checkers"
)

type DeploySuite struct {
	testing.RepoSuite
}

var _ = gc.Suite(&DeploySuite{})

func runDeploy(c *gc.C, args ...string) error {
	_, err := coretesting.RunCommand(c, &DeployCommand{}, args)
	return err
}

var initErrorTests = []struct {
	args []string
	err  string
}{
	{
		args: nil,
		err:  `no charm specified`,
	}, {
		args: []string{"craz~ness"},
		err:  `invalid charm name "craz~ness"`,
	}, {
		args: []string{"craziness", "burble-1"},
		err:  `invalid service name "burble-1"`,
	}, {
		args: []string{"craziness", "burble1", "-n", "0"},
		err:  `--num-units must be a positive integer`,
	}, {
		args: []string{"craziness", "burble1", "--to", "bigglesplop"},
		err:  `invalid --to parameter "bigglesplop"`,
	}, {
		args: []string{"craziness", "burble1", "-n", "2", "--to", "123"},
		err:  `cannot use --num-units > 1 with --to`,
	}, {
		args: []string{"craziness", "burble1", "--constraints", "gibber=plop"},
		err:  `invalid value "gibber=plop" for flag --constraints: unknown constraint "gibber"`,
	},
}

func (s *DeploySuite) TestInitErrors(c *gc.C) {
	for i, t := range initErrorTests {
		c.Logf("test %d", i)
		err := coretesting.InitCommand(&DeployCommand{}, t.args)
		c.Assert(err, gc.ErrorMatches, t.err)
	}
}

func (s *DeploySuite) TestNoCharm(c *gc.C) {
	err := runDeploy(c, "local:unknown-123")
	c.Assert(err, gc.ErrorMatches, `cannot get charm: charm not found in ".*": local:precise/unknown-123`)
}

func (s *DeploySuite) TestCharmDir(c *gc.C) {
	coretesting.Charms.ClonedDirPath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/dummy-1")
	s.AssertService(c, "dummy", curl, 1, 0)
}

func (s *DeploySuite) TestUpgradeCharmDir(c *gc.C) {
	dirPath := coretesting.Charms.ClonedDirPath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy", "-u")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/dummy-2")
	s.AssertService(c, "dummy", curl, 1, 0)
	// Check the charm really was upgraded.
	ch, err := charm.ReadDir(dirPath)
	c.Assert(err, gc.IsNil)
	c.Assert(ch.Revision(), gc.Equals, 2)
}

func (s *DeploySuite) TestCharmBundle(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy", "some-service-name")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/dummy-1")
	s.AssertService(c, "some-service-name", curl, 1, 0)
}

func (s *DeploySuite) TestCannotUpgradeCharmBundle(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy", "-u")
	c.Assert(err, gc.ErrorMatches, `cannot increment revision of charm "local:precise/dummy-1": not a directory`)
	// Verify state not touched...
	curl := charm.MustParseURL("local:precise/dummy-1")
	_, err = s.State.Charm(curl)
	c.Assert(err, gc.ErrorMatches, `charm "local:precise/dummy-1" not found`)
	_, err = s.State.Service("dummy")
	c.Assert(err, gc.ErrorMatches, `service "dummy" not found`)
}

func (s *DeploySuite) TestSubordinateCharm(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "logging")
	err := runDeploy(c, "local:logging")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/logging-1")
	s.AssertService(c, "logging", curl, 0, 0)
}

func (s *DeploySuite) TestConfig(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	path := setupConfigfile(c, c.MkDir())
	err := runDeploy(c, "local:dummy", "dummy-service", "--config", path)
	c.Assert(err, gc.IsNil)
	service, err := s.State.Service("dummy-service")
	c.Assert(err, gc.IsNil)
	settings, err := service.ConfigSettings()
	c.Assert(err, gc.IsNil)
	c.Assert(settings, gc.DeepEquals, charm.Settings{
		"skill-level": int64(9000),
		"username":    "admin001",
	})
}

func (s *DeploySuite) TestConfigError(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	path := setupConfigfile(c, c.MkDir())
	err := runDeploy(c, "local:dummy", "other-service", "--config", path)
	c.Assert(err, gc.ErrorMatches, `no settings found for "other-service"`)
	_, err = s.State.Service("other-service")
	c.Assert(err, checkers.Satisfies, errors.IsNotFoundError)
}

func (s *DeploySuite) TestConstraints(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy", "--constraints", "mem=2G cpu-cores=2")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/dummy-1")
	service, _ := s.AssertService(c, "dummy", curl, 1, 0)
	cons, err := service.Constraints()
	c.Assert(err, gc.IsNil)
	c.Assert(cons, gc.DeepEquals, constraints.MustParse("mem=2G cpu-cores=2"))
}

func (s *DeploySuite) TestSubordinateConstraints(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "logging")
	err := runDeploy(c, "local:logging", "--constraints", "mem=1G")
	c.Assert(err, gc.ErrorMatches, "cannot use --constraints with subordinate service")
}

func (s *DeploySuite) TestNumUnits(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	err := runDeploy(c, "local:dummy", "-n", "13")
	c.Assert(err, gc.IsNil)
	curl := charm.MustParseURL("local:precise/dummy-1")
	s.AssertService(c, "dummy", curl, 13, 0)
}

func (s *DeploySuite) TestNumUnitsSubordinate(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "logging")
	err := runDeploy(c, "--num-units", "3", "local:logging")
	c.Assert(err, gc.ErrorMatches, "cannot use --num-units or --to with subordinate service")
	_, err = s.State.Service("dummy")
	c.Assert(err, gc.ErrorMatches, `service "dummy" not found`)
}

func (s *DeploySuite) assertForceMachine(c *gc.C, machineId string) {
	svc, err := s.State.Service("portlandia")
	c.Assert(err, gc.IsNil)
	units, err := svc.AllUnits()
	c.Assert(err, gc.IsNil)
	c.Assert(units, gc.HasLen, 1)
	mid, err := units[0].AssignedMachineId()
	c.Assert(err, gc.IsNil)
	c.Assert(mid, gc.Equals, machineId)
}

func (s *DeploySuite) TestForceMachine(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	machine, err := s.State.AddMachine("precise", state.JobHostUnits)
	c.Assert(err, gc.IsNil)
	err = runDeploy(c, "--to", machine.Id(), "local:dummy", "portlandia")
	c.Assert(err, gc.IsNil)
	s.assertForceMachine(c, machine.Id())
}

func (s *DeploySuite) TestForceMachineExistingContainer(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	params := &state.AddMachineParams{
		Series:        "precise",
		ContainerType: instance.LXC,
		Jobs:          []state.MachineJob{state.JobHostUnits},
	}
	container, err := s.State.AddMachineWithConstraints(params)
	c.Assert(err, gc.IsNil)
	err = runDeploy(c, "--to", container.Id(), "local:dummy", "portlandia")
	c.Assert(err, gc.IsNil)
	s.assertForceMachine(c, container.Id())
	machines, err := s.State.AllMachines()
	c.Assert(err, gc.IsNil)
	c.Assert(machines, gc.HasLen, 2)
}

func (s *DeploySuite) TestForceMachineNewContainer(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	machine, err := s.State.AddMachine("precise", state.JobHostUnits)
	c.Assert(err, gc.IsNil)
	err = runDeploy(c, "--to", "lxc:"+machine.Id(), "local:dummy", "portlandia")
	c.Assert(err, gc.IsNil)
	s.assertForceMachine(c, machine.Id()+"/lxc/0")
	machines, err := s.State.AllMachines()
	c.Assert(err, gc.IsNil)
	c.Assert(machines, gc.HasLen, 2)
}

func (s *DeploySuite) TestForceMachineNotFound(c *gc.C) {
	coretesting.Charms.BundlePath(s.SeriesPath, "dummy")
	err := runDeploy(c, "--to", "42", "local:dummy", "portlandia")
	c.Assert(err, gc.ErrorMatches, `cannot assign unit "portlandia/0" to machine: machine 42 not found`)
	_, err = s.State.Service("dummy")
	c.Assert(err, gc.ErrorMatches, `service "dummy" not found`)
}

func (s *DeploySuite) TestForceMachineSubordinate(c *gc.C) {
	machine, err := s.State.AddMachine("precise", state.JobHostUnits)
	c.Assert(err, gc.IsNil)
	coretesting.Charms.BundlePath(s.SeriesPath, "logging")
	err = runDeploy(c, "--to", machine.Id(), "local:logging")
	c.Assert(err, gc.ErrorMatches, "cannot use --num-units or --to with subordinate service")
	_, err = s.State.Service("dummy")
	c.Assert(err, gc.ErrorMatches, `service "dummy" not found`)
}
