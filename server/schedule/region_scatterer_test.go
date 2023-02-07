package schedule

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/tikv/pd/pkg/mock/mockcluster"
	"github.com/tikv/pd/server/config"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/hbstream"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/placement"
	"github.com/tikv/pd/server/versioninfo"
)

type sequencer struct {
	minID uint64
	maxID uint64
	curID uint64
}

func newSequencer(maxID uint64) *sequencer {
	return newSequencerWithMinID(1, maxID)
}

func newSequencerWithMinID(minID, maxID uint64) *sequencer {
	return &sequencer{
		minID: minID,
		maxID: maxID,
		curID: maxID,
	}
}

func (s *sequencer) next() uint64 {
	s.curID++
	if s.curID > s.maxID {
		s.curID = s.minID
	}
	return s.curID
}

var _ = Suite(&testScatterRegionSuite{})

type testScatterRegionSuite struct{}

func (s *testScatterRegionSuite) TestSixStores(c *C) {
	s.scatter(c, 6, 100, false)
	s.scatter(c, 6, 100, true)
	s.scatter(c, 6, 1000, false)
	s.scatter(c, 6, 1000, true)
}

func (s *testScatterRegionSuite) TestFiveStores(c *C) {
	s.scatter(c, 5, 100, false)
	s.scatter(c, 5, 100, true)
	s.scatter(c, 5, 1000, false)
	s.scatter(c, 5, 1000, true)
}

func (s *testScatterRegionSuite) TestSixSpecialStores(c *C) {
	s.scatterSpecial(c, 3, 6, 100)
	s.scatterSpecial(c, 3, 6, 1000)
}

func (s *testScatterRegionSuite) TestFiveSpecialStores(c *C) {
	s.scatterSpecial(c, 5, 5, 100)
	s.scatterSpecial(c, 5, 5, 1000)
}

func (s *testScatterRegionSuite) checkOperator(op *operator.Operator, c *C) {
	for i := 0; i < op.Len(); i++ {
		if rp, ok := op.Step(i).(operator.RemovePeer); ok {
			for j := i + 1; j < op.Len(); j++ {
				if tr, ok := op.Step(j).(operator.TransferLeader); ok {
					c.Assert(rp.FromStore, Not(Equals), tr.FromStore)
					c.Assert(rp.FromStore, Not(Equals), tr.ToStore)
				}
			}
		}
	}
}

func (s *testScatterRegionSuite) scatter(c *C, numStores, numRegions uint64, useRules bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	tc.DisableFeature(versioninfo.JointConsensus)

	// Add ordinary stores.
	for i := uint64(1); i <= numStores; i++ {
		tc.AddRegionStore(i, 0)
	}
	tc.SetEnablePlacementRules(useRules)

	for i := uint64(1); i <= numRegions; i++ {
		// region distributed in same stores.
		tc.AddLeaderRegion(i, 1, 2, 3)
	}
	scatterer := NewRegionScatterer(ctx, tc)

	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		if op, _ := scatterer.Scatter(region, ""); op != nil {
			s.checkOperator(op, c)
			ApplyOperator(tc, op)
		}
	}

	countPeers := make(map[uint64]uint64)
	countLeader := make(map[uint64]uint64)
	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		leaderStoreID := region.GetLeader().GetStoreId()
		for _, peer := range region.GetPeers() {
			countPeers[peer.GetStoreId()]++
			if peer.GetStoreId() == leaderStoreID {
				countLeader[peer.GetStoreId()]++
			}
		}
	}

	//Each store should have the same number of peers.
	for _, count := range countPeers {
		c.Assert(float64(count), LessEqual, 1.1*float64(numRegions*3)/float64(numStores))
		c.Assert(float64(count), GreaterEqual, 0.9*float64(numRegions*3)/float64(numStores))
	}

	// Each store should have the same number of leaders.
	c.Assert(len(countPeers), Equals, int(numStores))
	c.Assert(len(countLeader), Equals, int(numStores))
	for _, count := range countLeader {
		c.Assert(float64(count), LessEqual, 1.1*float64(numRegions)/float64(numStores))
		c.Assert(float64(count), GreaterEqual, 0.9*float64(numRegions)/float64(numStores))
	}
}

func (s *testScatterRegionSuite) scatterSpecial(c *C, numOrdinaryStores, numSpecialStores, numRegions uint64) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	tc.DisableFeature(versioninfo.JointConsensus)

	// Add ordinary stores.
	for i := uint64(1); i <= numOrdinaryStores; i++ {
		tc.AddRegionStore(i, 0)
	}
	// Add special stores.
	for i := uint64(1); i <= numSpecialStores; i++ {
		tc.AddLabelsStore(numOrdinaryStores+i, 0, map[string]string{"engine": "tiflash"})
	}
	tc.SetEnablePlacementRules(true)
	c.Assert(tc.RuleManager.SetRule(&placement.Rule{
		GroupID: "pd", ID: "learner", Role: placement.Learner, Count: 3,
		LabelConstraints: []placement.LabelConstraint{{Key: "engine", Op: placement.In, Values: []string{"tiflash"}}}}), IsNil)

	// Region 1 has the same distribution with the Region 2, which is used to test selectPeerToReplace.
	tc.AddRegionWithLearner(1, 1, []uint64{2, 3}, []uint64{numOrdinaryStores + 1, numOrdinaryStores + 2, numOrdinaryStores + 3})
	for i := uint64(2); i <= numRegions; i++ {
		tc.AddRegionWithLearner(
			i,
			1,
			[]uint64{2, 3},
			[]uint64{numOrdinaryStores + 1, numOrdinaryStores + 2, numOrdinaryStores + 3},
		)
	}
	scatterer := NewRegionScatterer(ctx, tc)

	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		if op, _ := scatterer.Scatter(region, ""); op != nil {
			s.checkOperator(op, c)
			ApplyOperator(tc, op)
		}
	}

	countOrdinaryPeers := make(map[uint64]uint64)
	countSpecialPeers := make(map[uint64]uint64)
	countOrdinaryLeaders := make(map[uint64]uint64)
	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		leaderStoreID := region.GetLeader().GetStoreId()
		for _, peer := range region.GetPeers() {
			storeID := peer.GetStoreId()
			store := tc.Stores.GetStore(storeID)
			if store.GetLabelValue("engine") == "tiflash" {
				countSpecialPeers[storeID]++
			} else {
				countOrdinaryPeers[storeID]++
			}
			if peer.GetStoreId() == leaderStoreID {
				countOrdinaryLeaders[storeID]++
			}
		}
	}

	// Each store should have the same number of peers.
	for _, count := range countOrdinaryPeers {
		c.Assert(float64(count), LessEqual, 1.1*float64(numRegions*3)/float64(numOrdinaryStores))
		c.Assert(float64(count), GreaterEqual, 0.9*float64(numRegions*3)/float64(numOrdinaryStores))
	}
	for _, count := range countSpecialPeers {
		c.Assert(float64(count), LessEqual, 1.1*float64(numRegions*3)/float64(numSpecialStores))
		c.Assert(float64(count), GreaterEqual, 0.9*float64(numRegions*3)/float64(numSpecialStores))
	}
	for _, count := range countOrdinaryLeaders {
		c.Assert(float64(count), LessEqual, 1.1*float64(numRegions)/float64(numOrdinaryStores))
		c.Assert(float64(count), GreaterEqual, 0.9*float64(numRegions)/float64(numOrdinaryStores))
	}
}

func (s *testScatterRegionSuite) TestStoreLimit(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	stream := hbstream.NewTestHeartbeatStreams(ctx, tc.ID, tc, false)
	oc := NewOperatorController(ctx, tc, stream)

	// Add stores 1~6.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
	}

	// Add regions 1~4.
	seq := newSequencer(3)
	// Region 1 has the same distribution with the Region 2, which is used to test selectPeerToReplace.
	tc.AddLeaderRegion(1, 1, 2, 3)
	for i := uint64(2); i <= 5; i++ {
		tc.AddLeaderRegion(i, seq.next(), seq.next(), seq.next())
	}

	scatterer := NewRegionScatterer(ctx, tc)

	for i := uint64(1); i <= 5; i++ {
		region := tc.GetRegion(i)
		if op, _ := scatterer.Scatter(region, ""); op != nil {
			c.Assert(oc.AddWaitingOperator(op), Equals, 1)
		}
	}
}

func (s *testScatterRegionSuite) TestScatterCheck(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	// Add 5 stores.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
	}
	testcases := []struct {
		name        string
		checkRegion *core.RegionInfo
		needFix     bool
	}{
		{
			name:        "region with 4 replicas",
			checkRegion: tc.AddLeaderRegion(1, 1, 2, 3, 4),
			needFix:     true,
		},
		{
			name:        "region with 3 replicas",
			checkRegion: tc.AddLeaderRegion(1, 1, 2, 3),
			needFix:     false,
		},
		{
			name:        "region with 2 replicas",
			checkRegion: tc.AddLeaderRegion(1, 1, 2),
			needFix:     true,
		},
	}
	for _, testcase := range testcases {
		c.Logf(testcase.name)
		scatterer := NewRegionScatterer(ctx, tc)
		_, err := scatterer.Scatter(testcase.checkRegion, "")
		if testcase.needFix {
			c.Assert(err, NotNil)
			c.Assert(tc.CheckRegionUnderSuspect(1), Equals, true)
		} else {
			c.Assert(err, IsNil)
			c.Assert(tc.CheckRegionUnderSuspect(1), Equals, false)
		}
		tc.ResetSuspectRegions()
	}
}

func (s *testScatterRegionSuite) TestScatterGroupInConcurrency(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	// Add 5 stores.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
	}

	testcases := []struct {
		name       string
		groupCount int
	}{
		{
			name:       "1 group",
			groupCount: 1,
		},
		{
			name:       "2 group",
			groupCount: 2,
		},
		{
			name:       "3 group",
			groupCount: 3,
		},
	}

	// We send scatter interweave request for each group to simulate scattering multiple region groups in concurrency.
	for _, testcase := range testcases {
		c.Logf(testcase.name)
		scatterer := NewRegionScatterer(ctx, tc)
		regionID := 1
		for i := 0; i < 100; i++ {
			for j := 0; j < testcase.groupCount; j++ {
				scatterer.scatterRegion(tc.AddLeaderRegion(uint64(regionID), 1, 2, 3),
					fmt.Sprintf("group-%v", j))
				regionID++
			}
		}

		checker := func(ss *selectedStores, expected uint64, delta float64) {
			for i := 0; i < testcase.groupCount; i++ {
				// comparing the leader distribution
				group := fmt.Sprintf("group-%v", i)
				max := uint64(0)
				min := uint64(math.MaxUint64)
				groupDistribution, _ := ss.groupDistribution.Get(group)
				for _, count := range groupDistribution.(map[uint64]uint64) {
					if count > max {
						max = count
					}
					if count < min {
						min = count
					}
				}
				c.Assert(math.Abs(float64(max)-float64(expected)), LessEqual, delta)
				c.Assert(math.Abs(float64(min)-float64(expected)), LessEqual, delta)
			}
		}
		// For leader, we expect each store have about 20 leader for each group
		checker(scatterer.ordinaryEngine.selectedLeader, 20, 5)
		// For peer, we expect each store have about 60 peers for each group
		checker(scatterer.ordinaryEngine.selectedPeer, 60, 15)
	}
}

func (s *testScatterRegionSuite) TestScattersGroup(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	// Add 5 stores.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
	}
	testcases := []struct {
		name    string
		failure bool
	}{
		{
			name:    "have failure",
			failure: true,
		},
		{
			name:    "no failure",
			failure: false,
		},
	}
	group := "group"
	for _, testcase := range testcases {
		scatterer := NewRegionScatterer(ctx, tc)
		regions := map[uint64]*core.RegionInfo{}
		for i := 1; i <= 100; i++ {
			regions[uint64(i)] = tc.AddLeaderRegion(uint64(i), 1, 2, 3)
		}
		c.Log(testcase.name)
		failures := map[uint64]error{}
		if testcase.failure {
			c.Assert(failpoint.Enable("github.com/tikv/pd/server/schedule/scatterFail", `return(true)`), IsNil)
		}

		scatterer.ScatterRegions(regions, failures, group, 3)
		max := uint64(0)
		min := uint64(math.MaxUint64)
		groupDistribution, exist := scatterer.ordinaryEngine.selectedLeader.GetGroupDistribution(group)
		c.Assert(exist, Equals, true)
		for _, count := range groupDistribution {
			if count > max {
				max = count
			}
			if count < min {
				min = count
			}
		}
		// 100 regions divided 5 stores, each store expected to have about 20 regions.
		c.Assert(min, LessEqual, uint64(20))
		c.Assert(max, GreaterEqual, uint64(20))
		c.Assert(max-min, LessEqual, uint64(3))
		if testcase.failure {
			c.Assert(len(failures), Equals, 1)
			_, ok := failures[1]
			c.Assert(ok, Equals, true)
			c.Assert(failpoint.Disable("github.com/tikv/pd/server/schedule/scatterFail"), IsNil)
		} else {
			c.Assert(len(failures), Equals, 0)
		}
	}
}

func (s *testScatterRegionSuite) TestSelectedStoreGC(c *C) {
	// use a shorter gcTTL and gcInterval during the test
	gcInterval = time.Second
	gcTTL = time.Second * 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stores := newSelectedStores(ctx)
	stores.Put(1, "testgroup")
	_, ok := stores.GetGroupDistribution("testgroup")
	c.Assert(ok, Equals, true)
	_, ok = stores.GetGroupDistribution("testgroup")
	c.Assert(ok, Equals, true)
	time.Sleep(gcTTL)
	_, ok = stores.GetGroupDistribution("testgroup")
	c.Assert(ok, Equals, false)
	_, ok = stores.GetGroupDistribution("testgroup")
	c.Assert(ok, Equals, false)
}

// TestRegionFromDifferentGroups test the multi regions. each region have its own group.
// After scatter, the distribution for the whole cluster should be well.
func (s *testScatterRegionSuite) TestRegionFromDifferentGroups(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	// Add 6 stores.
	storeCount := 6
	for i := uint64(1); i <= uint64(storeCount); i++ {
		tc.AddRegionStore(i, 0)
	}
	scatterer := NewRegionScatterer(ctx, tc)
	regionCount := 50
	for i := 1; i <= regionCount; i++ {
		p := rand.Perm(storeCount)
		scatterer.scatterRegion(tc.AddLeaderRegion(uint64(i), uint64(p[0])+1, uint64(p[1])+1, uint64(p[2])+1), fmt.Sprintf("t%d", i))
	}
	check := func(ss *selectedStores) {
		max := uint64(0)
		min := uint64(math.MaxUint64)
		for i := uint64(1); i <= uint64(storeCount); i++ {
			count := ss.TotalCountByStore(i)
			if count > max {
				max = count
			}
			if count < min {
				min = count
			}
		}
		c.Assert(max-min, LessEqual, uint64(2))
	}
	check(scatterer.ordinaryEngine.selectedPeer)
}

<<<<<<< HEAD
// TestSelectedStores tests if the peer count has changed due to the picking strategy.
// Ref https://github.com/tikv/pd/issues/4565
func (s *testScatterRegionSuite) TestSelectedStores(c *C) {
=======
func TestRegionHasLearner(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	stream := hbstream.NewTestHeartbeatStreams(ctx, tc.ID, tc, false)
	oc := NewOperatorController(ctx, tc, stream)
	// Add 8 stores.
	voterCount := uint64(6)
	storeCount := uint64(8)
	for i := uint64(1); i <= voterCount; i++ {
		tc.AddLabelsStore(i, 0, map[string]string{"zone": "z1"})
	}
	for i := voterCount + 1; i <= 8; i++ {
		tc.AddLabelsStore(i, 0, map[string]string{"zone": "z2"})
	}
	tc.RuleManager.SetRule(&placement.Rule{
		GroupID: "pd",
		ID:      "default",
		Role:    placement.Voter,
		Count:   3,
		LabelConstraints: []placement.LabelConstraint{
			{
				Key:    "zone",
				Op:     placement.In,
				Values: []string{"z1"},
			},
		},
	})
	tc.RuleManager.SetRule(&placement.Rule{
		GroupID: "pd",
		ID:      "learner",
		Role:    placement.Learner,
		Count:   1,
		LabelConstraints: []placement.LabelConstraint{
			{
				Key:    "zone",
				Op:     placement.In,
				Values: []string{"z2"},
			},
		},
	})
	scatterer := NewRegionScatterer(ctx, tc, oc)
	regionCount := 50
	for i := 1; i <= regionCount; i++ {
		_, err := scatterer.Scatter(tc.AddRegionWithLearner(uint64(i), uint64(1), []uint64{uint64(2), uint64(3)}, []uint64{7}), "group")
		re.NoError(err)
	}
	check := func(ss *selectedStores) {
		max := uint64(0)
		min := uint64(math.MaxUint64)
		for i := uint64(1); i <= max; i++ {
			count := ss.TotalCountByStore(i)
			if count > max {
				max = count
			}
			if count < min {
				min = count
			}
		}
		re.LessOrEqual(max-min, uint64(2))
	}
	check(scatterer.ordinaryEngine.selectedPeer)
	checkLeader := func(ss *selectedStores) {
		max := uint64(0)
		min := uint64(math.MaxUint64)
		for i := uint64(1); i <= voterCount; i++ {
			count := ss.TotalCountByStore(i)
			if count > max {
				max = count
			}
			if count < min {
				min = count
			}
		}
		re.LessOrEqual(max-2, uint64(regionCount)/voterCount)
		re.LessOrEqual(min-1, uint64(regionCount)/voterCount)
		for i := voterCount + 1; i <= storeCount; i++ {
			count := ss.TotalCountByStore(i)
			re.LessOrEqual(count, uint64(0))
		}
	}
	checkLeader(scatterer.ordinaryEngine.selectedLeader)
}

// TestSelectedStoresTooFewPeers tests if the peer count has changed due to the picking strategy.
// Ref https://github.com/tikv/pd/issues/4565
func TestSelectedStoresTooFewPeers(t *testing.T) {
	re := require.New(t)
>>>>>>> f5b5391c0 (region_scatterer: fix the bug that could generate schedule with too many peers (#5920))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	// Add 4 stores.
	for i := uint64(1); i <= 4; i++ {
		tc.AddRegionStore(i, 0)
		// prevent store from being disconnected
		tc.SetStoreLastHeartbeatInterval(i, -10*time.Minute)
	}
	group := "group"
	scatterer := NewRegionScatterer(ctx, tc)

	// Put a lot of regions in Store 1/2/3.
	for i := uint64(1); i < 100; i++ {
		region := tc.AddLeaderRegion(i+10, i%3+1, (i+1)%3+1, (i+2)%3+1)
		peers := make(map[uint64]*metapb.Peer, 3)
		for _, peer := range region.GetPeers() {
			peers[peer.GetStoreId()] = peer
		}
		scatterer.Put(peers, i%3+1, group)
	}

	// Try to scatter a region with peer store id 2/3/4
	for i := uint64(1); i < 20; i++ {
		region := tc.AddLeaderRegion(i+200, i%3+2, (i+1)%3+2, (i+2)%3+2)
		op := scatterer.scatterRegion(region, group)
		c.Assert(isPeerCountChanged(op), IsFalse)
	}
}

// TestSelectedStoresTooManyPeers tests if the peer count has changed due to the picking strategy.
// Ref https://github.com/tikv/pd/issues/5909
func TestSelectedStoresTooManyPeers(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := config.NewTestOptions()
	tc := mockcluster.NewCluster(ctx, opt)
	stream := hbstream.NewTestHeartbeatStreams(ctx, tc.ID, tc, false)
	oc := NewOperatorController(ctx, tc, stream)
	// Add 4 stores.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
		// prevent store from being disconnected
		tc.SetStoreLastHeartbeatInterval(i, -10*time.Minute)
	}
	group := "group"
	scatterer := NewRegionScatterer(ctx, tc, oc)
	// priority 4 > 1 > 5 > 2 == 3
	for i := 0; i < 1200; i++ {
		scatterer.ordinaryEngine.selectedPeer.Put(2, group)
		scatterer.ordinaryEngine.selectedPeer.Put(3, group)
	}
	for i := 0; i < 800; i++ {
		scatterer.ordinaryEngine.selectedPeer.Put(5, group)
	}
	for i := 0; i < 400; i++ {
		scatterer.ordinaryEngine.selectedPeer.Put(1, group)
	}
	// test region with peer 1 2 3
	for i := uint64(1); i < 20; i++ {
		region := tc.AddLeaderRegion(i+200, i%3+1, (i+1)%3+1, (i+2)%3+1)
		op := scatterer.scatterRegion(region, group)
		re.False(isPeerCountChanged(op))
	}
}

func isPeerCountChanged(op *operator.Operator) bool {
	if op == nil {
		return false
	}
	add, remove := 0, 0
	for i := 0; i < op.Len(); i++ {
		step := op.Step(i)
		switch step.(type) {
		case operator.AddPeer, operator.AddLearner, operator.AddLightPeer, operator.AddLightLearner:
			add++
		case operator.RemovePeer:
			remove++
		}
	}
	return add != remove
}
