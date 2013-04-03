package state_test

import (
	"labix.org/v2/mgo"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/testing"
	stdtesting "testing"
)

// TestPackage integrates the tests into gotest.
func TestPackage(t *stdtesting.T) {
	testing.MgoTestPackage(t)
}

// ConnSuite provides the infrastructure for all other
// test suites (StateSuite, CharmSuite, MachineSuite, etc).
type ConnSuite struct {
	testing.MgoSuite
	testing.LoggingSuite
	annotations *mgo.Collection
	charms      *mgo.Collection
	machines    *mgo.Collection
	relations   *mgo.Collection
	services    *mgo.Collection
	units       *mgo.Collection
	State       *state.State
}

func (cs *ConnSuite) SetUpSuite(c *C) {
	cs.LoggingSuite.SetUpSuite(c)
	cs.MgoSuite.SetUpSuite(c)
}

func (cs *ConnSuite) TearDownSuite(c *C) {
	cs.MgoSuite.TearDownSuite(c)
	cs.LoggingSuite.TearDownSuite(c)
}

func (cs *ConnSuite) SetUpTest(c *C) {
	cs.LoggingSuite.SetUpTest(c)
	cs.MgoSuite.SetUpTest(c)
	var err error
	cs.State, err = state.Open(state.TestingStateInfo(), state.TestingDialOpts())
	c.Assert(err, IsNil)

	state.TestingInitialize(c, nil)
	cs.annotations = cs.MgoSuite.Session.DB("juju").C("annotations")
	cs.charms = cs.MgoSuite.Session.DB("juju").C("charms")
	cs.machines = cs.MgoSuite.Session.DB("juju").C("machines")
	cs.relations = cs.MgoSuite.Session.DB("juju").C("relations")
	cs.services = cs.MgoSuite.Session.DB("juju").C("services")
	cs.units = cs.MgoSuite.Session.DB("juju").C("units")
}

func (cs *ConnSuite) TearDownTest(c *C) {
	cs.State.Close()
	cs.MgoSuite.TearDownTest(c)
	cs.LoggingSuite.TearDownTest(c)
}

func (s *ConnSuite) AddTestingCharm(c *C, name string) *state.Charm {
	return state.AddTestingCharm(c, s.State, name)
}

func (s *ConnSuite) AddSeriesCharm(c *C, name, series string) *state.Charm {
	return state.AddCustomCharm(c, s.State, name, "", "", series, -1)
}

// AddConfigCharm clones a testing charm, replaces its config with
// the given YAML string and adds it to the state, using the given
// revision.
func (s *ConnSuite) AddConfigCharm(c *C, name, configYaml string, revision int) *state.Charm {
	return state.AddCustomCharm(c, s.State, name, "config.yaml", configYaml, "series", revision)
}

// AddMetaCharm clones a testing charm, replaces its metadata with the
// given YAM: string and adds it to the state, using the given revision.
func (s *ConnSuite) AddMetaCharm(c *C, name, metaYaml string, revsion int) *state.Charm {
	return state.AddCustomCharm(c, s.State, name, "metadata.yaml", metaYaml, "series", revsion)
}
